// Package tty allocates a pseudo terminal from the devpts instance of a
// container.
//
// A terminal that is opened on the host does not exist inside the container's
// private devpts instance, so processes that resolve their terminal with
// ttyname(3) — such as the shell's `tty` command — fail with
// "ttyname error: No such device". Allocating the pseudo terminal through the
// container's own /dev/ptmx makes the pts node appear in the container's
// /dev/pts, where every process can resolve it. This is what runc and Docker
// do as well.
package tty

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Terminal is a pseudo terminal pair whose slave lives inside a container.
type Terminal struct {
	// Master is the host side of the terminal. Its size can be changed while
	// the session runs.
	Master *os.File
	// Slave is the container side. It is passed to the process that runs
	// inside the container and closed once it has been started.
	Slave *os.File
	// Number is the pts number inside the container, for diagnostics.
	Number int
}

// Open allocates a pseudo terminal from the devpts instance that the given
// container PID uses. The master and the slave are both host side file
// descriptors; the slave is addressable inside the container as
// /dev/pts/<Number>.
func Open(pid int) (*Terminal, error) {
	ptmxPath := fmt.Sprintf("/proc/%d/root/dev/ptmx", pid)
	master, err := os.OpenFile(ptmxPath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("open the container terminal multiplexer %s: %w", ptmxPath, err)
	}
	// Grantpt and unlockpt are already done by the kernel for the /dev/ptmx
	// clone device, but the lock must be cleared before the slave can be
	// opened. This mirrors grantpt(3) and unlockpt(3).
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, fmt.Errorf("unlock the container terminal: %w", err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("resolve the container terminal name: %w", err)
	}

	slavePath := fmt.Sprintf("/proc/%d/root/dev/pts/%d", pid, number)
	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("open the container terminal %s: %w", slavePath, err)
	}
	return &Terminal{Master: master, Slave: slave, Number: number}, nil
}

// SetSize applies a terminal window size.
func (t *Terminal) SetSize(rows, cols int) error {
	if t.Master == nil || rows <= 0 || cols <= 0 {
		return nil
	}
	return unix.IoctlSetWinsize(int(t.Master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: uint16(rows),
		Col: uint16(cols),
	})
}

// Close releases both ends of the terminal. The slave is usually closed right
// after the child process has been started, which is when a reader on the
// master starts to see the end of file.
func (t *Terminal) Close() error {
	var errs []error
	if t.Slave != nil {
		errs = append(errs, t.Slave.Close())
		t.Slave = nil
	}
	if t.Master != nil {
		errs = append(errs, t.Master.Close())
		t.Master = nil
	}
	return errors.Join(errs...)
}

// CloseSlave closes only the container side. The master stays open, which is
// what the session needs after the payload was started.
func (t *Terminal) CloseSlave() error {
	if t.Slave == nil {
		return nil
	}
	err := t.Slave.Close()
	t.Slave = nil
	return err
}
