// Package backend runs sandbox containers with systemd-nspawn(1).
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/process"
	"github.com/common-creation/sandboxxing/internal/progress"
	"github.com/common-creation/sandboxxing/internal/state"
)

// Machine is the unit name prefix used by systemd-run.
const unitPrefix = "sandboxxing-"

// Nspawn runs containers as transient systemd services.
type Nspawn struct {
	cfg *config.Config
	log *slog.Logger
}

// New creates a backend for the configuration.
func New(cfg *config.Config, log *slog.Logger) *Nspawn {
	return &Nspawn{cfg: cfg, log: log}
}

// Unit returns the systemd unit name of a container.
func Unit(name string) string { return unitPrefix + name }

// Running reports whether the container is currently running.
func (n *Nspawn) Running(ctx context.Context, name string) bool {
	out, err := n.systemctl(ctx, "is-active", Unit(name))
	return err == nil && strings.TrimSpace(out) == "active"
}

// Leader returns the PID 1 of the container, as seen from the host.
func (n *Nspawn) Leader(ctx context.Context, name string) (int, error) {
	out, err := exec.CommandContext(ctx, "machinectl", "show", name, "-p", "Leader", "--value").Output()
	if err != nil {
		return 0, fmt.Errorf("container %s is not running: %w", name, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("container %s has no leader process", name)
	}
	return pid, nil
}

// Start boots the container as a transient service. The systemd unit applies
// the configured CPU and memory limits, so the container cannot use more than
// its share even if a process inside misbehaves.
func (n *Nspawn) Start(ctx context.Context, vm *state.VM, imagePath string) error {
	if n.Running(ctx, vm.Name) {
		return nil
	}
	// A unit left behind by an earlier failure must not block the restart.
	_, _ = n.systemctl(ctx, "reset-failed", Unit(vm.Name))

	args := []string{
		"--unit=" + Unit(vm.Name),
		"--description=sandboxxing container " + vm.Name,
		"--collect",
		// The paths are passed verbatim: no $ expansion by systemd.
		"--expand-environment=no",
		"-p", "MemoryMax=" + strconv.FormatInt(vm.Memory, 10),
		"-p", "TasksMax=16384",
		// SIGTERM goes to systemd-nspawn only. nspawn forwards SIGRTMIN+3 to
		// the container init, which powers the container off in an orderly
		// fashion before systemd kills the remaining processes.
		"-p", "KillMode=mixed",
		"-p", "TimeoutStopSec=60",
	}
	if vm.CPU > 0 {
		// CPUQuota is expressed in percent of a single CPU.
		args = append(args, "-p", fmt.Sprintf("CPUQuota=%d%%", vm.CPU*100))
	}
	args = append(args,
		"systemd-nspawn",
		"--quiet",
		"--keep-unit",
		"--boot",
		"--machine="+vm.Name,
		"--uuid="+vm.MachineID,
		"--image="+imagePath,
		"--network-veth",
		"--network-bridge="+n.cfg.Bridge,
		"--resolv-conf=off",
		"--timezone=off",
		"--link-journal=no",
		"--console=passive",
	)

	progress.From(ctx).Step("starting container %s", vm.Name)
	cmd := process.Command(ctx, "systemd-run", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("start container %s: %w: %s", vm.Name, err, strings.TrimSpace(string(out)))
	}
	if err := n.waitBoot(ctx, vm.Name); err != nil {
		return err
	}
	progress.From(ctx).Step("container %s is up", vm.Name)
	return nil
}

// waitBoot waits until the container reports that it has finished booting.
func (n *Nspawn) waitBoot(ctx context.Context, name string) error {
	deadline := time.Now().Add(n.cfg.BootTimeout.Duration())
	for time.Now().Before(deadline) {
		if !n.Running(ctx, name) {
			out, _ := n.systemctl(ctx, "status", Unit(name), "--no-pager", "-n", "30")
			return fmt.Errorf("container %s exited during boot:\n%s", name, out)
		}
		if _, err := n.Leader(ctx, name); err == nil {
			// The leader exists as soon as the namespace is created. Wait a
			// moment so that systemd inside the container has started the
			// network and sshd before the first command runs.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("container %s did not boot within %s", name, n.cfg.BootTimeout.Duration())
}

// Limits applies CPU and memory limits to a running container by updating the
// transient unit. systemd enforces them immediately, without a reboot.
func (n *Nspawn) Limits(ctx context.Context, vm *state.VM) error {
	out, err := n.systemctl(ctx,
		"set-property", Unit(vm.Name),
		"MemoryMax="+strconv.FormatInt(vm.Memory, 10),
		fmt.Sprintf("CPUQuota=%d%%", vm.CPU*100),
	)
	if err != nil {
		return fmt.Errorf("update limits of %s: %w: %s", vm.Name, err, out)
	}
	return nil
}

// Stop powers the container off.
func (n *Nspawn) Stop(ctx context.Context, name string) error {
	if !n.Running(ctx, name) {
		_, _ = n.systemctl(ctx, "reset-failed", Unit(name))
		return nil
	}
	if _, err := n.systemctl(ctx, "stop", Unit(name)); err != nil {
		return fmt.Errorf("stop container %s: %w", name, err)
	}
	// Wait for systemd to reap the unit so that the image can be removed or
	// recreated immediately afterwards.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if !n.Running(ctx, name) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("container %s did not stop in time", name)
}

// Restart stops and starts the container.
func (n *Nspawn) Restart(ctx context.Context, vm *state.VM, imagePath string) error {
	if err := n.Stop(ctx, vm.Name); err != nil {
		return err
	}
	return n.Start(ctx, vm, imagePath)
}

// Exec describes one command invocation inside a container. The standard
// streams are connected to the started nsenter(1) process.
type Exec struct {
	Argv   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// PTY requests a controlling terminal for the command. It must only be
	// set when Stdin, Stdout and Stderr are the same terminal.
	PTY bool
}

// nsenterArgs is the fixed part of the nsenter(1) command line. The working
// directory is set inside the container with --wdns so that it cannot point
// outside of the new root.
func nsenterArgs(pid int) []string {
	return []string{
		"--target", strconv.Itoa(pid),
		"--mount", "--uts", "--ipc", "--net", "--pid",
		"--root", "--wdns=/",
		"--",
	}
}

// Run executes a command inside a running container and returns its exit code.
// The command runs in the container's namespaces but as a host process, which
// keeps output streaming intact and makes the exit status available. Namespace
// entry is delegated to nsenter(1) so that the daemon does not have to perform
// setns(2) itself.
func (n *Nspawn) Run(ctx context.Context, name string, e Exec) (int, error) {
	cmd, err := n.command(ctx, name, e)
	if err != nil {
		return 1, err
	}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, fmt.Errorf("enter container %s: %w", name, err)
	}
	return 0, nil
}

// StartProcess starts a command inside the container and returns the running
// process. It is used for interactive sessions where the caller owns the
// standard streams.
func (n *Nspawn) StartProcess(ctx context.Context, name string, e Exec) (*exec.Cmd, error) {
	cmd, err := n.command(ctx, name, e)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("enter container %s: %w", name, err)
	}
	return cmd, nil
}

func (n *Nspawn) command(ctx context.Context, name string, e Exec) (*exec.Cmd, error) {
	pid, err := n.Leader(ctx, name)
	if err != nil {
		return nil, err
	}
	args := append(nsenterArgs(pid), e.Argv...)
	cmd := exec.CommandContext(ctx, "nsenter", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = e.Stdin, e.Stdout, e.Stderr
	if e.PTY {
		// Make the allocated terminal the controlling terminal of the
		// session so that job control works inside the container. Setsid
		// also creates a new process group, which is what the cancellation
		// below addresses.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	} else {
		// nsenter forks before exec'ing the command, so the whole process
		// group must be signalled to abort a running payload.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return nil
	}
	return cmd, nil
}

// Status summarises the runtime state of a container.
type Status struct {
	Running bool
	Unit    string
	Since   time.Time
	Leader  int
}

// StatusOf inspects the container through systemd.
func (n *Nspawn) StatusOf(ctx context.Context, name string) Status {
	st := Status{Unit: Unit(name)}
	if !n.Running(ctx, name) {
		return st
	}
	st.Running = true
	if pid, err := n.Leader(ctx, name); err == nil {
		st.Leader = pid
	}
	out, err := n.systemctl(ctx, "show", Unit(name), "-p", "ActiveEnterTimestampMonotonic", "--value")
	if err == nil {
		if usec, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil && usec > 0 {
			st.Since = time.Now().Add(-time.Duration(usec) * time.Microsecond)
		}
	}
	return st
}

// Metrics samples the cgroup counters of a running container.
type Metrics struct {
	CPUUsageNS   int64
	MemoryBytes  int64
	MemorySwap   int64
	MemoryLimit  int64
	ProcessCount int64
}

// Collect reads the cgroup values of a container through systemctl show.
func (n *Nspawn) Collect(ctx context.Context, name string) (Metrics, error) {
	props := []string{
		"CPUUsageNSec",
		"MemoryCurrent",
		"MemorySwapCurrent",
		"MemoryMax",
		"TasksCurrent",
	}
	args := []string{"show", Unit(name)}
	for _, p := range props {
		args = append(args, "-p", p)
	}
	out, err := n.systemctl(ctx, args...)
	if err != nil {
		return Metrics{}, fmt.Errorf("inspect container %s: %w", name, err)
	}
	vals := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		vals[k] = v
	}
	m := Metrics{
		CPUUsageNS:   parseUint(vals["CPUUsageNSec"]),
		MemoryBytes:  parseUint(vals["MemoryCurrent"]),
		MemorySwap:   parseUint(vals["MemorySwapCurrent"]),
		MemoryLimit:  parseUint(vals["MemoryMax"]),
		ProcessCount: parseUint(vals["TasksCurrent"]),
	}
	return m, nil
}

// Interfaces returns the host side of the veth links of a container. The
// names are read from the container's network namespace through /proc.
func (n *Nspawn) Interfaces(ctx context.Context, name string) ([]string, error) {
	pid, err := n.Leader(ctx, name)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/net", pid))
	if err != nil {
		return nil, err
	}
	var ifaces []string
	for _, e := range entries {
		ife, err := net.InterfaceByName(e.Name())
		if err == nil && ife.HardwareAddr != nil {
			ifaces = append(ifaces, e.Name())
		}
	}
	return ifaces, nil
}

func (n *Nspawn) systemctl(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func parseUint(s string) int64 {
	if s == "" || s == "[not set]" {
		return 0
	}
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}
