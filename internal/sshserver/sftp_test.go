package sshserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
)

// openSubsystemChannel opens a raw session channel and requests a subsystem.
// It returns the channel, the incoming request stream (which carries
// exit-status), and whether the server accepted the request.
func openSubsystemChannel(t *testing.T, client *ssh.Client, subsystem string) (ssh.Channel, <-chan *ssh.Request, bool) {
	t.Helper()
	ch, reqs, err := client.OpenChannel("session", nil)
	if err != nil {
		t.Fatalf("open session channel: %v", err)
	}
	ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{subsystem}))
	if err != nil {
		_ = ch.Close()
		t.Fatalf("subsystem %q request: %v", subsystem, err)
	}
	return ch, reqs, ok
}

// waitSubsystemExit collects the stderr stream and the exit-status request of
// a subsystem channel that the server is about to close.
func waitSubsystemExit(t *testing.T, ch ssh.Channel, reqs <-chan *ssh.Request) (int, string) {
	t.Helper()
	type exitResult struct {
		code int
		err  error
	}
	exitCh := make(chan exitResult, 1)
	go func() {
		timeout := time.After(5 * time.Second)
		for {
			select {
			case req, ok := <-reqs:
				if !ok {
					exitCh <- exitResult{err: io.EOF}
					return
				}
				if req.Type == "exit-status" {
					var payload struct{ Status uint32 }
					if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
						exitCh <- exitResult{err: err}
						return
					}
					exitCh <- exitResult{code: int(payload.Status)}
					return
				}
			case <-timeout:
				exitCh <- exitResult{err: context.DeadlineExceeded}
				return
			}
		}
	}()
	type stderrResult struct {
		text string
		err  error
	}
	stderrCh := make(chan stderrResult, 1)
	go func() {
		b, err := io.ReadAll(ch.Stderr())
		stderrCh <- stderrResult{text: string(b), err: err}
	}()
	var code int
	select {
	case res := <-exitCh:
		if res.err != nil {
			t.Fatalf("wait for exit-status: %v", res.err)
		}
		code = res.code
	case <-time.After(6 * time.Second):
		t.Fatal("timed out waiting for exit-status")
	}
	// The server closes the channel after the status, which ends the stderr
	// stream. Give it a moment, then stop waiting.
	select {
	case res := <-stderrCh:
		// ReadAll returns nil on clean EOF; any other error still leaves the
		// bytes that arrived, which is what the assertions check.
		return code, res.text
	case <-time.After(6 * time.Second):
		t.Fatal("timed out waiting for stderr")
		return code, ""
	}
}

