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
