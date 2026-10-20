// Package backend runs the sandbox containers. Nspawn runs them on this host
// with systemd-nspawn(1).
package backend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/process"
	"github.com/common-creation/sandboxxing/internal/progress"
	"github.com/common-creation/sandboxxing/internal/state"
	"github.com/common-creation/sandboxxing/internal/tty"
)

// Machine is the unit name prefix used by systemd-run.
const unitPrefix = "sandboxxing-"

// Nspawn runs containers as transient systemd services.
type Nspawn struct {
	cfg *config.Config
	log *slog.Logger
}

var _ Backend = (*Nspawn)(nil)

// New creates a backend for the configuration.
func New(cfg *config.Config, log *slog.Logger) *Nspawn {
	return &Nspawn{cfg: cfg, log: log}
}

// DiskPath returns the location of the disk image of a container.
func (n *Nspawn) DiskPath(name string) string {
	return filepath.Join(n.cfg.DataDir, "vms", name+".img")
}

// Unit returns the systemd unit name of a container.
func Unit(name string) string { return unitPrefix + name }

// fuseDevice is the host device handed to nested containers so that
// fuse-overlayfs (a storage driver that works inside nspawn) can run.
const fuseDevice = "/dev/fuse"

// nested reports whether a container may run its own container engine: either
// the whole host enables it with the "nested" option or the container was
// created with --nested.
func (n *Nspawn) nested(vm *state.VM) bool {
	return n.cfg.Nested || vm.Nested
}

// nestedArgs returns the extra systemd-nspawn arguments that let a container
// run Docker or another container engine. systemd-nspawn's seccomp filter
// blocks bpf() by default, but runc calls it to install the cgroup v2 device
// controller, so every nested container start fails without this. The keyring
// syscalls are needed by images that use the kernel keyring.
func nestedArgs() []string {
	args := []string{
		"--system-call-filter=@keyring",
		"--system-call-filter=bpf",
	}
	// A nested engine can be told to use fuse-overlayfs when its default
	// overlay2 driver is not usable, which needs /dev/fuse. It is only passed
	// through when the host provides it, so a host without the fuse module
	// can still start the container.
	if fuseAvailable() {
		args = append(args, "--bind="+fuseDevice)
	}
	return args
}

// fuseAvailable reports whether the host exposes /dev/fuse.
func fuseAvailable() bool {
	_, err := os.Stat(fuseDevice)
	return err == nil
}

// Running reports whether the container is currently running.
func (n *Nspawn) Running(ctx context.Context, vm *state.VM) bool {
	return n.running(ctx, vm.Name)
}

