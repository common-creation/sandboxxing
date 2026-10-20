package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHome records the home directory operations instead of mounting.
type fakeHome struct {
	mu       sync.Mutex
	mounts   int
	unmounts int
}

func (f *fakeHome) Mount(ctx context.Context, cfg *RunConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounts++
	return nil
}

func (f *fakeHome) Check(ctx context.Context, cfg *RunConfig) error { return nil }

func (f *fakeHome) Unmount(ctx context.Context, cfg *RunConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unmounts++
	return nil
}

const testSecret = "s3cret"

// newAgent starts an agent whose run hook was called, and returns a client
// for its data port together with the hook server.
func newAgent(t *testing.T) (*Client, *httptest.Server, *fakeHome) {
	client, hooks, home, _ := newAgentServer(t)
	return client, hooks, home
}

func newAgentServer(t *testing.T) (*Client, *httptest.Server, *fakeHome, *Server) {
	t.Helper()
	srv := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	home := &fakeHome{}
	srv.Home = home
	hooks := httptest.NewServer(srv.HookHandler())
	t.Cleanup(hooks.Close)
	data := httptest.NewServer(srv.DataHandler())
	t.Cleanup(data.Close)

	if code := runHook(t, hooks.URL, RunConfig{Secret: testSecret, FileSystemID: "fs-1", AccessPointID: "fsap-1",
		Env: []string{"SBX_TEST=instance"}}); code != http.StatusOK {
		t.Fatalf("run hook answered %d", code)
	}
	client := &Client{
		BaseURL: data.URL,
		Header: func(ctx context.Context) (http.Header, error) {
			return http.Header{SecretHeader: {testSecret}}, nil
		},
	}
	return client, hooks, home, srv
}

