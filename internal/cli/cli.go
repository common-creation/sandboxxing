// Package cli implements the control commands that are reachable through the
// SSH protocol. Commands follow the exe.dev style interface.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/progress"
	"github.com/common-creation/sandboxxing/internal/state"
	"github.com/common-creation/sandboxxing/internal/vm"
)

// IO carries the streams of one command invocation.
type IO struct {
	In   io.Reader
	Out  io.Writer
	Err  io.Writer
	TTY  bool
	Rows int
	Cols int
}

// Runner dispatches control commands.
type Runner struct {
	cfg  *config.Config
	log  *slog.Logger
	vms  *vm.Manager
	st   *state.State
	host string
}

// New creates a command runner. host is the name advertised in hints.
func New(cfg *config.Config, log *slog.Logger, vms *vm.Manager, st *state.State, host string) *Runner {
	if host == "" {
		if h, err := os.Hostname(); err == nil {
			host = h
		} else {
			host = "localhost"
		}
	}
	return &Runner{cfg: cfg, log: log, vms: vms, st: st, host: host}
}

// Host returns the advertised daemon host name.
func (r *Runner) Host() string { return r.host }

// Port returns the port the SSH endpoint listens on.
func (r *Runner) Port() int {
	_, port, err := splitHostPort(r.cfg.SSHAddr)
	if err != nil {
		return 2222
	}
	return port
}

// Run parses argv and executes the matching command. It returns the process
// exit code that is reported back to the SSH client.
//
// The session streams are attached to the context so that long running
// operations can forward the output of the commands they drive (pacstrap,
// systemd-nspawn, ...) to the SSH client that started them.
func (r *Runner) Run(ctx context.Context, argv []string, sess *IO) int {
	ctx = progress.With(ctx, progress.New(sess.Out, sess.Err))
	if len(argv) == 0 {
		return r.printHelp(sess)
	}
	name, rest := argv[0], argv[1:]
	switch name {
	case "ls", "list":
		return r.cmdList(ctx, rest, sess)
	case "new", "create":
		return r.cmdNew(ctx, rest, sess)
	case "rm", "delete", "remove":
		return r.cmdRemove(ctx, rest, sess)
	case "restart", "reboot":
		return r.cmdRestart(ctx, rest, sess)
	case "cp", "copy":
		return r.cmdCopy(ctx, rest, sess)
	case "resize":
		return r.cmdResize(ctx, rest, sess)
	case "stat":
		return r.cmdStat(ctx, rest, sess)
	case "ssh":
		return r.cmdSSH(ctx, rest, sess)
	case "images":
		return r.cmdImages(rest, sess)
	case "billing":
		return r.cmdBilling(rest, sess)
	case "host-check", "doctor":
		return r.cmdHostCheck(ctx, rest, sess)
	case "ssh-config":
		return r.cmdSSHConfig(rest, sess)
	case "help", "-h", "--help":
		return r.printHelp(sess)
	default:
		fmt.Fprintf(sess.Err, "sandboxxing: unknown command %q\n\n", name)
		return r.printHelp(sess)
	}
}

// Shell runs a small REPL for interactive sessions of control users.
func (r *Runner) Shell(ctx context.Context, sess *IO) int {
	fmt.Fprintf(sess.Out, "sandboxxing console on %s\nType 'help' for the command list, 'exit' to quit.\n", r.host)
	for {
		if _, err := fmt.Fprint(sess.Out, "sandboxxing> "); err != nil {
			return 0
		}
		line, err := sess.readLine()
		switch {
		case errors.Is(err, errInterrupt):
			fmt.Fprintln(sess.Out)
			return 0
		case errors.Is(err, errEOF):
			// Ctrl-D ends the session without an error.
			return 0
		case err != nil && !errors.Is(err, io.EOF):
			fmt.Fprintf(sess.Err, "sandboxxing: %v\n", err)
			fmt.Fprintln(sess.Out)
			return 0
		case errors.Is(err, io.EOF):
			fmt.Fprintln(sess.Out)
			return 0
		}
		argv, err := Split(line)
		if err != nil {
			fmt.Fprintf(sess.Err, "sandboxxing: %v\n", err)
			continue
		}
		if len(argv) == 0 {
			continue
		}
		switch argv[0] {
		case "exit", "quit", "logout":
			return 0
		}
		r.Run(ctx, argv, sess)
	}
}

