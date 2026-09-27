// Command sandboxxing runs the sandbox daemon. The daemon speaks the SSH
// protocol on a single port: control users get the management interface and
// every other user name is treated as a container to log into.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/common-creation/sandboxxing/internal/cli"
	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/sshserver"
	"github.com/common-creation/sandboxxing/internal/state"
	"github.com/common-creation/sandboxxing/internal/vm"
)

const version = "0.1.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "sandboxxing: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("sandboxxing", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		configPath  = fs.String("config", config.DefaultPath, "configuration file")
		showVersion = fs.Bool("version", false, "print the version and exit")
		checkOnly   = fs.Bool("check", false, "verify the host prerequisites and exit")
		showPasswd  = fs.Bool("show-password", false, "print the access password and exit")
		logLevel    = fs.String("log-level", "", "override log_level (debug, info, warn, error)")
		dumpConfig  = fs.Bool("dump-config", false, "print the effective configuration and exit")
		cleanup     = fs.Bool("cleanup", false, "remove the host resources (bridge, NAT rules) and exit")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `sandboxxing %s - systemd-nspawn sandboxes over SSH

Usage:
  sandboxxing [options]

Options:
`, version)
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
The daemon listens on the configured ssh_addr. Control users are configured
through admin_users; any other user name is a container name:

  ssh sandbox@host -p 2222 ls
  ssh sandbox@host -p 2222 new --name=demo
  ssh demo@host -p 2222

