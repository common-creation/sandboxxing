package backend

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/common-creation/sandboxxing/internal/state"
)

// Backend runs the containers of one kind. The methods take the recorded
// container rather than its name, because a backend may need more than the
// name to reach it (an address, the identifier of a remote machine, ...).
type Backend interface {
	// Start boots the container. Starting a running container is a no-op.
	Start(ctx context.Context, vm *state.VM) error
	// Stop powers the container off. Stopping a stopped container is a no-op.
	Stop(ctx context.Context, vm *state.VM) error
	// Running reports whether the container is currently running.
	Running(ctx context.Context, vm *state.VM) bool
	// StatusOf summarises the runtime state of the container.
	StatusOf(ctx context.Context, vm *state.VM) Status
	// Collect samples the resource usage of a running container.
	Collect(ctx context.Context, vm *state.VM) (Metrics, error)
	// Run executes a command inside a running container and returns its exit
	// code.
	Run(ctx context.Context, vm *state.VM, e Exec) (int, error)
	// StartProcess starts a command inside a running container. It is used
	// for interactive sessions where the caller owns the standard streams.
	StartProcess(ctx context.Context, vm *state.VM, e Exec) (Process, error)
	// StartTerminal starts a command on a pseudo terminal of the given size
	// inside a running container. The standard streams of e are ignored: the
	// terminal carries them.
	StartTerminal(ctx context.Context, vm *state.VM, e Exec, rows, cols int) (Terminal, error)
	// Dial connects to a TCP port of a running container.
	Dial(ctx context.Context, vm *state.VM, port int) (net.Conn, error)
}

// Process is a command started inside a container.
type Process interface {
	// Wait waits for the command to exit. A command that ran and exited
	// reports its exit status with a nil error; the error is reserved for a
	// command that could not be run or waited for.
	Wait() (int, error)
}

// Terminal is a command running on a pseudo terminal inside a container.
// Reading returns the output of the terminal and writing types into it.
type Terminal interface {
	io.ReadWriter
	// Resize applies a terminal window size.
	Resize(rows, cols int) error
	// Wait waits for the command to exit, like Process.Wait. It also releases
	// the container side of the terminal, so that a reader sees the end of
	// the output once the remaining data has been read.
	Wait() (int, error)
	// Close releases the terminal.
	Close() error
}

// Exec describes one command invocation inside a container.
type Exec struct {
	Argv   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Env is passed to the command. The environment of the daemon is used as
	// the base, so the variables that make a login work (HOME, PATH) are kept
	// and only the listed entries are replaced.
	Env []string
	// Dir is the working directory inside the container. It defaults to /.
	Dir string
	// PTY makes the command the session leader with the given terminal as its
	// controlling terminal, which is what enables job control. It must only
	// be set when Stdin, Stdout and Stderr are the same terminal, usually the
	// slave of the terminal that Nspawn.StartTerminal allocates.
	PTY bool
}

// Status summarises the runtime state of a container.
type Status struct {
	Running bool
	Unit    string
	Since   time.Time
	Leader  int
}

// Metrics samples the resource counters of a running container.
type Metrics struct {
	CPUUsageNS   int64
	MemoryBytes  int64
	MemorySwap   int64
	MemoryLimit  int64
	ProcessCount int64
}
