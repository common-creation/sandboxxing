package vm

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/common-creation/sandboxxing/internal/backend"
	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/host"
	"github.com/common-creation/sandboxxing/internal/image"
	"github.com/common-creation/sandboxxing/internal/process"
	"github.com/common-creation/sandboxxing/internal/progress"
	"github.com/common-creation/sandboxxing/internal/state"
	"github.com/common-creation/sandboxxing/internal/tty"
)

// Manager implements all container operations.
type Manager struct {
	cfg     *config.Config
	log     *slog.Logger
	state   *state.State
	images  *image.Builder
	backend *backend.Nspawn
	host    *host.Manager
}

// New creates the operation manager.
func New(cfg *config.Config, log *slog.Logger, st *state.State) *Manager {
	return &Manager{
		cfg:     cfg,
		log:     log,
		state:   st,
		images:  image.New(cfg, log),
		backend: backend.New(cfg, log),
		host:    host.New(cfg, log),
	}
}

// Options are the container settings accepted by create and copy.
type Options struct {
	Name    string
	Image   string
	CPU     int
	Memory  int64
	Disk    int64
	Comment string
	Tags    []string
	Env     map[string]string
}

// Create builds a new container: the cached base tree is materialised as an
// ext4 disk image, configured, registered and booted.
func (m *Manager) Create(ctx context.Context, opts Options) (*state.VM, error) {
	if opts.Name != "" && !state.ValidName(opts.Name) {
		return nil, fmt.Errorf("invalid container name %q: use letters, digits, '-', '_' or '.'", opts.Name)
	}
	if len(opts.Comment) > 200 {
		return nil, errors.New("comment must be at most 200 bytes")
	}
	m.applyDefaults(&opts)
	name, err := m.reserveName(opts.Name)
	if err != nil {
		return nil, err
	}
	opts.Name = name

	tree, err := m.images.Tree(ctx, opts.Image)
	if err != nil {
		return nil, err
	}
	diskPath := m.diskPath(name)
	progress.From(ctx).Step("creating the disk image of %s (%s)", name, config.DiskSize(opts.Disk).String())
	if err := image.PackExt4(ctx, tree, diskPath, opts.Disk); err != nil {
		os.Remove(diskPath)
		return nil, fmt.Errorf("create disk image: %w", err)
	}
	vm, err := m.finish(ctx, diskPath, opts)
	if err != nil {
		return nil, err
	}
	if err := m.backend.Start(ctx, vm, diskPath); err != nil {
		return vm, fmt.Errorf("container created but failed to start: %w", err)
	}
	return vm, nil
}

// Copy creates a new container from an existing one. The source may be
// running: its disk image is copied while in use and the copy is repaired by
// the file system check that follows, using the journal inside the image.
func (m *Manager) Copy(ctx context.Context, src string, opts Options) (*state.VM, error) {
	source, ok := m.state.Get(src)
	if !ok {
		return nil, fmt.Errorf("container %q not found", src)
	}
	if opts.Image == "" {
		opts.Image = source.Image
	}
	if opts.CPU <= 0 {
		opts.CPU = source.CPU
	}
	if opts.Memory <= 0 {
		opts.Memory = source.Memory
	}
	if opts.Disk <= 0 {
		opts.Disk = source.Disk
	}
	name, err := m.reserveName(opts.Name)
	if err != nil {
		return nil, err
	}
	opts.Name = name

	dst := m.diskPath(name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, err
	}
	progress.From(ctx).Step("copying the disk image of %s to %s", src, name)
	if err := copyImage(ctx, m.diskPath(src), dst); err != nil {
		os.Remove(dst)
		return nil, err
	}
	if opts.Disk != source.Disk {
		if err := m.resizeDiskImage(ctx, dst, opts.Disk); err != nil {
			os.Remove(dst)
			return nil, err
		}
	} else if err := repairImage(ctx, dst); err != nil {
		os.Remove(dst)
		return nil, err
	}
	vm, err := m.finish(ctx, dst, opts)
	if err != nil {
		return nil, err
	}
	m.log.Info("copied container", "source", src, "name", vm.Name)
	if err := m.backend.Start(ctx, vm, dst); err != nil {
		return vm, fmt.Errorf("container copied but failed to start: %w", err)
	}
	return vm, nil
}