func (n *Nspawn) running(ctx context.Context, name string) bool {
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
func (n *Nspawn) Start(ctx context.Context, vm *state.VM) error {
	if n.running(ctx, vm.Name) {
		return nil
	}
	// A unit left behind by an earlier failure must not block the restart.
	_, _ = n.systemctl(ctx, "reset-failed", Unit(vm.Name))

	args := n.startArgs(vm, n.DiskPath(vm.Name))
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

// startArgs builds the systemd-run command line that boots a container. The
// resource limits are unit properties, the container settings are arguments of
// systemd-nspawn.
func (n *Nspawn) startArgs(vm *state.VM, imagePath string) []string {
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
	if strings.EqualFold(n.cfg.TmpSize, config.TmpSizeDisk) {
		// Keep /tmp on the container's own disk image. nspawn mounts a tmpfs
		// unless SYSTEMD_NSPAWN_TMPFS_TMP is 0; the image already carries a
		// /tmp from pacstrap, so nothing else is needed, and the size is
		// limited by the container disk instead of the host memory. The
		// variable belongs to the unit, so it is passed to systemd-run.
		args = append(args, "-E", "SYSTEMD_NSPAWN_TMPFS_TMP=0")
	}
	// A nested container runs its own engine, which needs to open /dev/fuse
	// (fuse-overlayfs). nspawn's device policy is closed by default, so the
	// device has to be allowed as well as passed through.
	if n.nested(vm) && fuseAvailable() {
		args = append(args, "-p", "DeviceAllow="+fuseDevice+" rwm")
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
	if n.nested(vm) {
		args = append(args, nestedArgs()...)
	}
	if n.cfg.TmpSize != "" && !strings.EqualFold(n.cfg.TmpSize, config.TmpSizeDisk) {
		// A custom mount on top of the one nspawn creates raises or lowers
		// the limit. An empty value keeps the nspawn default: a tmpfs sized
		// at 10% of the host memory.
		args = append(args, "--tmpfs=/tmp:mode=01777,size="+n.cfg.TmpSize)
	}
	// Shared host directories. The bind mount is happy when the destination
	// does not exist inside the image: nspawn creates the mount point.
	for _, share := range n.shares(vm) {
		option := "--bind="
		if share.ReadOnly {
			option = "--bind-ro="
		}
		args = append(args, option+share.Path+":"+share.MountTarget())
	}
	return args
}

// waitBoot waits until the container reports that it has finished booting.
func (n *Nspawn) waitBoot(ctx context.Context, name string) error {
	deadline := time.Now().Add(n.cfg.BootTimeout.Duration())
	for time.Now().Before(deadline) {
		if !n.running(ctx, name) {
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

// shares returns the host directories to mount into a container: the ones
// configured for every container plus the ones recorded for this container.
// A container specific entry with the same target wins.
func (n *Nspawn) shares(vm *state.VM) []config.Share {
	return config.MergeShares(n.cfg.Shares, vm.Shares)
}

// Stop powers the container off.
func (n *Nspawn) Stop(ctx context.Context, vm *state.VM) error {
	name := vm.Name
	if !n.running(ctx, name) {
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
		if !n.running(ctx, name) {
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
func (n *Nspawn) Restart(ctx context.Context, vm *state.VM) error {
	if err := n.Stop(ctx, vm); err != nil {
		return err
	}
	return n.Start(ctx, vm)
}

// nsenterArgs is the fixed part of the nsenter(1) command line. The working
// directory is set inside the container with --wdns, so that it cannot point
// outside of the new root.
func nsenterArgs(pid int, dir string) []string {
	if dir == "" {
		dir = "/"
	}
	return []string{
		"--target", strconv.Itoa(pid),
		"--mount", "--uts", "--ipc", "--net", "--pid",
		"--root", "--wdns=" + dir,
		"--",
	}
}

// Run executes a command inside a running container and returns its exit code.
// The command runs in the container's namespaces but as a host process, which
// keeps output streaming intact and makes the exit status available. Namespace
// entry is delegated to nsenter(1) so that the daemon does not have to perform
// setns(2) itself.
func (n *Nspawn) Run(ctx context.Context, vm *state.VM, e Exec) (int, error) {
	cmd, err := n.command(ctx, vm.Name, e)
	if err != nil {
		return 1, err
	}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, fmt.Errorf("enter container %s: %w", vm.Name, err)
	}
	return 0, nil
}

// terminal allocates a pseudo terminal from the container's own devpts
// instance. The slave appears as /dev/pts/<n> inside the container, which is
// what makes ttyname(3) and the `tty` command work.
func (n *Nspawn) terminal(ctx context.Context, name string) (*tty.Terminal, error) {
	pid, err := n.Leader(ctx, name)
	if err != nil {
		return nil, err
	}
	return tty.Open(pid)
}

// StartProcess starts a command inside the container and returns the running
// process. It is used for interactive sessions where the caller owns the
// standard streams.
func (n *Nspawn) StartProcess(ctx context.Context, vm *state.VM, e Exec) (Process, error) {
	cmd, err := n.command(ctx, vm.Name, e)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("enter container %s: %w", vm.Name, err)
	}
	return cmdProcess{cmd}, nil
}

// StartTerminal runs a command on a pseudo terminal allocated from the
// container's devpts instance, so that the terminal is resolvable inside the
// container. The master carries the data and the slave is handed to the
// process that runs in the container.
func (n *Nspawn) StartTerminal(ctx context.Context, vm *state.VM, e Exec, rows, cols int) (Terminal, error) {
	term, err := n.terminal(ctx, vm.Name)
	if err != nil {
		return nil, err
	}
	if err := term.SetSize(rows, cols); err != nil {
		term.Close()
		return nil, err
	}
	e.Stdin, e.Stdout, e.Stderr = term.Slave, term.Slave, term.Slave
	e.PTY = true
	proc, err := n.StartProcess(ctx, vm, e)
	if err != nil {
		term.Close()
		return nil, err
	}
	// The descriptor is captured now so that readers never see the field
	// while Close clears it.
	return &nspawnTerminal{term: term, master: term.Master, proc: proc}, nil
}

// Dial connects to a port of the container over the bridge, with the address
// the daemon assigned to it.
func (n *Nspawn) Dial(ctx context.Context, vm *state.VM, port int) (net.Conn, error) {
	if vm.IP == "" {
		return nil, fmt.Errorf("container %q has no address yet", vm.Name)
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	return dialer.DialContext(ctx, "tcp", net.JoinHostPort(vm.IP, strconv.Itoa(port)))
}

// cmdProcess is a process started on this host.
type cmdProcess struct{ cmd *exec.Cmd }

func (p cmdProcess) Wait() (int, error) {
	if err := p.cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}

// nspawnTerminal is a process running on a terminal of the container's devpts.
type nspawnTerminal struct {
	term   *tty.Terminal
	master *os.File
	proc   Process
}

func (t *nspawnTerminal) Read(p []byte) (int, error)  { return t.master.Read(p) }
func (t *nspawnTerminal) Write(p []byte) (int, error) { return t.master.Write(p) }
func (t *nspawnTerminal) Resize(rows, cols int) error { return t.term.SetSize(rows, cols) }

func (t *nspawnTerminal) Wait() (int, error) {
	code, err := t.proc.Wait()
	// Closing the container side releases the terminal: a reader of the
	// master sees the end of file and a writer fails on its next write.
	_ = t.term.CloseSlave()
	return code, err
}

func (t *nspawnTerminal) Close() error { return t.term.Close() }

func (n *Nspawn) command(ctx context.Context, name string, e Exec) (*exec.Cmd, error) {
	pid, err := n.Leader(ctx, name)
	if err != nil {
		return nil, err
	}
	args := append(nsenterArgs(pid, e.Dir), e.Argv...)
	cmd := exec.CommandContext(ctx, "nsenter", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = e.Stdin, e.Stdout, e.Stderr
	if len(e.Env) > 0 {
		cmd.Env = process.MergeEnv(os.Environ(), e.Env)
	}
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

// StatusOf inspects the container through systemd.
func (n *Nspawn) StatusOf(ctx context.Context, vm *state.VM) Status {
	name := vm.Name
	st := Status{Unit: Unit(name)}
	if !n.running(ctx, name) {
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

// Collect reads the cgroup values of a container through systemctl show.
func (n *Nspawn) Collect(ctx context.Context, vm *state.VM) (Metrics, error) {
	name := vm.Name
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
