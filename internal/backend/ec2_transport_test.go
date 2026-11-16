package backend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/common-creation/sandboxxing/internal/agent"
	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
)

// The guest is started from the configuration the daemon puts in user data,
// so the digest sent to EC2 must admit the secret the daemon keeps.
func TestEC2GuestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	vm := &state.VM{Name: "ec2test", Backend: state.BackendEC2, Env: map[string]string{"GUEST": "yes"},
		EC2: &state.EC2{InstanceID: "i-test", Secret: "shared"}}
	if err := st.Add(vm); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	b := &EC2{transport: newEC2Transport(aws.Config{Region: "us-east-1"}, &config.EC2Config{StartTimeout: config.Duration(10 * time.Second)}, st)}
	if _, err := b.transport.client(vm.Name); err == nil || !strings.Contains(err.Error(), "recreated") {
		t.Fatalf("sandbox without a private address: %v", err)
	}
	if err := st.UpdateEC2(vm.Name, func(v *state.VM) { v.EC2.PrivateIP = "10.0.0.5" }); err != nil {
		t.Fatal(err)
	}

	script, err := b.userData(ctx, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded := regexp.MustCompile(`echo '([^']+)' \| base64 -d > /etc/sandboxxing/agent\.json`).FindSubmatch(script)
	if encoded == nil {
		t.Fatal("user data does not write the agent configuration")
	}
	guest, err := base64.StdEncoding.DecodeString(string(encoded[1]))
	if err != nil {
		t.Fatal(err)
	}
	var sent agent.RunConfig
	digest := sha256.Sum256([]byte("shared"))
	if err := json.Unmarshal(guest, &sent); err != nil || sent.Secret != "" || sent.SecretSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("user data must carry only the digest of the secret: %s, %v", guest, err)
	}
	configPath := filepath.Join(dir, "guest.json")
	if err := os.WriteFile(configPath, guest, 0600); err != nil {
		t.Fatal(err)
	}
	h, err := agent.NewServer(slog.Default()).EC2Handler(ctx, configPath, configPath+".journal")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(listener) }()
	defer srv.Close()
	b.transport.dial = func(ctx context.Context, addr string) (net.Conn, error) {
		if addr != "10.0.0.5:8080" {
			t.Errorf("dialled %q", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}

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
	// The digest is public: presenting it instead of the secret must fail.
	for _, wrong := range []string{"", sent.SecretSHA256} {
		client.Header = func(context.Context) (http.Header, error) {
			return http.Header{agent.SecretHeader: {wrong}}, nil
		}
		if err := client.Ping(ctx); err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatalf("expected forbidden for %q: %v", wrong, err)
		}
	}
}
