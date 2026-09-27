package sshserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/common-creation/sandboxxing/internal/cli"
	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
	"github.com/common-creation/sandboxxing/internal/vm"
)

// newTestServer builds a server on an ephemeral port with a temporary state.
func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	return newTestServerWith(t, nil)
}

// newTestServerWith allows a test to adjust the configuration before the
// server is created.
func newTestServerWith(t *testing.T, configure func(*config.Config)) (*Server, string) {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ImageDir = cfg.DataDir + "/images"
	cfg.StateFile = cfg.DataDir + "/state.json"
	cfg.PasswordFile = cfg.DataDir + "/password"
	cfg.Password = "test-password"
	cfg.AuthorizedKeys = cfg.DataDir + "/authorized_keys"
	cfg.Bridge = "sbx-test"
	cfg.Subnet = "10.100.0.0/16"
	cfg.SSHAddr = "127.0.0.1:0"
	if configure != nil {
		configure(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	st.SetSubnet(cfg.Addr())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	vms := vm.New(cfg, log, st)
	runner := cli.New(cfg, log, vms, st, "test-host")
	srv, err := New(cfg, log, vms, st, runner)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", cfg.SSHAddr)
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = ln
	go func() { _ = srv.Serve(context.Background()) }()
	t.Cleanup(func() { ln.Close() })
	return srv, ln.Addr().String()
}

func dial(t *testing.T, addr, user, password string) (*ssh.Client, error) {
	t.Helper()
	return ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
}

type commandResult struct {
	stdout string
	stderr string
	code   int
}

func runCommand(t *testing.T, client *ssh.Client, command string) commandResult {
	t.Helper()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	var stdout, stderr strings.Builder
	session.Stdout = &stdout
	session.Stderr = &stderr
	err = session.Run(command)
	result := commandResult{stdout: stdout.String(), stderr: stderr.String()}
	if err != nil {
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			result.code = exitErr.ExitStatus()
			return result
		}
		t.Fatalf("run %q: %v", command, err)
	}
	return result
}

func TestAuthentication(t *testing.T) {
	_, addr := newTestServer(t)

	if _, err := dial(t, addr, "sandbox", "wrong"); err == nil {
		t.Error("wrong password must be rejected")
	}
	client, err := dial(t, addr, "sandbox", "test-password")
	if err != nil {
		t.Fatalf("valid password rejected: %v", err)
	}
	client.Close()
}

func TestControlCommands(t *testing.T) {
	_, addr := newTestServer(t)
	client, err := dial(t, addr, "sandbox", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if got := runCommand(t, client, "ls"); got.code != 0 || !strings.Contains(got.stdout, "no containers") {
		t.Errorf("ls = %+v", got)
	}
	if got := runCommand(t, client, "help"); got.code != 0 || !strings.Contains(got.stdout, "sandboxxing - manage") {
		t.Errorf("help = %+v", got)
	}
	if got := runCommand(t, client, "billing plan"); got.code != 0 || !strings.Contains(got.stdout, "cpus:") {
		t.Errorf("billing = %+v", got)
	}
	if got := runCommand(t, client, "stat missing"); got.code == 0 {
		t.Errorf("stat of a missing container must fail: %+v", got)
	}
	if got := runCommand(t, client, "resize demo"); got.code == 0 {
		t.Errorf("resize without options must fail: %+v", got)
	}
	if got := runCommand(t, client, "images"); got.code != 0 || !strings.Contains(got.stdout, "no cached images") {
		t.Errorf("images = %+v", got)
	}
}

func TestConfiguredControlUser(t *testing.T) {
	_, addr := newTestServerWith(t, func(cfg *config.Config) {
		cfg.AdminUsers = []string{"boss"}
	})

	// The configured user gets the command interface.
	client, err := dial(t, addr, "boss", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if got := runCommand(t, client, "ls"); got.code != 0 || !strings.Contains(got.stdout, "no containers") {
		t.Errorf("boss ls = %+v", got)
	}

	// A user that is not in the list is treated as a container name.
	client2, err := dial(t, addr, "sandbox", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer client2.Close()
	if got := runCommand(t, client2, "ls"); got.code == 0 {
		t.Errorf("sandbox must not run control commands: %+v", got)
	}
}

func TestContainerNameIsNotAControlUser(t *testing.T) {
	_, addr := newTestServer(t)
	client, err := dial(t, addr, "demo", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	got := runCommand(t, client, "true")
	if got.code == 0 {
		t.Error("connecting to an unknown container must fail")
	}
	if !strings.Contains(got.stdout+got.stderr, "not found") {
		t.Errorf("no hint about the unknown container: %+v", got)
	}
}

// TestLongRunningCommandStreamsToClient verifies that the progress messages
// and the output of the helpers that a command drives are forwarded to the
// SSH client. `new` needs root to build an image, so the run is expected to
// fail; the point is that the client sees the steps and the error, not just
// a bare exit status.
func TestLongRunningCommandStreamsToClient(t *testing.T) {
	_, addr := newTestServer(t)
	client, err := dial(t, addr, "sandbox", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	got := runCommand(t, client, "new --name=streams --cpu=1 --memory=256M --disk=1G")
	if got.code == 0 {
		t.Fatalf("new should not succeed in a temporary data dir: %+v", got)
	}
	combined := got.stdout + got.stderr
	if !strings.Contains(combined, "creating container streams") {
		t.Errorf("the progress message was not forwarded:\n%s", combined)
	}
	if !strings.Contains(combined, "sandboxxing:") {
		t.Errorf("the failure was not reported to the client:\n%s", combined)
	}
}

func TestDomainSuffixedUserNameIsContainer(t *testing.T) {
	_, addr := newTestServer(t)
	// demo.example.com must be recognised as the container "demo" so that
	// `User %n` based ssh_config aliases work. The container does not exist
	// here, so the session must fail with the "not found" hint rather than
	// with a name validation error.
	client, err := dial(t, addr, "demo.example.com", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	got := runCommand(t, client, "true")
	if got.code != 127 {
		t.Errorf("exit code = %d, want 127 (%+v)", got.code, got)
	}
	if !strings.Contains(got.stdout+got.stderr, "container demo not found") {
		t.Errorf("the domain suffix was not stripped: %+v", got)
	}
}

func TestExecFormAndArguments(t *testing.T) {
	_, addr := newTestServer(t)
	client, err := dial(t, addr, "sandbox", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if got := runCommand(t, client, `ls --group=tag`); got.code != 0 {
		t.Errorf("quoted argument failed: %+v", got)
	}
	if got := runCommand(t, client, `ls "--group no"`); got.code == 0 {
		t.Errorf("invalid group must fail: %+v", got)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestPasswordAuthDisabled verifies that "password": null turns the method
// off: the password is not offered and not accepted, and the daemon does not
// fall back to the generated password.
func TestPasswordAuthDisabled(t *testing.T) {
	srv, addr := newTestServerWith(t, func(cfg *config.Config) {
		cfg.PasswordAuthDisabled = true
		cfg.Password = ""
	})
	if !srv.PasswordAuthDisabled() {
		t.Fatal("the server should report password authentication as disabled")
	}
	if srv.Password() != "" {
		t.Errorf("no password should be kept, got %q", srv.Password())
	}

	// A connection that only offers a password must be rejected.
	if _, err := dial(t, addr, "sandbox", "test-password"); err == nil {
		t.Error("password authentication must be rejected when disabled")
	}

	// The advertised methods must not include password.
	methods := advertisedMethods(t, addr, "sandbox")
	joined := strings.Join(methods, "; ")
	if strings.Contains(joined, "password") {
		t.Errorf("password must not be attempted when disabled, got %v", methods)
	}
}

// TestPublicKeyAuthentication verifies that a key listed in authorized_keys
// can connect, and that another key cannot.
func TestPublicKeyAuthentication(t *testing.T) {
	dir := t.TempDir()
	allowed := filepath.Join(dir, "id_ed25519")
	intruder := filepath.Join(dir, "id_ed25519_other")
	if err := writeTestKey(allowed); err != nil {
		t.Fatal(err)
	}
	if err := writeTestKey(intruder); err != nil {
		t.Fatal(err)
	}
	allowedSigner := loadTestKey(t, allowed)

	keyPath := filepath.Join(dir, "authorized_keys")
	pub, err := os.ReadFile(allowed + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pub, 0o600); err != nil {
		t.Fatal(err)
	}

	srv, addr := newTestServerWith(t, func(cfg *config.Config) {
		cfg.AuthorizedKeys = keyPath
		cfg.AuthorizedKeysExplicit = true
	})
	if !srv.PublicKeyAuthEnabled() {
		t.Fatal("public key authentication should be enabled")
	}

	client, err := dialWithKey(t, addr, "sandbox", allowedSigner)
	if err != nil {
		t.Fatalf("the authorized key was rejected: %v", err)
	}
	if got := runCommand(t, client, "ls"); got.code != 0 {
		t.Errorf("control command with a key failed: %+v", got)
	}
	client.Close()

	intruderSigner := loadTestKey(t, intruder)
	if _, err := dialWithKey(t, addr, "sandbox", intruderSigner); err == nil {
		t.Error("an unauthorized key must be rejected")
	}
}

// TestAuthorizedKeysDisabled verifies that "authorized_keys": null turns
// public key authentication off even when a file exists at the default path.
func TestAuthorizedKeysDisabled(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := writeTestKey(keyPath); err != nil {
		t.Fatal(err)
	}
	signer := loadTestKey(t, keyPath)

	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	defaultPath := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(defaultPath, pub, 0o600); err != nil {
		t.Fatal(err)
	}

	srv, addr := newTestServerWith(t, func(cfg *config.Config) {
		cfg.AuthorizedKeysDisabled = true
		cfg.AuthorizedKeys = ""
		cfg.AuthorizedKeysExplicit = false
	})
	if srv.PublicKeyAuthEnabled() {
		t.Fatal("public key authentication should be disabled")
	}
	if _, err := dialWithKey(t, addr, "sandbox", signer); err == nil {
		t.Error("a key must be rejected when public key authentication is disabled")
	}
}

// TestMissingExplicitAuthorizedKeysFails makes a configuration typo loud
// instead of silently locking everyone out.
func TestMissingExplicitAuthorizedKeysFails(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Password = "test-password"
	cfg.AuthorizedKeys = filepath.Join(t.TempDir(), "does-not-exist")
	cfg.AuthorizedKeysExplicit = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := state.Open(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	st.SetSubnet(cfg.Addr())
	vms := vm.New(cfg, log, st)
	runner := cli.New(cfg, log, vms, st, "test-host")
	if _, err := New(cfg, log, vms, st, runner); err == nil {
		t.Error("a missing authorized_keys file named in the configuration must fail")
	}
}

// advertisedMethods performs a handshake that is bound to fail and reports
// the authentication methods the server offered. A method that the server did
// not enable does not appear in the error list, which is how the tests check
// that "password": null really withdraws the method.
func advertisedMethods(t *testing.T, addr, user string) []string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _, _, err = ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password("definitely-wrong")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err == nil {
		t.Fatal("the handshake unexpectedly succeeded")
	}
	var authErr *ssh.ServerAuthError
	if !errors.As(err, &authErr) {
		// The client reports "no supported methods remain" when the server
		// offers nothing it can use; that is a valid outcome here.
		return nil
	}
	var methods []string
	for _, e := range authErr.Errors {
		methods = append(methods, e.Error())
	}
	return methods
}

// writeTestKey creates an ed25519 key pair at path.
func writeTestKey(path string) error {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	pemBlock, err := ssh.MarshalPrivateKey(priv, "test key")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(pemBlock), 0o600); err != nil {
		return err
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		return err
	}
	return os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(pub), 0o600)
}

// loadTestKey reads the private key at path.
func loadTestKey(t *testing.T, path string) ssh.Signer {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// dialWithKey connects using public key authentication only.
func dialWithKey(t *testing.T, addr, user string, signer ssh.Signer) (*ssh.Client, error) {
	t.Helper()
	return ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
}