// finish reserves an address, configures the disk image and registers the
// container metadata. The disk image is removed when any step fails.
func (m *Manager) finish(ctx context.Context, diskPath string, opts Options) (*state.VM, error) {
	ip, err := m.state.AllocIP()
	if err != nil {
		os.Remove(diskPath)
		return nil, err
	}
	if err := m.prepareDisk(ctx, diskPath, opts.Name, ip, opts.Env); err != nil {
		os.Remove(diskPath)
		return nil, err
	}
	vm := &state.VM{
		Name:      opts.Name,
		Image:     opts.Image,
		CPU:       opts.CPU,
		Memory:    opts.Memory,
		Disk:      opts.Disk,
		Comment:   opts.Comment,
		Tags:      opts.Tags,
		Env:       opts.Env,
		IP:        ip,
		NetConfig: state.NetConfigVersion,
	}
	if err := m.state.Add(vm); err != nil {
		os.Remove(diskPath)
		return nil, err
	}
	m.log.Info("created container", "name", vm.Name, "image", vm.Image, "ip", vm.IP,
		"cpu", vm.CPU, "memory", config.DiskSize(vm.Memory).String(), "disk", config.DiskSize(vm.Disk).String())
	return vm, nil
}

func (m *Manager) applyDefaults(opts *Options) {
	if opts.Image == "" {
		opts.Image = m.cfg.Image
	}
	if opts.CPU <= 0 {
		opts.CPU = m.cfg.DefaultCPU
	}
	if opts.Memory <= 0 {
		opts.Memory = int64(m.cfg.DefaultMemory)
	}
	if opts.Disk <= 0 {
		opts.Disk = int64(m.cfg.DefaultDisk)
	}
}

// reserveName returns a free, valid container name. An empty request is
// replaced by a generated name.
func (m *Manager) reserveName(name string) (string, error) {
	if name == "" {
		return m.uniqueName()
	}
	if !state.ValidName(name) {
		return "", fmt.Errorf("invalid container name %q: use letters, digits, '-', '_' or '.'", name)
	}
	if _, exists := m.state.Get(name); exists {
		return "", fmt.Errorf("container %q already exists", name)
	}
	return name, nil
}

// Remove stops and deletes a container together with its disk image.
func (m *Manager) Remove(ctx context.Context, name string) error {
	vm, ok := m.state.Get(name)
	if !ok {
		return fmt.Errorf("container %q not found", name)
	}
	if err := m.backend.Stop(ctx, vm.Name); err != nil {
		return err
	}
	if err := m.state.Remove(name); err != nil {
		return err
	}
	if err := os.Remove(m.diskPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove disk image of %s: %w", name, err)
	}
	m.log.Info("removed container", "name", name)
	return nil
}

// Restart reboots a container.
func (m *Manager) Restart(ctx context.Context, name string) error {
	vm, ok := m.state.Get(name)
	if !ok {
		return fmt.Errorf("container %q not found", name)
	}
	if err := m.backend.Restart(ctx, vm, m.diskPath(name)); err != nil {
		return err
	}
	m.log.Info("restarted container", "name", name)
	return nil
}