func (r *Runner) printHelp(sess *IO) int {
	fmt.Fprint(sess.Out, `sandboxxing - manage systemd-nspawn sandboxes over SSH

Usage:
  ssh `+r.host+` <command> [options] [arguments]
  ssh <name>@`+r.host+` [command...]      run a command inside a container

Commands:
  ls [-l] [--group=none|tag|type] [--json] [name|pattern]
        list containers
  new [--name=N] [--image=I] [--cpu=N] [--memory=4G] [--disk=20G]
      [--comment=TEXT] [--tag=T] [--env K=V] [--setup-script=FILE] [--json]
        create and boot a container
  rm <name>... [--json]
        remove containers
  restart <name> [--json]
        reboot a container
  cp <source> [new-name] [--cpu=N] [--memory=4G] [--disk=20G] [--copy-tags] [--json]
        copy a container
  resize <name> [--cpu=N] [--memory=4G] [--disk=20G] [--json]
        change the resource limits (disk can only grow)
  stat <name> [--range=24h|7d|30d] [--json]
        show container details and live resource usage
  ssh [-l user] [user@]name [command...]
        run a command inside a container from the host
  images
        list cached base images
  ssh-config [--host=NAME]
        print the ssh_config snippet that creates vm name aliases
  host-check
        verify the host prerequisites
  billing plan
        show the capacity of the host

Options such as --cpu=4, --memory=4GB, --disk=20G accept the same values as
systemd unit settings. Memory and CPU are enforced through the transient
systemd unit of the container.
`)
	return 0
}

func (r *Runner) cmdList(ctx context.Context, args []string, sess *IO) int {
	opts, err := parseFlags(args, map[string]flagSpec{
		"l":     {kind: flagBool},
		"json":  {kind: flagBool},
		"group": {kind: flagString},
	})
	if err != nil {
		return r.fail(sess, err)
	}
	infos, err := r.vms.List(ctx, strings.Join(opts.positional, " "))
	if err != nil {
		return r.fail(sess, err)
	}
	if opts.bool("json") {
		return r.writeJSON(sess, infos)
	}

	group := opts.str("group")
	switch group {
	case "", "none":
		r.printList(sess, infos, opts.bool("l"))
	case "tag":
		r.printGroupedList(sess, infos, opts.bool("l"), func(vm *state.VM) []string {
			if len(vm.Tags) == 0 {
				return []string{"(no tag)"}
			}
			return vm.Tags
		})
	case "type":
		r.printGroupedList(sess, infos, opts.bool("l"), func(vm *state.VM) []string {
			return []string{vm.Image}
		})
	case "region":
		return r.fail(sess, errors.New("--group=region is not available: sandboxxing runs on a single host"))
	default:
		return r.fail(sess, fmt.Errorf("invalid --group value %q: use none, tag or type", group))
	}
	return 0
}

func (r *Runner) printList(sess *IO, infos []vm.Info, long bool) {
	if len(infos) == 0 {
		fmt.Fprintln(sess.Out, "no containers")
		return
	}
	tw := newTable(sess.Out)
	tw.header("NAME", "STATUS", "IMAGE", "IP", "SSH")
	for _, info := range infos {
		tw.row(info.VM.Name, statusText(info), info.VM.Image, info.IP, r.vmSSH(info.VM))
	}
	tw.flush()
	if long {
		fmt.Fprintln(sess.Out)
		for _, info := range infos {
			r.printDetail(sess, info)
			fmt.Fprintln(sess.Out)
		}
	}
}

func (r *Runner) printGroupedList(sess *IO, infos []vm.Info, long bool, keys func(*state.VM) []string) {
	groups := map[string][]vm.Info{}
	var order []string
	for _, info := range infos {
		for _, k := range keys(info.VM) {
			if _, seen := groups[k]; !seen {
				order = append(order, k)
			}
			groups[k] = append(groups[k], info)
		}
	}
	sort.Strings(order)
	for i, k := range order {
		if i > 0 {
			fmt.Fprintln(sess.Out)
		}
		fmt.Fprintf(sess.Out, "%s:\n", k)
		r.printList(sess, groups[k], long)
	}
}

