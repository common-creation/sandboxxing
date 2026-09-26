package process

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCancelKillsProcessGroup verifies that cancelling the context terminates
// not only the direct child but also the grandchildren it spawned. This is
// what makes cancelling `new` stop pacstrap and everything below it.
func TestCancelKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")

	ctx, cancel := context.WithCancel(context.Background())
	// The shell writes the PID of its background child and then waits.
	script := "sleep 300 & echo $! > " + pidFile + "; wait"
	cmd := Command(ctx, "sh", "-c", script)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	var grandchild int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				grandchild = pid
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if grandchild == 0 {
		cmd.Cancel()
		t.Fatal("the helper did not start its child")
	}

	cancel()
	_ = cmd.Wait()

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// Signal 0 only probes for the existence of the process.
		if err := syscall.Kill(grandchild, 0); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the grandchild %d survived the cancellation", grandchild)
}

// TestCommandRuns verifies the happy path.
func TestCommandRuns(t *testing.T) {
	out, err := Output(context.Background(), "sh", "-c", "echo hello")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "hello" {
		t.Errorf("output = %q", out)
	}
}