// Resize changes the CPU, memory or disk allowance. The disk can only grow;
// growth is applied offline with resize2fs.
func (m *Manager) Resize(ctx context.Context, name string, cpu int, memory, disk int64) error {
	vm, ok := m.state.Get(name)
	if !ok {
		return fmt.Errorf("container %q not found", name)
	}
	if cpu < 0 || memory < 0 || disk < 0 {
		return errors.New("sizes must not be negative")
	}
	if cpu == 0 && memory == 0 && disk == 0 {
		return errors.New("nothing to resize: pass --cpu, --memory or --disk")
	}
	if disk > 0 && disk < vm.Disk {
		return fmt.Errorf("disk can only grow: current size is %s", config.DiskSize(vm.Disk).String())
	}

	wasRunning := m.backend.Running(ctx, name)
	diskGrew := disk > vm.Disk
	if diskGrew {
		progress.From(ctx).Step("growing the disk of %s to %s", name, config.DiskSize(disk).String())
		if err := m.backend.Stop(ctx, name); err != nil {
			return err
		}
		if err := m.resizeDiskImage(ctx, m.diskPath(name), disk); err != nil {
			return err
		}
	}
	if err := m.state.Update(name, func(v *state.VM) error {
		if cpu > 0 {
			v.CPU = cpu
		}
		if memory > 0 {
			v.Memory = memory
		}
		if disk > 0 {
			v.Disk = disk
		}
		return nil
	}); err != nil {
		return err
	}
	vm, _ = m.state.Get(name)
	switch {
	case !wasRunning:
	case diskGrew:
		if err := m.backend.Start(ctx, vm, m.diskPath(name)); err != nil {
			return err
		}
	default:
		// CPU and memory apply live; only the disk requires a reboot.
		if err := m.backend.Limits(ctx, vm); err != nil {
			return err
		}
	}
	m.log.Info("resized container", "name", name, "cpu", vm.CPU,
		"memory", config.DiskSize(vm.Memory).String(), "disk", config.DiskSize(vm.Disk).String())
	return nil
}

// Exec describes one command invocation inside a container.
type Exec = backend.Exec

// Info is the runtime view of one container.
type Info struct {
	VM      *state.VM
	Running bool
	Uptime  time.Duration
	IP      string
	Metrics *backend.Metrics
}

// List returns the containers matching pattern ("" or "*" means all).
func (m *Manager) List(ctx context.Context, pattern string) ([]Info, error) {
	re, err := compilePattern(pattern)
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, vm := range m.state.List() {
		if re != nil && !re.MatchString(vm.Name) {
			continue
		}
		out = append(out, m.inspect(ctx, vm))
	}
	return out, nil
}

// Stat returns the details and the live usage of one container.
func (m *Manager) Stat(ctx context.Context, name string) (Info, error) {
	vm, ok := m.state.Get(name)
	if !ok {
		return Info{}, fmt.Errorf("container %q not found", name)
	}
	return m.inspect(ctx, vm), nil
}

func (m *Manager) inspect(ctx context.Context, vm *state.VM) Info {
	info := Info{VM: vm, IP: vm.IP}
	st := m.backend.StatusOf(ctx, vm.Name)
	if !st.Running {
		return info
	}
	info.Running = true
	if !st.Since.IsZero() {
		info.Uptime = time.Since(st.Since)
	}
	if mm, err := m.backend.Collect(ctx, vm.Name); err == nil {
		info.Metrics = &mm
	}
	return info
}

// EnsureStarted boots a stopped container. This is what makes direct SSH
// access work even after the host rebooted.
func (m *Manager) EnsureStarted(ctx context.Context, name string) error {
	vm, ok := m.state.Get(name)
	if !ok {
		return fmt.Errorf("container %q not found", name)
	}
	// A container created by an older release is updated even while it is
	// running, so that the fix reaches it without an explicit restart.
	if vm.NetConfig < state.NetConfigVersion {
		if err := m.refreshNetConfig(ctx, vm); err != nil {
			return err
		}
		if updated, ok := m.state.Get(name); ok {
			vm = updated
		}
	}
	if m.backend.Running(ctx, name) {
		return nil
	}
	m.log.Info("starting container on demand", "name", name)
	progress.From(ctx).Step("container %s was stopped, starting it", name)
	return m.backend.Start(ctx, vm, m.diskPath(name))
}

