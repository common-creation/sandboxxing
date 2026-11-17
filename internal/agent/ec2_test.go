package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEC2RebootWithRevokedShare(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "agent.json")
	digest := sha256.Sum256([]byte("instance-only"))
	initial := `{"secret_sha256":"` + hex.EncodeToString(digest[:]) + `","fs":"fs-test","shares":[{"ap":"fsap-gone","target":"/shared/old"}]}`
	if err := os.WriteFile(cfg, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	// A corrupt last-known config simulates an unsuccessful restore. The
	// daemon must still be able to verify the reboot and remove the share.
	if err := os.WriteFile(filepath.Join(dir, "shares.json"), []byte("truncated"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(slog.Default())
	srv.StartupLog = filepath.Join(dir, "startup.log")
	srv.StartupMarker = filepath.Join(dir, "startup.done")
	h, err := srv.EC2Handler(t.Context(), cfg, filepath.Join(dir, "mounts"))
	if err != nil {
		t.Fatal(err)
	}
	send := func(method, path, body, secret string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(SecretHeader, secret)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		return res
	}
	if r := send("GET", PathPing, "", "instance-only"); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("guest accepted login before mounts: %d", r.Code)
	}
	for _, wrong := range []string{"wrong", "", hex.EncodeToString(digest[:])} {
		if r := send("GET", PathBootID, "", wrong); r.Code != http.StatusForbidden {
			t.Fatalf("unprotected boot ID for %q: %d", wrong, r.Code)
		}
	}
	if r := send("GET", PathBootID, "", "instance-only"); r.Code != http.StatusOK || len(strings.TrimSpace(r.Body.String())) != 36 {
		t.Fatalf("boot ID: %d %q", r.Code, r.Body.String())
	}
	if r := send("POST", PathShares, "{}", "instance-only"); r.Code != http.StatusOK {
		t.Fatalf("remove shares: %d %q", r.Code, r.Body.String())
	}
	if r := send("GET", PathPing, "", "instance-only"); r.Code != http.StatusOK {
		t.Fatalf("login unavailable after recovery: %d", r.Code)
	}
	if b, err := os.ReadFile(cfg); err != nil || string(b) != initial {
		t.Fatal("immutable agent config was overwritten")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "shares.json")); err != nil || string(b) != "{}" {
		t.Fatalf("expected empty share list, got %q, %v", b, err)
	}
}

// An EC2 instance runs the startup script once per boot after its shares are
// mounted. A restart of the agent in the same boot does not run it again.
func TestEC2StartupScript(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, filepath.Dir(StartupScript)), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$SBX_TEST $HOME\" >> \"$HOME/ran\"\n"
	if err := os.WriteFile(filepath.Join(home, StartupScript), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "agent.json")
	digest := sha256.Sum256([]byte("instance-only"))
	raw := `{"secret_sha256":"` + hex.EncodeToString(digest[:]) + `","home":"` + home + `","env":["SBX_TEST=ec2"]}`
	if err := os.WriteFile(cfg, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "startup.done")
	log := filepath.Join(dir, "startup.log")
	start := func() {
		srv := NewServer(slog.Default())
		srv.StartupLog = log
		srv.StartupMarker = marker
		if _, err := srv.EC2Handler(t.Context(), cfg, filepath.Join(dir, "mounts")); err != nil {
			t.Fatal(err)
		}
	}
	waitRuns := func(want int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			b, _ := os.ReadFile(log)
			if strings.Contains(string(b), "exited with status 0") {
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
		if got := strings.Count(string(b), "ec2 "+home+"\n"); got != want {
			t.Fatalf("the startup script ran %d times, want %d:\n%s", got, want, b)
		}
	}

	start()
	waitRuns(1)
	// The agent restarts in the same boot: the marker survives.
	start()
	time.Sleep(500 * time.Millisecond)
	waitRuns(1)
	// A reboot clears the marker on the tmpfs.
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	start()
	waitRuns(2)
}

// The command doubles record both the mount table and the EFS options.
// Mounts remain present between service restarts and disappear on reboot.
func TestEC2ShareReconciliation(t *testing.T) {
	dir := t.TempDir()
	commands := map[string]string{
		"mount": `test ! -f "$FAIL_MOUNT" || exit 1
printf '%s\n' "$4" > "$6/.mount"`,
		"mountpoint": `test -f "$3/.mount" || exit 32`,
		"umount":     `rm "$1/.mount"`,
	}
	for name, body := range commands {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("FAIL_MOUNT", filepath.Join(dir, "fail"))
	guest := &ec2Mounts{desiredPath: filepath.Join(dir, "desired"), journalPath: filepath.Join(dir, "mounted")}
	target := filepath.Join(dir, "data")
	writable := EC2Shares{FileSystemID: "fs-test", Shares: []ShareMount{{AccessPointID: "fsap-a", Target: target}}}
	readOnly := EC2Shares{FileSystemID: "fs-test", Shares: []ShareMount{{AccessPointID: "fsap-b", Target: target, ReadOnly: true}}}
	if err := guest.apply(t.Context(), writable); err != nil {
		t.Fatal(err)
	}
	if err := guest.apply(t.Context(), readOnly); err != nil {
		t.Fatal(err)
	}
	verify := func() {
		raw, err := os.ReadFile(filepath.Join(target, ".mount"))
		if err != nil || strings.TrimSpace(string(raw)) != "tls,iam,accesspoint=fsap-b,ro" {
			t.Fatalf("unexpected mount %q: %v", raw, err)
		}
	}
	verify()
	if err := os.Remove(filepath.Join(target, ".mount")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(guest.journalPath); err != nil {
		t.Fatal(err)
	}
	rebooted := &ec2Mounts{desiredPath: guest.desiredPath, journalPath: guest.journalPath}
	if err := rebooted.restore(t.Context(), writable); err != nil {
		t.Fatal(err)
	}
	verify()
	if err := rebooted.apply(t.Context(), EC2Shares{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, ".mount")); !os.IsNotExist(err) {
		t.Fatalf("removed share still mounted: %v", err)
	}
	if err := os.WriteFile(os.Getenv("FAIL_MOUNT"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := rebooted.apply(ctx, readOnly); err == nil || rebooted.ready.Load() {
		t.Fatal("failed mounts allowed logins")
	}
	if err := os.Remove(os.Getenv("FAIL_MOUNT")); err != nil {
		t.Fatal(err)
	}
	if err := rebooted.apply(t.Context(), readOnly); err != nil {
		t.Fatal(err)
	}
	verify()
}
