package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/progress"
	"github.com/common-creation/sandboxxing/internal/vm"
)

func (r *Runner) cmdNew(ctx context.Context, args []string, sess *IO) int {
	opts, err := parseFlags(args, map[string]flagSpec{
		"name":         {kind: flagString},
		"image":        {kind: flagString},
		"cpu":          {kind: flagString},
		"memory":       {kind: flagString},
		"disk":         {kind: flagString},
		"comment":      {kind: flagString},
		"tag":          {kind: flagString, repeatable: true, commaList: true},
		"env":          {kind: flagString, repeatable: true},
		"setup-script": {kind: flagString},
		"json":         {kind: flagBool},
		"no-start":     {kind: flagBool},
		"prompt":       {kind: flagString},
	})
	if err != nil {
		return r.fail(sess, err)
	}
	if len(opts.positional) > 0 {
		return r.fail(sess, fmt.Errorf("unexpected arguments: %s", strings.Join(opts.positional, " ")))
	}

	create := vm.Options{
		Name:    opts.str("name"),
		Image:   opts.str("image"),
		Comment: opts.str("comment"),
		Tags:    opts.strs("tag"),
	}
	if create.CPU, err = r.parseCPU(opts.str("cpu")); err != nil {
		return r.fail(sess, err)
	}
	if create.Memory, err = r.parseSize(opts.str("memory"), "memory"); err != nil {
		return r.fail(sess, err)
	}
	if create.Disk, err = r.parseSize(opts.str("disk"), "disk"); err != nil {
		return r.fail(sess, err)
	}
	if create.Env, err = parseEnv(opts.strs("env")); err != nil {
		return r.fail(sess, err)
	}

	progress.From(ctx).Step("creating container %s", displayName(create.Name))
	vmInfo, err := r.vms.Create(ctx, create)
	if err != nil {
		return r.fail(sess, err)
	}

	// Optional setup script and initial command are executed inside the new
	// container, exactly like a first login would.
	script := opts.str("setup-script")
	if script == "/dev/stdin" {
		b, err := io.ReadAll(sess.In)
		if err != nil {
			return r.fail(sess, fmt.Errorf("read setup script from stdin: %w", err))
		}
		script = string(b)
	}
	if script != "" {
		if err := r.runScript(ctx, vmInfo.Name, script); err != nil {
			return r.fail(sess, fmt.Errorf("setup script failed: %w", err))
		}
	}
	if prompt := opts.str("prompt"); prompt != "" {
		if prompt == "/dev/stdin" {
			b, err := io.ReadAll(sess.In)
			if err != nil {
				return r.fail(sess, fmt.Errorf("read prompt from stdin: %w", err))
			}
			prompt = string(b)
		}
		if err := r.runScript(ctx, vmInfo.Name, prompt); err != nil {
			return r.fail(sess, fmt.Errorf("initial prompt failed: %w", err))
		}
	}

	if opts.bool("json") {
		return r.writeJSON(sess, vmInfo)
	}
	// The container name stays on stdout so that it can be captured by a
	// script; the human readable summary goes to stderr.
	fmt.Fprintf(sess.Out, "%s\n", vmInfo.Name)
	fmt.Fprintf(sess.Err, "ready: ssh %s@%s -p %d\n", vmInfo.Name, r.host, r.Port())
	return 0
}

func (r *Runner) cmdRemove(ctx context.Context, args []string, sess *IO) int {
	opts, err := parseFlags(args, map[string]flagSpec{"json": {kind: flagBool}})
	if err != nil {
		return r.fail(sess, err)
	}
	if len(opts.positional) == 0 {
		return r.fail(sess, errors.New("usage: rm <name>..."))
	}
	removed := make([]string, 0, len(opts.positional))
	for _, name := range opts.positional {
		progress.From(ctx).Step("removing container %s", name)
		if err := r.vms.Remove(ctx, name); err != nil {
			return r.fail(sess, err)
		}
		removed = append(removed, name)
	}
	if opts.bool("json") {
		return r.writeJSON(sess, map[string]any{"removed": removed})
	}
	fmt.Fprintf(sess.Out, "removed %s\n", strings.Join(removed, ", "))
	return 0
}