// MigrateNetwork brings every container created by an older release up to
// date. Older versions used the network address of the subnet as the gateway,
// which is not a usable address. Running containers are stopped, updated and
// started again; stopped ones are updated in place.
func (m *Manager) MigrateNetwork(ctx context.Context) error {
	var pending []*state.VM
	for _, vm := range m.state.List() {
		if vm.IP != "" && vm.NetConfig < state.NetConfigVersion {
			pending = append(pending, vm)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	m.log.Info("updating the network configuration of existing containers", "count", len(pending))
	var errs []error
	for _, vm := range pending {
		// Only containers that are running are started again; a stopped one
		// is merely updated and stays stopped.
		wasRunning := m.backend.Running(ctx, vm.Name)
		if err := m.refreshNetConfig(ctx, vm); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", vm.Name, err))
			continue
		}
		if !wasRunning {
			continue
		}
		if err := m.backend.Start(ctx, vm, m.diskPath(vm.Name)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", vm.Name, err))
		}
	}
	return errors.Join(errs...)
}

// refreshNetConfig rewrites the network configuration of a container created
// by an older release. The container is stopped first so that the disk image
// can be mounted.
func (m *Manager) refreshNetConfig(ctx context.Context, vm *state.VM) error {
	if vm.IP == "" || vm.NetConfig >= state.NetConfigVersion {
		return nil
	}
	m.log.Info("updating the network configuration of an existing container",
		"name", vm.Name, "from", vm.NetConfig, "to", state.NetConfigVersion)
	progress.From(ctx).Step("updating the network configuration of %s", vm.Name)
	wasRunning := m.backend.Running(ctx, vm.Name)
	if wasRunning {
		if err := m.backend.Stop(ctx, vm.Name); err != nil {
			return err
		}
	}
	if err := m.prepareDisk(ctx, m.diskPath(vm.Name), vm.Name, vm.IP, vm.Env); err != nil {
		return err
	}
	return m.state.Update(vm.Name, func(v *state.VM) error {
		v.NetConfig = state.NetConfigVersion
		return nil
	})
}

// Running reports whether the container is running.
func (m *Manager) Running(ctx context.Context, name string) bool {
	return m.backend.Running(ctx, name)
}

// Run executes a command inside a container, starting it when needed.
func (m *Manager) Run(ctx context.Context, name string, e backend.Exec) (int, error) {
	if err := m.EnsureStarted(ctx, name); err != nil {
		return 1, err
	}
	return m.backend.Run(ctx, name, e)
}

// StartProcess starts a command inside a container, starting the container
// when needed, and returns the running process.
func (m *Manager) StartProcess(ctx context.Context, name string, e backend.Exec) (*exec.Cmd, error) {
	if err := m.EnsureStarted(ctx, name); err != nil {
		return nil, err
	}
	return m.backend.StartProcess(ctx, name, e)
}

// Terminal allocates a pseudo terminal from the container's devpts instance,
// so that the terminal is resolvable inside the container.
func (m *Manager) Terminal(ctx context.Context, name string) (*tty.Terminal, error) {
	if err := m.EnsureStarted(ctx, name); err != nil {
		return nil, err
	}
	return m.backend.Terminal(ctx, name)
}

// Images lists the cached base images.
func (m *Manager) Images() ([]string, error) { return m.images.List() }

// DataDir is the directory holding the container disk images.
func (m *Manager) DataDir() string { return filepath.Join(m.cfg.DataDir, "vms") }

// DiskPath returns the disk image location of a container.
func (m *Manager) DiskPath(name string) string { return m.diskPath(name) }

// EnsureHost prepares the bridge and NAT rules.
func (m *Manager) EnsureHost(ctx context.Context) error { return m.host.Ensure(ctx) }

// CleanupHost removes the bridge and the NAT rules that EnsureHost created.
func (m *Manager) CleanupHost(ctx context.Context) error { return m.host.Cleanup(ctx) }

// StopAll powers off every container. It runs before the host resources are
// removed so that no sandbox is left without a network. The disk images are
// kept; only the processes are terminated.
func (m *Manager) StopAll(ctx context.Context) (int, error) {
	var errs []error
	stopped := 0
	for _, vm := range m.state.List() {
		if !m.backend.Running(ctx, vm.Name) {
			continue
		}
		if err := m.backend.Stop(ctx, vm.Name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", vm.Name, err))
			continue
		}
		m.log.Info("stopped container", "name", vm.Name)
		stopped++
	}
	return stopped, errors.Join(errs...)
}

// HostCheck runs the host prerequisite checks.
func (m *Manager) HostCheck(ctx context.Context) []host.Checkable { return m.host.Check(ctx) }

func (m *Manager) diskPath(name string) string {
	return filepath.Join(m.DataDir(), name+".img")
}

// prepareDisk configures the container specific files inside the disk image.
// The image is loop mounted for the short duration of the update.
func (m *Manager) prepareDisk(ctx context.Context, diskPath, name, ip string, env map[string]string) error {
	mountDir, err := os.MkdirTemp("", "sandboxxing-prepare-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mountDir)

	cmd := process.Command(ctx, "mount", "-o", "loop", diskPath, mountDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount %s: %w: %s", diskPath, err, strings.TrimSpace(string(out)))
	}
	defer func() {
		_ = exec.Command("umount", "-l", mountDir).Run()
	}()

	write := func(rel, data string, mode os.FileMode) error {
		full := filepath.Join(mountDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		return os.WriteFile(full, []byte(data), mode)
	}
	if err := write("etc/hostname", name+"\n", 0o644); err != nil {
		return err
	}
	// An empty machine-id makes systemd generate a fresh identity from the
	// UUID that systemd-nspawn passes to the container.
	if err := write("etc/machine-id", "", 0o644); err != nil {
		return err
	}
	if len(env) > 0 {
		// /etc/environment is what systemd reads, and the profile script
		// covers login shells, which is what `ssh <name>@host` starts.
		if err := write("etc/environment", envFile(env), 0o644); err != nil {
			return err
		}
		if err := write("etc/profile.d/sandboxxing-env.sh", envProfile(env), 0o644); err != nil {
			return err
		}
	}
	prefix, _ := m.cfg.Mask().Size()
	network := fmt.Sprintf(`[Match]
Name=host0

[Network]
Address=%s/%d
Gateway=%s
`, ip, prefix, m.cfg.Gateway())
	if err := write("etc/systemd/network/20-host0.network", network, 0o644); err != nil {
		return err
	}
	// The container does not run systemd-resolved, so it gets a plain
	// resolv.conf instead of a symlink into a stub resolver. Any pre-existing
	// symlink is replaced, otherwise the write would follow it into the
	// host's /run.
	if err := replaceFile(filepath.Join(mountDir, "etc/resolv.conf"), m.resolvConf(), 0o644); err != nil {
		return err
	}
	return nil
}

// hostResolvers reports the DNS servers of the host, excluding the addresses
// that only a resolver running on the host itself can answer. systemd-resolved
// publishes 127.0.0.53 (and the IPv6 loopback) in /etc/resolv.conf, but the
// container does not run that resolver, so forwarding those addresses would
// make every lookup fail.
func hostResolvers() []string {
	var servers []string
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "nameserver" {
			servers = append(servers, fields[1])
		}
	}
	var usable []string
	for _, s := range servers {
		if isLoopbackResolver(s) {
			continue
		}
		usable = append(usable, s)
	}
	return usable
}

// isLoopbackResolver reports whether an address belongs to a resolver that is
// only reachable from the host itself, such as systemd-resolved's stub
// listener on 127.0.0.53. The container cannot use such an address because the
// resolver that answers it lives in the host's network namespace.
func isLoopbackResolver(addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// replaceFile writes data to path, removing any pre-existing node first so
// that a symlink cannot redirect the write.
func replaceFile(path, data string, mode os.FileMode) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.WriteFile(path, []byte(data), mode)
}

// envFile renders /etc/environment from the container environment.
func envFile(env map[string]string) string {
	keys := sortedKeys(env)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, env[k])
	}
	return b.String()
}