func runHook(t *testing.T, base string, cfg RunConfig) int {
	t.Helper()
	payload, _ := json.Marshal(cfg)
	body, _ := json.Marshal(RunRequest{MicrovmID: "mvm-1", RunHookPayload: string(payload)})
	resp, err := http.Post(base+HookPath+"run", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestExecPipes(t *testing.T) {
	client, _, home := newAgent(t)
	if home.mounts != 1 {
		t.Errorf("the run hook mounted the home directory %d times, want 1", home.mounts)
	}
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	sess, err := client.Start(ctx, ExecRequest{
		Argv: []string{"sh", "-c", `read line; echo "out:$line:$SBX_TEST:$SBX_REQ"; echo err >&2; exit 3`},
		Env:  []string{"SBX_REQ=request"},
	}, strings.NewReader("hello\n"), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	code, err := sess.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	if got := stdout.String(); got != "out:hello:instance:request\n" {
		t.Errorf("stdout = %q", got)
	}
	if got := stderr.String(); got != "err\n" {
		t.Errorf("stderr = %q", got)
	}
}

// TestExecLargeOutput sends more than one message worth of data each way.
func TestExecLargeOutput(t *testing.T) {
	client, _, _ := newAgent(t)
	input := bytes.Repeat([]byte("0123456789abcdef"), 3*MaxChunk/16+7)
	var stdout bytes.Buffer
	sess, err := client.Start(context.Background(), ExecRequest{Argv: []string{"cat"}},
		bytes.NewReader(input), &stdout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := sess.Wait(); err != nil || code != 0 {
		t.Fatalf("cat exited with %d, %v", code, err)
	}
	if !bytes.Equal(stdout.Bytes(), input) {
		t.Errorf("cat returned %d bytes, want %d", stdout.Len(), len(input))
	}
}

func TestExecTerminal(t *testing.T) {
	client, _, _ := newAgent(t)
	sess, err := client.Start(context.Background(), ExecRequest{
		Argv: []string{"sh", "-c", "tty; stty size"},
		TTY:  true, Rows: 33, Cols: 101,
	}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(sess)
	if code, err := sess.Wait(); err != nil || code != 0 {
		t.Fatalf("exited with %d, %v: %s", code, err, out)
	}
	if !strings.Contains(string(out), "/dev/pts/") {
		t.Errorf("the command has no terminal: %q", out)
	}
	if !strings.Contains(string(out), "33 101") {
		t.Errorf("the terminal size was not applied: %q", out)
	}
}

func TestExecMissingCommand(t *testing.T) {
	client, _, _ := newAgent(t)
	sess, err := client.Start(context.Background(), ExecRequest{Argv: []string{"/nonexistent/command"}}, nil, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Wait(); err == nil {
		t.Error("a command that cannot start did not fail")
	}
}

func TestDial(t *testing.T) {
	client, _, _ := newAgent(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()
	conn, err := client.Dial(context.Background(), ln.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Errorf("echo returned %q", buf)
	}
}

func TestDataPortRequiresSecret(t *testing.T) {
	client, _, _ := newAgent(t)
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("ping with the secret failed: %v", err)
	}
	client.Header = func(ctx context.Context) (http.Header, error) {
		return http.Header{SecretHeader: {"wrong"}}, nil
	}
	if err := client.Ping(context.Background()); err == nil {
		t.Error("a wrong secret was accepted")
	}
	if _, err := client.Start(context.Background(), ExecRequest{Argv: []string{"true"}}, nil, nil, nil); err == nil {
		t.Error("exec with a wrong secret was accepted")
	}
}

// TestRunHookOnce makes sure that a second run hook, which can only come from
// inside the VM, cannot reconfigure the instance.
func TestRunHookOnce(t *testing.T) {
	_, hooks, home := newAgent(t)
	if code := runHook(t, hooks.URL, RunConfig{Secret: "other", FileSystemID: "fs-1", AccessPointID: "fsap-other"}); code != http.StatusConflict {
		t.Errorf("a second run hook answered %d, want 409", code)
	}
	if home.mounts != 1 {
		t.Errorf("the second run hook mounted again")
	}
}

func TestTerminateNotifiesAndUnmounts(t *testing.T) {
	client, hooks, home, srv := newAgentServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events := make(chan Control, 1)
	go func() {
		_ = client.Events(ctx, func(ev Control) { events <- ev })
	}()
	// The subscription is registered after the WebSocket handshake.
	for {
		srv.mu.Lock()
		n := len(srv.subscribers)
		srv.mu.Unlock()
		if n > 0 {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("the events connection was not registered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := http.Post(hooks.URL+HookPath+"terminate", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case ev := <-events:
		if ev.Type != ControlTerminating || ev.Reason != ReasonTerminated {
			t.Errorf("event = %+v", ev)
		}
	case <-ctx.Done():
		t.Fatal("no terminating event")
	}
	if home.unmounts != 1 {
		t.Errorf("the terminate hook unmounted %d times, want 1", home.unmounts)
	}
}

// TestKeepaliveDuringIdleCommand pings a session that prints nothing for a
// while, which is what keeps the MicroVM endpoint from closing it.
func TestKeepaliveDuringIdleCommand(t *testing.T) {
	old := KeepaliveInterval
	KeepaliveInterval = 20 * time.Millisecond
	defer func() { KeepaliveInterval = old }()
	client, _, _ := newAgent(t)
	var stdout bytes.Buffer
	sess, err := client.Start(context.Background(), ExecRequest{Argv: []string{"sh", "-c", "sleep 0.3; echo done"}}, nil, &stdout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := sess.Wait(); err != nil || code != 0 {
		t.Fatalf("exited with %d, %v", code, err)
	}
	if stdout.String() != "done\n" {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// TestStartupScript runs the startup script of the home directory in the
// background once the run hook configured the VM.
func TestStartupScript(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, filepath.Dir(StartupScript)), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nsleep 1\necho \"$SBX_TEST $HOME\" > \"$HOME/ran\"\necho done\n"
	if err := os.WriteFile(filepath.Join(home, StartupScript), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.Home = &fakeHome{}
	srv.StartupLog = filepath.Join(t.TempDir(), "startup.log")
	hooks := httptest.NewServer(srv.HookHandler())
	t.Cleanup(hooks.Close)

	began := time.Now()
	if code := runHook(t, hooks.URL, RunConfig{Secret: testSecret, FileSystemID: "fs-1", AccessPointID: "fsap-1",
		Home: home, Env: []string{"SBX_TEST=instance"}}); code != http.StatusOK {
		t.Fatalf("run hook answered %d", code)
	}
	if took := time.Since(began); took > 800*time.Millisecond {
		t.Errorf("the run hook waited %s for the startup script", took)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(srv.StartupLog)
		if strings.Contains(string(b), "exited with status 0") {
			if !strings.Contains(string(b), "done") {
				t.Errorf("the log lacks the output of the script:\n%s", b)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the startup script did not finish:\n%s", b)
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, err := os.ReadFile(filepath.Join(home, "ran"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != "instance "+home {
		t.Errorf("the script saw %q, want the instance environment and HOME", got)
	}
}