func (r *Runner) printDetail(sess *IO, info vm.Info) {
	fmt.Fprintf(sess.Out, "name:      %s\n", info.VM.Name)
	fmt.Fprintf(sess.Out, "status:    %s\n", statusText(info))
	fmt.Fprintf(sess.Out, "image:     %s\n", info.VM.Image)
	fmt.Fprintf(sess.Out, "address:   %s\n", info.IP)
	fmt.Fprintf(sess.Out, "ssh:       %s\n", r.vmSSH(info.VM))
	fmt.Fprintf(sess.Out, "cpu:       %d\n", info.VM.CPU)
	fmt.Fprintf(sess.Out, "memory:    %s\n", config.DiskSize(info.VM.Memory).String())
	fmt.Fprintf(sess.Out, "disk:      %s\n", config.DiskSize(info.VM.Disk).String())
	fmt.Fprintf(sess.Out, "created:   %s\n", info.VM.Created.Local().Format(time.RFC3339))
	fmt.Fprintf(sess.Out, "comment:   %s\n", info.VM.Comment)
	if len(info.VM.Tags) > 0 {
		fmt.Fprintf(sess.Out, "tags:      %s\n", strings.Join(info.VM.Tags, ", "))
	}
	if len(info.VM.Env) > 0 {
		fmt.Fprintf(sess.Out, "env:       %s\n", envString(info.VM.Env))
	}
	if info.Running {
		fmt.Fprintf(sess.Out, "uptime:    %s\n", humanDuration(info.Uptime))
		if info.Metrics != nil {
			fmt.Fprintf(sess.Out, "cpu(time): %s\n", humanDuration(time.Duration(info.Metrics.CPUUsageNS)))
			fmt.Fprintf(sess.Out, "mem(now):  %s\n", config.DiskSize(info.Metrics.MemoryBytes).String())
			fmt.Fprintf(sess.Out, "procs:     %d\n", info.Metrics.ProcessCount)
		}
	}
}

func (r *Runner) cmdStat(ctx context.Context, args []string, sess *IO) int {
	opts, err := parseFlags(args, map[string]flagSpec{
		"json":  {kind: flagBool},
		"range": {kind: flagString},
	})
	if err != nil {
		return r.fail(sess, err)
	}
	if len(opts.positional) != 1 {
		return r.fail(sess, errors.New("usage: stat <name> [--range=24h|7d|30d] [--json]"))
	}
	switch opts.str("range") {
	case "", "24h", "7d", "30d":
	default:
		return r.fail(sess, errors.New("invalid --range: use 24h, 7d or 30d"))
	}
	info, err := r.vms.Stat(ctx, opts.positional[0])
	if err != nil {
		return r.fail(sess, err)
	}
	if opts.bool("json") {
		return r.writeJSON(sess, info)
	}
	r.printDetail(sess, info)
	if info.Running && info.Metrics != nil {
		fmt.Fprintf(sess.Out, "cpu quota: %d%%\n", info.VM.CPU*100)
		fmt.Fprintf(sess.Out, "mem max:   %s\n", config.DiskSize(info.Metrics.MemoryLimit).String())
		fmt.Fprintf(sess.Out, "mem swap:  %s\n", config.DiskSize(info.Metrics.MemorySwap).String())
	}
	if !info.Running {
		fmt.Fprintln(sess.Out, "note:      container is stopped; it starts on the next ssh")
	}
	fmt.Fprintln(sess.Out, "note:      sandboxxing keeps no metric history; values are live samples")
	return 0
}

// cmdSSHConfig prints the ssh_config snippet that turns a container name into
// a host name: with `User %n` the whole host name becomes the SSH user name,
// and sandboxxing treats everything before the first dot as the container.
func (r *Runner) cmdSSHConfig(args []string, sess *IO) int {
	opts, err := parseFlags(args, map[string]flagSpec{
		"host":   {kind: flagString},
		"port":   {kind: flagString},
		"domain": {kind: flagString},
	})
	if err != nil {
		return r.fail(sess, err)
	}
	host := opts.str("host")
	if host == "" {
		host = r.host
	}
	port := r.Port()
	if p := opts.str("port"); p != "" {
		_, parsed, err := splitHostPort(":" + p)
		if err != nil {
			return r.fail(sess, errors.New("invalid --port"))
		}
		port = parsed
	}
	domain := opts.str("domain")
	if domain == "" {
		domain = r.cfg.Domain
	}
	if domain == "" {
		domain = "sandbox.example.com"
	}
	fmt.Fprintf(sess.Out, `# sandboxxing ssh aliases
# Install as /etc/ssh/ssh_config.d/99-sandboxxing.conf
#
# With this file a container is reachable as a host name:
#   ssh demo.%s
# sandboxxing strips the domain and logs into the container "demo".
Host *.%s
    HostName %s
    Port %d
    User %%n

# Without this file the user name form works as well:
#   ssh <container>@%s -p %d
`, domain, domain, host, port, host, port)
	return 0
}