// envProfile renders a profile.d script exporting the container environment
// to login shells.
func envProfile(env map[string]string) string {
	keys := sortedKeys(env)
	var b strings.Builder
	b.WriteString("# generated by sandboxxing\n")
	for _, k := range keys {
		if !envNameRe.MatchString(k) {
			continue
		}
		fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(env[k]))
	}
	return b.String()
}

// envNameRe matches the variable names that may be exported to a shell.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func sortedKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// shellQuote quotes a value for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// resolvConf renders /etc/resolv.conf for the containers. The configured
// servers win; otherwise the host resolvers are used with the ones that only
// run on the host filtered out.
func (m *Manager) resolvConf() string {
	servers := m.cfg.DNS
	source := "config"
	if len(servers) == 0 {
		servers = hostResolvers()
		source = "host"
	}
	if len(servers) == 0 {
		// The host runs systemd-resolved and its stub is not reachable from
		// the container; fall back to public resolvers so that DNS works.
		servers = []string{"1.1.1.1", "8.8.8.8"}
		source = "fallback"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# generated by sandboxxing (source: %s)\n", source)
	for _, s := range servers {
		fmt.Fprintf(&b, "nameserver %s\n", s)
	}
	return b.String()
}

// resizeDiskImage grows the image file and the ext4 file system inside it.
func (m *Manager) resizeDiskImage(ctx context.Context, diskPath string, size int64) error {
	if err := os.Truncate(diskPath, size); err != nil {
		return fmt.Errorf("grow image file %s: %w", diskPath, err)
	}
	return repairImage(ctx, diskPath)
}

// repairImage makes the ext4 file system consistent and grows it to fill the
// image file. e2fsck(8) replays the journal of a copy taken from a running
// container, and resize2fs(8) picks up a previous os.Truncate.
func repairImage(ctx context.Context, diskPath string) error {
	sink := progress.From(ctx)
	out, err := process.Output(ctx, "e2fsck", "-f", "-p", diskPath)
	if out != nil {
		sink.Err().Write(out)
	}
	if err != nil && exitCode(err) > 1 {
		// e2fsck reports 1 when it repaired something, which is expected for
		// a copy of a running container.
		return fmt.Errorf("e2fsck %s: %w", diskPath, err)
	}
	out, err = process.Output(ctx, "resize2fs", diskPath)
	if out != nil {
		sink.Err().Write(out)
	}
	if err != nil {
		return fmt.Errorf("resize2fs %s: %w", diskPath, err)
	}
	return nil
}

// copyImage copies the image file, preserving holes and using a reflink when
// the file system supports it so that copying stays cheap.
func copyImage(ctx context.Context, src, dst string) error {
	out, err := process.Output(ctx, "cp",
		"--reflink=auto", "--sparse=always", "--", src, dst)
	if err != nil {
		return fmt.Errorf("copy %s to %s: %w: %s", src, dst, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// uniqueName returns a short random container name that is not in use.
func (m *Manager) uniqueName() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	for attempt := 0; attempt < 100; attempt++ {
		b := make([]byte, 6)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		for i := range b {
			b[i] = alphabet[int(b[i])%len(alphabet)]
		}
		name := "sbx-" + string(b)
		if _, exists := m.state.Get(name); !exists {
			return name, nil
		}
	}
	return "", errors.New("could not generate a unique container name")
}

// compilePattern turns a shell style pattern into a regular expression.
func compilePattern(pattern string) (*regexp.Regexp, error) {
	if pattern == "" || pattern == "*" {
		return nil, nil
	}
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
