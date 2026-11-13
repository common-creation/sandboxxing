package backend

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/common-creation/sandboxxing/internal/agent"
	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
)

func TestSSMBanner(t *testing.T) {
	payload := "HTTP/1.1 101 Switching Protocols\r\n\r\n\x00\xff"
	r := bufio.NewReader(strings.NewReader("\nStarting session with SessionId: user-abcd\n" + payload))
	id, err := readSSMBanner(r)
	if id != "user-abcd" || err != nil {
		t.Fatalf("read banner: %q, %v", id, err)
	}
	data, err := io.ReadAll(r)
	if err != nil || string(data) != payload {
		t.Fatalf("protocol payload %q, %v", data, err)
	}
	for _, bad := range []string{"", "error\n", "\nStarting session with SessionId: \n", strings.Repeat("z", 8192)} {
		if _, err := readSSMBanner(bufio.NewReader(strings.NewReader(bad))); err == nil {
			t.Fatal("bad plugin banner accepted")
		}
	}
}

// The subprocess models the plugin's stdio connection to a running guest.
// This exercises the actual HTTP and WebSocket upgrades without AWS or a
// TCP listener on the host.
func TestSSMGuestProcess(t *testing.T) {
	configPath := os.Getenv("SBX_TEST_AGENT_CONFIG")
	if configPath == "" || os.Getenv("SBX_TEST_PLUGIN_CHILD") == "" {
		return
	}
	h, err := agent.NewServer(slog.Default()).EC2Handler(context.Background(), configPath, configPath+".journal")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, agent.PathPing, nil)
	req.Header.Set(agent.SecretHeader, "shared")
	for {
		result := httptest.NewRecorder()
		h.ServeHTTP(result, req)
		if result.Code == http.StatusOK {
			break
		}
		time.Sleep(time.Millisecond)
	}
	c, err := net.FileConn(os.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(os.Stdout, "\nStarting session with SessionId: fake-session-id\n")
	srv := &http.Server{Handler: h}
	_ = srv.Serve(&pluginListener{Conn: c, accepted: false})
}

type pluginListener struct {
	net.Conn
	accepted bool
}

func (l *pluginListener) Accept() (net.Conn, error) {
	if l.accepted {
		// The plugin is killed when its caller closes the tunnel.
		select {}
	}
	l.accepted = true
	return l.Conn, nil
}
func (l *pluginListener) Addr() net.Addr { return l.LocalAddr() }

func TestEC2SSMGuestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SBX_TEST_EXE", exe)
	t.Setenv("SBX_TEST_AGENT_CONFIG", filepath.Join(dir, "guest.json"))
	t.Setenv("SBX_TEST_TERMINATED", filepath.Join(dir, "terminated"))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	const fakeAWS = `#!/bin/sh
case "$*" in
  "--region us-east-1 ssm start-session --target i-test --document-name AWS-StartSSHSession --parameters portNumber=8080")
    export SBX_TEST_PLUGIN_CHILD=1
    exec "$SBX_TEST_EXE" -test.run '^TestSSMGuestProcess$' ;;
  "--region us-east-1 ssm terminate-session --session-id fake-session-id")
    touch "$SBX_TEST_TERMINATED" ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(fakeAWS), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("SBX_TEST_AGENT_CONFIG"), []byte(`{"secret":"shared","env":["GUEST=yes"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	vm := &state.VM{Name: "ec2test", Backend: state.BackendEC2, EC2: &state.EC2{InstanceID: "i-test", Secret: "shared"}}
	if err := st.Add(vm); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	b := &EC2{transport: newEC2Transport(aws.Config{Region: "us-east-1"}, &config.EC2Config{StartTimeout: config.Duration(10 * time.Second)}, st)}
	if err := b.waitAgent(ctx, vm, nil); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code, err := b.Run(ctx, vm, Exec{
		Argv:  []string{"sh", "-c", `cat; echo "$GUEST" >&2; exit 23`},
		Stdin: strings.NewReader("binary\x00stdin\xff"), Stdout: &out, Stderr: &stderr,
	})
	if err != nil || code != 23 || out.String() != "binary\x00stdin\xff" || stderr.String() != "yes\n" {
		t.Fatalf("code %d, stdout %q, stderr %q, err %v", code, out.String(), stderr.String(), err)
	}
	term, err := b.StartTerminal(ctx, vm, Exec{Argv: []string{"sh", "-c", "stty size"}}, 32, 120)
	if err != nil {
		t.Fatal(err)
	}
	ttyOutput, err := io.ReadAll(term)
	if err != nil {
		t.Fatal(err)
	}
	ttyCode, err := term.Wait()
	if err != nil || ttyCode != 0 || strings.TrimSpace(string(ttyOutput)) != "32 120" {
		t.Fatalf("PTY: exit %d, output %q, err %v", ttyCode, ttyOutput, err)
	}
	sessCtx, endSession := context.WithCancel(ctx)
	proc, err := b.StartProcess(sessCtx, vm, Exec{Argv: []string{"sleep", "120"}})
	endSession()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proc.Wait(); err == nil {
		t.Fatal("cancelled command reported success")
	}
	client, err := b.transport.client(vm.Name)
	if err != nil {
		t.Fatal(err)
	}
	client.Header = func(context.Context) (http.Header, error) { return http.Header{}, nil }
	if err := client.Ping(ctx); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected forbidden: %v", err)
	}
	for {
		if _, err := os.Stat(os.Getenv("SBX_TEST_TERMINATED")); err == nil {
			break
		}
		if err := sleep(ctx, time.Millisecond); err != nil {
			t.Fatal("plugin sessions were not released")
		}
	}
}