func (r *Runner) cmdImages(args []string, sess *IO) int {
	opts, err := parseFlags(args, map[string]flagSpec{"json": {kind: flagBool}})
	if err != nil {
		return r.fail(sess, err)
	}
	images, err := r.vms.Images()
	if err != nil {
		return r.fail(sess, err)
	}
	if opts.bool("json") {
		return r.writeJSON(sess, images)
	}
	if len(images) == 0 {
		fmt.Fprintln(sess.Out, "no cached images yet; the first 'new' builds one with pacstrap")
		return 0
	}
	tw := newTable(sess.Out)
	tw.header("IMAGE")
	for _, name := range images {
		tw.row(name)
	}
	tw.flush()
	return 0
}

func (r *Runner) cmdBilling(args []string, sess *IO) int {
	opts, err := parseFlags(args, map[string]flagSpec{"json": {kind: flagBool}})
	if err != nil {
		return r.fail(sess, err)
	}
	if len(opts.positional) > 0 && opts.positional[0] != "plan" {
		return r.fail(sess, errors.New("usage: billing plan"))
	}
	plan := inspectHost(r.cfg)
	if opts.bool("json") {
		return r.writeJSON(sess, plan)
	}
	fmt.Fprintf(sess.Out, "sandboxxing runs locally on one host; there is no billing.\n\n")
	fmt.Fprintf(sess.Out, "cpus:            %d\n", plan.CPUs)
	fmt.Fprintf(sess.Out, "memory:          %s total, %s available\n",
		config.DiskSize(plan.MemoryTotal).String(), config.DiskSize(plan.MemoryAvailable).String())
	fmt.Fprintf(sess.Out, "disk:            %s available on %s\n",
		config.DiskSize(plan.DiskAvailable).String(), plan.DataDir)
	fmt.Fprintf(sess.Out, "containers:      %d defined, %d running\n", plan.Containers, plan.Running)
	fmt.Fprintf(sess.Out, "limits:          cpu up to %d, memory up to %s per container\n",
		plan.CPUs, config.DiskSize(plan.MemoryTotal).String())
	fmt.Fprintf(sess.Out, "defaults:        cpu=%d memory=%s disk=%s\n",
		r.cfg.DefaultCPU, r.cfg.DefaultMemory.String(), r.cfg.DefaultDisk.String())
	return 0
}

func (r *Runner) cmdHostCheck(ctx context.Context, args []string, sess *IO) int {
	opts, err := parseFlags(args, map[string]flagSpec{"json": {kind: flagBool}})
	if err != nil {
		return r.fail(sess, err)
	}
	checks := r.vms.HostCheck(ctx)
	if opts.bool("json") {
		return r.writeJSON(sess, checks)
	}
	failed := 0
	tw := newTable(sess.Out)
	tw.header("STATE", "CHECK", "DETAIL")
	for _, c := range checks {
		state := "ok"
		if !c.OK {
			state = "FAIL"
			if !c.Critical {
				state = "warn"
			}
			if c.Critical {
				failed++
			}
		}
		tw.row(state, c.Name, c.Detail)
	}
	tw.flush()
	if failed > 0 {
		fmt.Fprintf(sess.Err, "\n%d check(s) failed\n", failed)
		return 1
	}
	return 0
}

func (r *Runner) vmSSH(vm *state.VM) string {
	return fmt.Sprintf("ssh %s@%s -p %d", vm.Name, r.host, r.Port())
}

func (r *Runner) writeJSON(sess *IO, v any) int {
	enc := json.NewEncoder(sess.Out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return r.fail(sess, err)
	}
	return 0
}

func (r *Runner) fail(sess *IO, err error) int {
	fmt.Fprintf(sess.Err, "sandboxxing: %v\n", err)
	return 1
}

func statusText(info vm.Info) string {
	if info.Running {
		return "running"
	}
	return "stopped"
}

func envString(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+env[k])
	}
	return strings.Join(parts, " ")
}

// readLine reads one line for the interactive console.
func (sess *IO) readLine() (string, error) {
	lr := &lineReader{in: sess.In, out: sess.Out, echo: sess.TTY}
	return lr.readLine()
}