// TestUnknownSubsystemIsRejected guards that only sftp is accepted: any other
// subsystem must keep the historical behaviour (reply false).
func TestUnknownSubsystemIsRejected(t *testing.T) {
	_, addr := newTestServer(t)
	client, err := dial(t, addr, "sandbox", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.RequestSubsystem("bogus-subsystem"); err == nil {
		t.Error("an unknown subsystem must be rejected")
	}
}

// TestSFTPSubsystemIsAccepted verifies that the server advertises sftp:
// modern scp/sftp clients fail fast when the reply is false.
func TestSFTPSubsystemIsAccepted(t *testing.T) {
	_, addr := newTestServer(t)
	client, err := dial(t, addr, "sandbox", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// The control user rejects the transfer after accepting the subsystem
	// request, so the request itself must succeed here.
	if err := sess.RequestSubsystem("sftp"); err != nil {
		t.Fatalf("sftp subsystem must be accepted: %v", err)
	}
	// Drain the rejection so the session does not leak.
	_ = sess.Wait()
}

// TestSFTPControlUserRejected makes sure a control user cannot use the
// subsystem to read the host file system: there is no container behind the
// name, so the only safe answer is an error on stderr and a non-zero status.
func TestSFTPControlUserRejected(t *testing.T) {
	_, addr := newTestServer(t)
	client, err := dial(t, addr, "sandbox", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ch, reqs, ok := openSubsystemChannel(t, client, "sftp")
	if !ok {
		_ = ch.Close()
		t.Fatal("sftp subsystem must be accepted before the rejection")
	}
	defer ch.Close()
	code, stderr := waitSubsystemExit(t, ch, reqs)
	if code == 0 {
		t.Error("sftp for a control user must fail")
	}
	if !strings.Contains(stderr, "control users") {
		t.Errorf("stderr should explain the rejection, got %q", stderr)
	}
}

// TestSFTPUnknownContainerRejected covers the domain-stripped name lookup:
// an unknown container must fail with the familiar "not found" hint on
// stderr (not on the binary SFTP stream) and exit 127 like exec sessions.
func TestSFTPUnknownContainerRejected(t *testing.T) {
	_, addr := newTestServer(t)
	client, err := dial(t, addr, "demo", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ch, reqs, ok := openSubsystemChannel(t, client, "sftp")
	if !ok {
		_ = ch.Close()
		t.Fatal("sftp subsystem must be accepted before the rejection")
	}
	defer ch.Close()
	code, stderr := waitSubsystemExit(t, ch, reqs)
	if code != 127 {
		t.Errorf("exit code = %d, want 127", code)
	}
	if !strings.Contains(stderr, "container demo not found") {
		t.Errorf("stderr should name the container, got %q", stderr)
	}
}

// TestSFTPBridgesToSFTPServer drives a minimal SFTP handshake through the SSH
// subsystem: INIT (version 3) must yield a VERSION reply. The container
// execution is replaced with the host sftp-server so the test needs no
// systemd-nspawn, while the SSH dispatch, channel bridging and exit handling
// are the production code.
func TestSFTPBridgesToSFTPServer(t *testing.T) {
	srv, addr := newTestServer(t)
	if err := srv.state.Add(&state.VM{
		Name:   "demo",
		Image:  "arch",
		CPU:    1,
		Memory: 1 << 30,
		Disk:   1 << 30,
	}); err != nil {
		t.Fatal(err)
	}
	sftpServer := hostSFTPServer(t)
	serverDone := make(chan int, 1)
	srv.sftpRun = func(ctx context.Context, name, dir string, env []string, ch ssh.Channel) (int, error) {
		if name != "demo" {
			t.Errorf("sftpRun container = %q, want demo", name)
		}
		cmd := exec.CommandContext(ctx, sftpServer)
		cmd.Stdin = ch
		cmd.Stdout = ch
		cmd.Stderr = ch.Stderr()
		runErr := cmd.Run()
		code := 0
		if runErr != nil {
			if exitErr, ok := runErr.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else {
				serverDone <- 1
				return 1, runErr
			}
		}
		select {
		case serverDone <- code:
		default:
		}
		if runErr != nil {
			if exitErr, ok := runErr.(*exec.ExitError); ok {
				return exitErr.ExitCode(), nil
			}
			return 1, runErr
		}
		return 0, nil
	}

	client, err := dial(t, addr, "demo", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestSubsystem("sftp"); err != nil {
		t.Fatalf("sftp subsystem must be accepted: %v", err)
	}

	// SSH_FXP_INIT (type 1) with version 3.
	var init bytes.Buffer
	_ = binary.Write(&init, binary.BigEndian, uint32(5))
	init.WriteByte(1)
	_ = binary.Write(&init, binary.BigEndian, uint32(3))
	if _, err := stdin.Write(init.Bytes()); err != nil {
		t.Fatalf("write INIT: %v", err)
	}

	version, extensions := readSFTPVersion(t, stdout)
	if version < 3 {
		t.Errorf("sftp version = %d, want >= 3", version)
	}
	t.Logf("sftp version %d with %d extension bytes", version, len(extensions))

	// Closing the session ends the transfer; the server must not hang.
	_ = stdin.Close()
	sess.Close()
	select {
	case <-serverDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the sftp server did not finish after close")
	}
}

// readSFTPVersion reads one SFTP packet and verifies it is VERSION (type 2).
func readSFTPVersion(t *testing.T, r io.Reader) (uint32, []byte) {
	t.Helper()
	type result struct {
		version uint32
		rest    []byte
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		var length uint32
		if err := binary.Read(r, binary.BigEndian, &length); err != nil {
			ch <- result{err: err}
			return
		}
		if length > 1<<20 {
			ch <- result{err: io.ErrUnexpectedEOF}
			return
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			ch <- result{err: err}
			return
		}
		if len(payload) < 5 || payload[0] != 2 {
			ch <- result{err: io.ErrUnexpectedEOF}
			return
		}
		ch <- result{version: binary.BigEndian.Uint32(payload[1:5]), rest: payload[5:]}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("read VERSION: %v", res.err)
		}
		return res.version, res.rest
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the sftp VERSION reply")
		return 0, nil
	}
}

// hostSFTPServer locates the sftp-server binary used by the bridge test.
func hostSFTPServer(t *testing.T) string {
	t.Helper()
	for _, path := range sftpServerCandidates {
		var err error
		if _, err = exec.LookPath(path); err == nil {
			return path
		}
		// LookPath fails for absolute paths without a PATH entry; check the
		// file directly as well.
		if _, statErr := exec.Command("test", "-x", path).Output(); statErr == nil {
			return path
		}
	}
	// Fall back to PATH lookup, which covers distributions that ship it as
	// "sftp-server".
	if path, err := exec.LookPath("sftp-server"); err == nil {
		return path
	}
	t.Skip("no sftp-server binary found on the host")
	return ""
}

// TestSCPFileTransferViaSFTP is the end-to-end proof for the requested
// feature: the OpenSSH `scp` client (SFTP mode, the default since 9.0)
// uploads and downloads a file through the subsystem. Public key
// authentication avoids password prompts, and the container execution is
// replaced with the host sftp-server so no nspawn is needed.
func TestSCPFileTransferViaSFTP(t *testing.T) {
	if _, err := exec.LookPath("scp"); err != nil {
		t.Skip("no scp binary found on the host")
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := writeTestKey(keyPath); err != nil {
		t.Fatal(err)
	}
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	keysPath := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(keysPath, pub, 0o600); err != nil {
		t.Fatal(err)
	}

	srv, addr := newTestServerWith(t, func(cfg *config.Config) {
		cfg.AuthorizedKeys = keysPath
		cfg.AuthorizedKeysExplicit = true
	})
	if err := srv.state.Add(&state.VM{
		Name:   "demo",
		Image:  "arch",
		CPU:    1,
		Memory: 1 << 30,
		Disk:   1 << 30,
	}); err != nil {
		t.Fatal(err)
	}
	sftpServer := hostSFTPServer(t)
	srv.sftpRun = func(ctx context.Context, name, _ string, _ []string, ch ssh.Channel) (int, error) {
		cmd := exec.CommandContext(ctx, sftpServer)
		cmd.Stdin = ch
		cmd.Stdout = ch
		cmd.Stderr = ch.Stderr()
		if err := cmd.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				return exitErr.ExitCode(), nil
			}
			return 1, err
		}
		return 0, nil
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}

	// A file to upload, and a remote directory served by the host sftp-server.
	localSrc := filepath.Join(dir, "upload.txt")
	content := "hello sandboxxing sftp via scp\n"
	if err := os.WriteFile(localSrc, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	remoteDir := filepath.Join(dir, "remote")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	remoteFile := filepath.Join(remoteDir, "remote.txt")

	sshOpts := []string{
		"-i", keyPath,
		"-P", port,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
		"-o", "LogLevel=ERROR",
	}
	runSCP := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "scp", args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("scp %v: %v\n%s", args, err, out)
		}
	}

	// Upload through the SFTP subsystem (scp defaults to SFTP since 9.0).
	runSCP(append(sshOpts, localSrc, "demo@"+host+":"+remoteFile)...)

	got, err := os.ReadFile(remoteFile)
	if err != nil {
		t.Fatalf("uploaded file not found: %v", err)
	}
	if string(got) != content {
		t.Errorf("uploaded content = %q, want %q", got, content)
	}

	// Download it back under another name.
	localDst := filepath.Join(dir, "download.txt")
	runSCP(append(sshOpts, "demo@"+host+":"+remoteFile, localDst)...)
	got, err = os.ReadFile(localDst)
	if err != nil {
		t.Fatalf("downloaded file not found: %v", err)
	}
	if string(got) != content {
		t.Errorf("downloaded content = %q, want %q", got, content)
	}
}
