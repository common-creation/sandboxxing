// Package process runs external commands that can be aborted as a whole.
//
// The helpers drive tools such as pacstrap(8), which is a shell script that
// spawns pacman and further children. exec.CommandContext only signals the
// direct child, so cancellation would leave the grandchildren running. The
// commands created here are placed in their own process group and the whole
// group is terminated when the context is cancelled.
package process

import (
	"context"
	"os/exec"
	"strings"
	"syscall"
)

// Command creates an exec.Cmd for name that runs in its own process group and
// whose entire process tree is killed when ctx is cancelled.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// The negative PID addresses the whole process group. A vanished
		// group means the work is already done, which is not an error.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return nil
	}
	return cmd
}

// Output runs the command and returns its combined output.
func Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return Command(ctx, name, args...).CombinedOutput()
}

// MergeEnv returns base with the entries of extra applied on top. A variable
// is replaced instead of duplicated so that the last value is the effective
// one, matching what a shell would do when it sees both.
func MergeEnv(base, extra []string) []string {
	replaced := make(map[string]bool, len(extra))
	for _, kv := range extra {
		if name, _, ok := strings.Cut(kv, "="); ok {
			replaced[name] = true
		}
	}
	merged := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		name, _, ok := strings.Cut(kv, "=")
		if ok && replaced[name] {
			continue
		}
		merged = append(merged, kv)
	}
	return append(merged, extra...)
}