Run "sandboxxing -check" to verify the host. See README.md for the host setup.
"sandboxxing -cleanup" removes the bridge and firewall rules, for example
before an uninstall. Stop the service first.
`)
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *showVersion {
		fmt.Printf("sandboxxing %s\n", version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *logLevel != "" {
		cfg.LogLevel = *logLevel
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)

	if *dumpConfig {
		b, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}

	st, err := state.Open(cfg.StateFile)
	if err != nil {
		return err
	}
	st.SetSubnet(cfg.Addr())

	// -check only reads the environment, so it must work without root as
	// well: the missing privileges are reported as a failed check.
	if *checkOnly {
		return runChecks(cfg, log, st)
	}
	if *cleanup {
		vms := vm.New(cfg, log, st)
		stopped, err := vms.StopAll(context.Background())
		if err != nil {
			return fmt.Errorf("stop the containers: %w", err)
		}
		if err := vms.CleanupHost(context.Background()); err != nil {
			return err
		}
		fmt.Printf("stopped %d container(s) and removed the host resources (bridge %s)\n", stopped, cfg.Bridge)
		return nil
	}

	lock, err := st.Lock()
	if err != nil {
		return err
	}
	defer lock.Close()

	vms := vm.New(cfg, log, st)

	runner := cli.New(cfg, log, vms, st, advertisedHost(cfg))
	srv, err := sshserver.New(cfg, log, vms, st, runner)
	if err != nil {
		return err
	}
	if *showPasswd {
		if srv.PasswordAuthDisabled() {
			fmt.Fprintln(os.Stderr, "password authentication is disabled by the configuration")
			return nil
		}
		fmt.Println(srv.Password())
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := vms.EnsureHost(ctx); err != nil {
		// Containers keep working, but without the bridge and NAT they have
		// no connectivity. Make the failure loud and point at the diagnostic.
		log.Error("host network preparation failed; containers will not have network access",
			"error", err, "hint", "run 'sandboxxing -check' for details")
	} else {
		log.Info("host network is ready", "bridge", cfg.Bridge, "subnet", cfg.Subnet)
	}
	if err := os.MkdirAll(vms.DataDir(), 0o755); err != nil {
		return err
	}
	// Containers created by an older release still carry the old gateway in
	// their disk image. They are updated and restarted here so that the fix
	// reaches existing sandboxes without recreating them.
	if err := vms.MigrateNetwork(ctx); err != nil {
		log.Warn("could not update the network configuration of existing containers", "error", err)
	}

	log.Info("sandboxxing ready",
		"version", version,
		"address", cfg.SSHAddr,
		"control_users", cfg.AdminUsers,
		"password_auth", !srv.PasswordAuthDisabled(),
		"public_key_auth", srv.PublicKeyAuthEnabled(),
		"authorized_keys", cfg.AuthorizedKeys,
		"data_dir", cfg.DataDir,
	)
	return srv.Serve(ctx)
}

func runChecks(cfg *config.Config, log *slog.Logger, st *state.State) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	vms := vm.New(cfg, log, st)
	failed := 0

	fmt.Printf("%-6s %-18s %s\n", "----", "authentication", "----")
	for _, c := range authChecks(cfg) {
		result := "ok"
		if !c.OK {
			result = "FAIL"
			failed++
		}
		fmt.Printf("%-6s %-18s %s\n", result, c.Name, c.Detail)
	}
	if failed > 0 {
		return fmt.Errorf("%d authentication check(s) failed", failed)
	}

	for _, c := range vms.HostCheck(ctx) {
		result := "ok"
		if !c.OK {
			result = "FAIL"
			if c.Critical {
				failed++
			}
		}
		fmt.Printf("%-6s %-18s %s\n", result, c.Name, c.Detail)
	}
	if failed > 0 {
		return fmt.Errorf("%d prerequisite check(s) failed", failed)
	}
	return nil
}

// hostCheck mirrors the shape of host.Checkable for the local checks.
type hostCheck struct {
	Name   string
	OK     bool
	Detail string
}

// authChecks reports how a client would authenticate, and whether that can
// work at all.
func authChecks(cfg *config.Config) []hostCheck {
	var checks []hostCheck
	switch {
	case cfg.PasswordAuthDisabled:
		checks = append(checks, hostCheck{Name: "password auth", OK: true, Detail: "disabled by the configuration"})
	default:
		checks = append(checks, hostCheck{Name: "password auth", OK: true, Detail: "enabled"})
	}

	switch {
	case cfg.AuthorizedKeysDisabled:
		checks = append(checks, hostCheck{Name: "public key auth", OK: true, Detail: "disabled by the configuration"})
	default:
		b, err := os.ReadFile(cfg.AuthorizedKeys)
		switch {
		case err != nil && os.IsNotExist(err) && !cfg.AuthorizedKeysExplicit:
			checks = append(checks, hostCheck{Name: "public key auth", OK: true,
				Detail: "off, no keys in " + cfg.AuthorizedKeys})
		case err != nil:
			checks = append(checks, hostCheck{Name: "public key auth", OK: false, Detail: err.Error()})
		default:
			keys := 0
			for _, line := range strings.Split(string(b), "\n") {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "#") {
					keys++
				}
			}
			if keys == 0 && cfg.AuthorizedKeysExplicit {
				checks = append(checks, hostCheck{Name: "public key auth", OK: false,
					Detail: "no usable keys in " + cfg.AuthorizedKeys})
				break
			}
			checks = append(checks, hostCheck{Name: "public key auth", OK: true,
				Detail: fmt.Sprintf("%d key(s) in %s", keys, cfg.AuthorizedKeys)})
		}
	}
	if cfg.PasswordAuthDisabled && cfg.AuthorizedKeysDisabled {
		checks = append(checks, hostCheck{Name: "reachability", OK: false,
			Detail: "every authentication method is disabled"})
	}
	checks = append(checks, dnsCheck(cfg))
	checks = append(checks, shareChecks(cfg)...)
	return checks
}

// shareChecks reports whether the configured shared directories exist on the
// host. A missing source directory makes nspawn create a mount point that
// cannot be filled, so it is worth catching before the first container start.
func shareChecks(cfg *config.Config) []hostCheck {
	checks := make([]hostCheck, 0, len(cfg.Shares))
	for _, share := range cfg.Shares {
		info, err := os.Stat(share.Path)
		switch {
		case err != nil:
			checks = append(checks, hostCheck{Name: "share " + share.Name(), OK: false,
				Detail: share.Path + ": " + err.Error()})
		case !info.IsDir():
			checks = append(checks, hostCheck{Name: "share " + share.Name(), OK: false,
				Detail: share.Path + " is not a directory"})
		default:
			mode := "rw"
			if share.ReadOnly {
				mode = "ro"
			}
			checks = append(checks, hostCheck{Name: "share " + share.Name(), OK: true,
				Detail: share.Path + " -> " + share.MountTarget() + " (" + mode + ")"})
		}
	}
	return checks
}

// dnsCheck reports which resolvers the containers will use and warns when the
// host resolver cannot be reached from them, which is the case for the
// systemd-resolved stub on 127.0.0.53.
func dnsCheck(cfg *config.Config) hostCheck {
	if len(cfg.DNS) > 0 {
		return hostCheck{Name: "container dns", OK: true,
			Detail: "from the configuration: " + strings.Join(cfg.DNS, ", ")}
	}
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return hostCheck{Name: "container dns", OK: true,
			Detail: "host resolv.conf unreadable, public resolvers will be used"}
	}
	usable, skipped := splitResolvers(string(b))
	switch {
	case len(usable) > 0:
		return hostCheck{Name: "container dns", OK: true,
			Detail: "from the host: " + strings.Join(usable, ", ")}
	case len(skipped) > 0:
		return hostCheck{Name: "container dns", OK: false,
			Detail: "the host only uses " + strings.Join(skipped, ", ") +
				" which a container cannot reach; set \"dns\" in the configuration"}
	default:
		return hostCheck{Name: "container dns", OK: false,
			Detail: "no nameserver found; set \"dns\" in the configuration"}
	}
}

// splitResolvers separates the nameservers of a resolv.conf into the ones a
// container can reach and the ones that only answer on the host itself, such
// as systemd-resolved's 127.0.0.53 stub.
func splitResolvers(contents string) (usable, skipped []string) {
	for _, line := range strings.Split(contents, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		if ip := net.ParseIP(fields[1]); ip != nil && ip.IsLoopback() {
			skipped = append(skipped, fields[1])
			continue
		}
		usable = append(usable, fields[1])
	}
	return usable, skipped
}

func advertisedHost(cfg *config.Config) string {
	if cfg.Domain != "" {
		return cfg.Domain
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "sandboxxing"
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