func (r *Runner) cmdRestart(ctx context.Context, args []string, sess *IO) int {
	opts, err := parseFlags(args, map[string]flagSpec{"json": {kind: flagBool}})
	if err != nil {
		return r.fail(sess, err)
	}
	if len(opts.positional) != 1 {
		return r.fail(sess, errors.New("usage: restart <name>"))
	}
	name := opts.positional[0]
	progress.From(ctx).Step("restarting container %s", name)
	if err := r.vms.Restart(ctx, name); err != nil {
		return r.fail(sess, err)
	}
	info, err := r.vms.Stat(ctx, name)
	if err != nil {
		return r.fail(sess, err)
	}
	if opts.bool("json") {
		return r.writeJSON(sess, info)
	}
	fmt.Fprintf(sess.Out, "restarted %s\n", name)
	return 0
}

func (r *Runner) cmdCopy(ctx context.Context, args []string, sess *IO) int {
	opts, err := parseFlags(args, map[string]flagSpec{
		"cpu":       {kind: flagString},
		"memory":    {kind: flagString},
		"disk":      {kind: flagString},
		"json":      {kind: flagBool},
		"copy-tags": {kind: flagBool, hasValue: true, def: "true"},
		"comment":   {kind: flagString},
	})
	if err != nil {
		return r.fail(sess, err)
	}
	if len(opts.positional) < 1 || len(opts.positional) > 2 {
		return r.fail(sess, errors.New("usage: cp <source> [new-name]"))
	}
	copyOpts := vm.Options{
		Image:   r.cfg.Image,
		Comment: opts.str("comment"),
	}
	if len(opts.positional) == 2 {
		copyOpts.Name = opts.positional[1]
	}
	if copyOpts.CPU, err = r.parseCPU(opts.str("cpu")); err != nil {
		return r.fail(sess, err)
	}
	if copyOpts.Memory, err = r.parseSize(opts.str("memory"), "memory"); err != nil {
		return r.fail(sess, err)
	}
	if copyOpts.Disk, err = r.parseSize(opts.str("disk"), "disk"); err != nil {
		return r.fail(sess, err)
	}
	copyOpts.Image = ""
	if opts.boolDefault("copy-tags", true) {
		if source, ok := r.st.Get(opts.positional[0]); ok {
			copyOpts.Tags = append(copyOpts.Tags, source.Tags...)
		}
	}
	progress.From(ctx).Step("copying container %s", opts.positional[0])
	created, err := r.vms.Copy(ctx, opts.positional[0], copyOpts)
	if err != nil {
		return r.fail(sess, err)
	}
	if opts.bool("json") {
		return r.writeJSON(sess, created)
	}
	fmt.Fprintf(sess.Out, "%s\n", created.Name)
	fmt.Fprintf(sess.Out, "ssh %s@%s -p %d\n", created.Name, r.host, r.Port())
	return 0
}

func (r *Runner) cmdResize(ctx context.Context, args []string, sess *IO) int {
	opts, err := parseFlags(args, map[string]flagSpec{
		"cpu":    {kind: flagString},
		"memory": {kind: flagString},
		"disk":   {kind: flagString},
		"json":   {kind: flagBool},
	})
	if err != nil {
		return r.fail(sess, err)
	}
	if len(opts.positional) != 1 {
		return r.fail(sess, errors.New("usage: resize <name> [--cpu=N] [--memory=4G] [--disk=20G]"))
	}
	cpu, err := r.parseCPU(opts.str("cpu"))
	if err != nil {
		return r.fail(sess, err)
	}
	memory, err := r.parseSize(opts.str("memory"), "memory")
	if err != nil {
		return r.fail(sess, err)
	}
	disk, err := r.parseSize(opts.str("disk"), "disk")
	if err != nil {
		return r.fail(sess, err)
	}
	progress.From(ctx).Step("resizing container %s", opts.positional[0])
	if err := r.vms.Resize(ctx, opts.positional[0], cpu, memory, disk); err != nil {
		return r.fail(sess, err)
	}
	info, err := r.vms.Stat(ctx, opts.positional[0])
	if err != nil {
		return r.fail(sess, err)
	}
	if opts.bool("json") {
		return r.writeJSON(sess, info)
	}
	fmt.Fprintf(sess.Out, "resized %s: cpu=%d memory=%s disk=%s\n",
		info.VM.Name, info.VM.CPU, config.DiskSize(info.VM.Memory).String(), config.DiskSize(info.VM.Disk).String())
	return 0
}

// cmdSSH implements the host side of the `ssh` helper. It runs a command in a
// container from a control session.
func (r *Runner) cmdSSH(ctx context.Context, args []string, sess *IO) int {
	opts, err := parseFlags(args, map[string]flagSpec{
		"l": {kind: flagString},
	})
	if err != nil {
		return r.fail(sess, err)
	}
	pos := opts.positional
	if len(pos) == 0 {
		return r.fail(sess, errors.New("usage: ssh [-l user] [user@]name [command...]"))
	}
	target := pos[0]
	if user := opts.str("l"); user != "" {
		target = user + "@" + target
	}
	user, name := "", target
	if at := strings.LastIndex(target, "@"); at >= 0 {
		user, name = target[:at], target[at+1:]
	}
	if _, ok := r.st.Get(name); !ok {
		return r.fail(sess, fmt.Errorf("container %q not found", name))
	}
	command := pos[1:]
	// A login shell is used for every invocation so that the container's
	// PATH and profile are in effect, exactly like a direct ssh login.
	var argv []string
	if len(command) == 0 {
		argv = wrapUserCommand(user, []string{"/bin/bash", "-l"})
	} else {
		line := strings.Join(quoteAll(command), " ")
		argv = wrapUserCommand(user, []string{"/bin/bash", "-lc", line})
	}
	exec := vm.Exec{
		Argv:   argv,
		Stdin:  sess.In,
		Stdout: sess.Out,
		Stderr: sess.Err,
	}
	code, err := r.vms.Run(ctx, name, exec)
	if err != nil {
		return r.fail(sess, err)
	}
	return code
}

// wrapUserCommand starts command as user inside the container. The daemon
// already runs as root, so setpriv(1) is used instead of a setuid helper.
func wrapUserCommand(user string, command []string) []string {
	if user == "" || user == "root" {
		return command
	}
	argv := []string{"/usr/bin/setpriv", "--reuid", user, "--regid", user, "--init-groups", "--"}
	return append(argv, command...)
}

// displayName renders a container name for a progress message. An empty name
// means that the daemon generates one.
func displayName(name string) string {
	if name == "" {
		return "(generated name)"
	}
	return name
}

// quoteAll quotes every argument for a POSIX shell.
func quoteAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return out
}

func parseEnv(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	env := map[string]string{}
	for _, v := range values {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --env %q: use KEY=VALUE", v)
		}
		env[k] = val
	}
	return env, nil
}

func (r *Runner) parseCPU(value string) (int, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	n, err := parseCount(value)
	if err != nil {
		return 0, fmt.Errorf("invalid --cpu %q: %w", value, err)
	}
	if n <= 0 || n > 256 {
		return 0, fmt.Errorf("invalid --cpu %q: must be between 1 and 256", value)
	}
	return n, nil
}

func (r *Runner) parseSize(value, what string) (int64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	size, err := config.ParseDiskSize(value)
	if err != nil {
		return 0, fmt.Errorf("invalid --%s %q: %w", what, value, err)
	}
	if size <= 0 {
		return 0, fmt.Errorf("invalid --%s %q: must be positive", what, value)
	}
	return int64(size), nil
}

// runScript feeds a shell script to the container through its stdin.
func (r *Runner) runScript(ctx context.Context, name, script string) error {
	f, err := os.CreateTemp("", "sandboxxing-script-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return err
	}
	exit, err := r.vms.Run(ctx, name, vm.Exec{
		Argv:  []string{"/bin/bash", "-s"},
		Stdin: io.Reader(f),
	})
	f.Close()
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("script exited with status %d", exit)
	}
	return nil
}
