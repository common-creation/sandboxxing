// Package host manages the host side resources needed to run containers:
// the container bridge, IPv4 forwarding and NAT rules.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/common-creation/sandboxxing/internal/config"
)

// Manager prepares and inspects host resources.
type Manager struct {
	cfg *config.Config
	log *slog.Logger
}

// New returns a host manager for the configuration.
func New(cfg *config.Config, log *slog.Logger) *Manager {
	return &Manager{cfg: cfg, log: log}
}

// ErrNotRoot is returned when a privileged operation is attempted as a user.
var ErrNotRoot = errors.New("sandboxxing requires root privileges to manage systemd-nspawn containers")

// IsRoot reports whether the process runs as root.
func IsRoot() bool { return os.Geteuid() == 0 }

// Ensure creates the bridge and installs the forwarding rules. It is
// idempotent and safe to call on every start.
func (m *Manager) Ensure(ctx context.Context) error {
	if !IsRoot() {
		return ErrNotRoot
	}
	if err := m.ensureBridge(ctx); err != nil {
		return err
	}
	if err := m.ensureForwarding(); err != nil {
		return err
	}
	return m.ensureNAT(ctx)
}

// BridgeAddr returns the address assigned to the container bridge.
func (m *Manager) BridgeAddr() net.IP { return m.cfg.Addr() }

// ensureBridge creates the container bridge, brings it up and assigns it the
// gateway address. An address previously assigned from a wrong subnet (the
// network address of the configured subnet, which older versions of
// sandboxxing used) is removed so that containers can reach their gateway.
func (m *Manager) ensureBridge(ctx context.Context) error {
	if _, err := net.InterfaceByName(m.cfg.Bridge); err != nil {
		// The interface does not exist yet, so create it. Any other lookup
		// problem (a name that is too long, for example) surfaces here as a
		// failure of the create command.
		out, err := m.run(ctx, "ip", "link", "add", "name", m.cfg.Bridge, "type", "bridge")
		if err != nil {
			return fmt.Errorf("create bridge %s: %w: %s", m.cfg.Bridge, err, out)
		}
		m.log.Info("created container bridge", "bridge", m.cfg.Bridge)
	}
	// The link must be up before an address can be used. A freshly created
	// bridge is down by default, so this runs every time.
	if _, err := m.run(ctx, "ip", "link", "set", m.cfg.Bridge, "up"); err != nil {
		return fmt.Errorf("bring up bridge %s: %w", m.cfg.Bridge, err)
	}

	prefix, _ := m.cfg.Mask().Size()
	want := fmt.Sprintf("%s/%d", m.cfg.Addr(), prefix)

	current, err := m.addresses(ctx)
	if err != nil {
		return err
	}
	// Remove addresses left over from an older configuration, even when the
	// correct one is already present: two addresses in the same subnet would
	// make the kernel pick an arbitrary source address. Only addresses inside
	// the configured subnet are touched.
	for addr := range current {
		if addr == want {
			continue
		}
		if !m.isStaleAddress(addr, want) {
			m.log.Warn("bridge already has an address outside the configured subnet",
				"bridge", m.cfg.Bridge, "address", addr, "subnet", m.cfg.Subnet)
			continue
		}
		m.log.Info("replacing stale bridge address", "bridge", m.cfg.Bridge, "address", addr)
		if _, err := m.run(ctx, "ip", "addr", "del", addr, "dev", m.cfg.Bridge); err != nil {
			return fmt.Errorf("remove stale address %s from %s: %w", addr, m.cfg.Bridge, err)
		}
	}
	if current[want] {
		return nil
	}

	if _, err := m.run(ctx, "ip", "addr", "add", want, "dev", m.cfg.Bridge); err != nil {
		return fmt.Errorf("assign %s to bridge %s: %w", want, m.cfg.Bridge, err)
	}
	m.log.Info("assigned address to bridge", "bridge", m.cfg.Bridge, "address", want)
	return nil
}

// isStaleAddress reports whether address is inside the configured subnet but
// is not the requested bridge address. Such an address was assigned by an
// older version of sandboxxing and must be replaced, otherwise containers
// cannot reach the real gateway.
func (m *Manager) isStaleAddress(address, want string) bool {
	ip, _, err := net.ParseCIDR(address)
	if err != nil {
		return false
	}
	wantIP, _, err := net.ParseCIDR(want)
	if err != nil {
		return false
	}
	if ip.Equal(wantIP) {
		return false
	}
	_, configured, err := net.ParseCIDR(m.cfg.Subnet)
	if err != nil {
		return false
	}
	return configured.Contains(ip)
}

// addresses returns the IPv4 addresses currently assigned to the bridge, as a
// set keyed by the CIDR notation.
func (m *Manager) addresses(ctx context.Context) (map[string]bool, error) {
	out, err := m.run(ctx, "ip", "-o", "-4", "addr", "show", "dev", m.cfg.Bridge)
	if err != nil {
		return nil, fmt.Errorf("inspect bridge %s: %w", m.cfg.Bridge, err)
	}
	return parseAddresses(out), nil
}

// parseAddresses extracts the CIDR notations from `ip -o -4 addr show`
// output. The one-line format looks like:
//
//	187: sbx0    inet 10.100.0.0/16 scope global sbx0
func parseAddresses(out string) map[string]bool {
	addrs := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "inet" && i+1 < len(fields) {
				addrs[fields[i+1]] = true
			}
		}
	}
	return addrs
}

func (m *Manager) ensureForwarding() error {
	const path = "/proc/sys/net/ipv4/ip_forward"
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if strings.TrimSpace(string(b)) == "1" {
		return nil
	}
	if err := os.WriteFile(path, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enable IPv4 forwarding: %w", err)
	}
	// Make the setting survive a reboot as well.
	const sysctl = "/etc/sysctl.d/99-sandboxxing.conf"
	if _, err := os.Stat(sysctl); err != nil {
		if err := os.WriteFile(sysctl, []byte("net.ipv4.ip_forward = 1\n"), 0o644); err != nil {
			m.log.Warn("could not persist IPv4 forwarding", "path", sysctl, "error", err)
		}
	}
	m.log.Info("enabled IPv4 forwarding")
	return nil
}

// ensureNAT installs the masquerade and forwarding rules for the container
// subnet. The table is replaced on every start so that configuration changes
// take effect. Deleting a table that does not exist yet is expected and is
// therefore attempted separately.
func (m *Manager) ensureNAT(ctx context.Context) error {
	if _, err := exec.LookPath("nft"); err != nil {
		return errors.New("nft not found: install nftables")
	}
	if _, err := m.run(ctx, "nft", "delete", "table", "ip", "sandboxxing"); err != nil {
		// A missing table is the normal case on the first run. Only report a
		// failure when the table really is still there.
		if out, listErr := m.run(ctx, "nft", "list", "table", "ip", "sandboxxing"); listErr == nil {
			return fmt.Errorf("could not replace the NAT rules: %w: %s", err, out)
		}
	}

	script := fmt.Sprintf(`table ip sandboxxing {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr %s oifname != "%s" masquerade
	}
	chain forward {
		type filter hook forward priority filter; policy accept;
		ip saddr %s accept
		ip daddr %s accept
	}
}
`, m.cfg.Subnet, m.cfg.Bridge, m.cfg.Subnet, m.cfg.Subnet)

	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("install NAT rules for %s: %v: %s", m.cfg.Subnet, err, strings.TrimSpace(string(out)))
	}

	// A chain in its own table is not enough: docker (and other tools) put a
	// "policy drop" on the FORWARD chain of the filter table, which applies
	// to every forwarded packet regardless of other tables. The container
	// traffic therefore needs an explicit accept there.
	if err := m.allowForwarding(ctx); err != nil {
		return err
	}
	m.log.Info("installed NAT rules", "subnet", m.cfg.Subnet, "bridge", m.cfg.Bridge)
	return nil
}

// forwardComment tags the rules that sandboxxing owns inside the forward
// chains. Other software manages the same chains and its rules must be kept.
const forwardComment = `"sandboxxing"`

// forwardChain identifies one base chain that is attached to the forward hook.
type forwardChain struct {
	Family string
	Table  string
	Name   string
	Policy string
}

// allowForwarding adds accept rules for the container subnet to every chain
// that is attached to the forward hook. A chain of its own is not enough:
// nftables evaluates every hooked chain, and an "accept" verdict is not final
// when a later chain drops the packet. Both the host firewall
// (/etc/nftables.conf uses the lowercase "forward" chain of "inet filter")
// and docker (which uses the uppercase "FORWARD" chain of "ip filter" through
// iptables-nft) therefore have to be considered.
func (m *Manager) allowForwarding(ctx context.Context) error {
	m.cleanupStrayChains(ctx)
	chains, err := m.forwardChains(ctx)
	if err != nil {
		return err
	}
	if len(chains) == 0 {
		// No firewall is managing the forward hook, so nothing can drop the
		// container traffic. The masquerade rule from the sandboxxing table
		// is enough.
		m.log.Debug("no forward chain found; no accept rules needed")
		return nil
	}
	// Only chains that drop traffic need the accept rules. Installing them in
	// an accepting chain is pointless and must not fail the whole host setup.
	var errs []error
	for _, chain := range chains {
		if chain.Policy != "drop" && !m.chainHasDropRule(ctx, chain) {
			continue
		}
		if err := m.allowForwardingIn(ctx, chain); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// chainHasDropRule reports whether a chain contains an explicit drop or
// reject verdict, which is what filters container traffic in a chain whose
// policy is accept.
func (m *Manager) chainHasDropRule(ctx context.Context, chain forwardChain) bool {
	args := append([]string{"-a", "list", "chain"}, chain.Family, chain.Table, chain.Name)
	out, err := m.run(ctx, "nft", args...)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		// Strip the trailing handle comment before looking at the verdict.
		if i := strings.Index(line, "# handle"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		for i, f := range fields {
			if (f == "drop" || f == "reject") && i > 0 {
				return true
			}
		}
	}
	return false
}

// cleanupStrayChains removes the empty "forward" chains that an earlier
// version of sandboxxing created in the filter tables. Those chains are not
// attached to any hook, so the rules inside them never run.
func (m *Manager) cleanupStrayChains(ctx context.Context) {
	hooked := map[string]bool{}
	chains, err := m.forwardChains(ctx)
	if err != nil {
		return
	}
	for _, c := range chains {
		hooked[c.Family+"/"+c.Table+"/"+c.Name] = true
	}
	for _, ref := range [][]string{
		{"ip", "filter", "forward"},
		{"inet", "filter", "forward"},
	} {
		if hooked[strings.Join(ref, "/")] {
			continue
		}
		out, err := m.run(ctx, "nft", "-a", "list", "chain", ref[0], ref[1], ref[2])
		if err != nil || !strings.Contains(out, forwardComment) {
			continue
		}
		if _, err := m.run(ctx, "nft", "delete", "chain", ref[0], ref[1], ref[2]); err != nil {
			m.log.Warn("could not remove a stray forward chain from an earlier version",
				"family", ref[0], "table", ref[1], "chain", ref[2], "error", err)
			continue
		}
		m.log.Info("removed a stray forward chain from an earlier version",
			"family", ref[0], "table", ref[1], "chain", ref[2])
	}
}

// allowForwardingIn installs the accept rules in one chain.
func (m *Manager) allowForwardingIn(ctx context.Context, chain forwardChain) error {
	ref := []string{chain.Family, chain.Table, chain.Name}
	if err := m.dropForwardingRules(ctx, ref); err != nil {
		return err
	}
	for _, expr := range []string{
		fmt.Sprintf("ip saddr %s accept", m.cfg.Subnet),
		fmt.Sprintf("ip daddr %s accept", m.cfg.Subnet),
	} {
		args := append([]string{"nft", "add", "rule"}, ref...)
		args = append(args, strings.Fields(expr)...)
		args = append(args, "comment", forwardComment)
		if _, err := m.run(ctx, args[0], args[1:]...); err != nil {
			return fmt.Errorf("allow forwarding for %s in %s %s %s: %w",
				m.cfg.Subnet, chain.Family, chain.Table, chain.Name, err)
		}
	}
	m.log.Info("allowed container forwarding",
		"family", chain.Family, "table", chain.Table, "chain", chain.Name, "policy", chain.Policy)
	return nil
}

// forwardChains lists the base chains attached to the forward hook that may
// drop container traffic. The JSON output is used because it carries the hook
// type, which cannot be read reliably from the human readable listing.
//
// Chains that cannot carry an IPv4 rule are skipped: the ip6 family rejects
// "ip saddr" outright, and the bridge family filters Ethernet frames rather
// than IP packets. sandboxxing's own table is skipped as well, because its
// forward chain already accepts the container subnet and needs no duplicate.
func (m *Manager) forwardChains(ctx context.Context) ([]forwardChain, error) {
	out, err := m.run(ctx, "nft", "-j", "list", "ruleset")
	if err != nil {
		return nil, fmt.Errorf("inspect the nftables ruleset: %w", err)
	}
	var ruleset struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal([]byte(out), &ruleset); err != nil {
		return nil, fmt.Errorf("parse the nftables ruleset: %w", err)
	}
	var chains []forwardChain
	for _, entry := range ruleset.Nftables {
		raw, ok := entry["chain"]
		if !ok {
			continue
		}
		var chain struct {
			Family string `json:"family"`
			Table  string `json:"table"`
			Name   string `json:"name"`
			Hook   string `json:"hook"`
			Policy string `json:"policy"`
		}
		if err := json.Unmarshal(raw, &chain); err != nil {
			continue
		}
		if chain.Hook != "forward" {
			continue
		}
		if !familyAcceptsIPv4(chain.Family) {
			continue
		}
		if chain.Table == sandboxxingTable && chain.Family == "ip" {
			continue
		}
		chains = append(chains, forwardChain{
			Family: chain.Family, Table: chain.Table, Name: chain.Name, Policy: chain.Policy,
		})
	}
	return chains, nil
}

// sandboxxingTable is the table that holds the masquerade and forward rules
// installed by ensureNAT.
const sandboxxingTable = "sandboxxing"

// familyAcceptsIPv4 reports whether an nftables family can match IPv4
// addresses. The ip6 family cannot, and the bridge family works on Ethernet
// frames, so an "ip saddr" rule would be rejected in both.
func familyAcceptsIPv4(family string) bool {
	return family == "ip" || family == "inet"
}

// dropForwardingRules removes the rules previously added by allowForwarding
// from one chain, so that a changed subnet takes effect.
func (m *Manager) dropForwardingRules(ctx context.Context, ref []string) error {
	args := append([]string{"-a", "list", "chain"}, ref...)
	out, err := m.run(ctx, "nft", args...)
	if err != nil {
		// The chain disappeared, so there is nothing to remove.
		return nil
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, forwardComment) {
			continue
		}
		handle := handleOf(line)
		if handle == "" {
			continue
		}
		del := append([]string{"delete", "rule"}, ref...)
		del = append(del, "handle", handle)
		if _, err := m.run(ctx, "nft", del...); err != nil {
			return fmt.Errorf("remove the previous forwarding rule %s: %w", handle, err)
		}
	}
	return nil
}

// handleOf extracts the rule handle from a line of `nft -a list` output.
func handleOf(line string) string {
	const marker = "# handle "
	i := strings.LastIndex(line, marker)
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(line[i+len(marker):])
}

func (m *Manager) run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, text)
		}
		return text, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return text, nil
}

// Checkable is one host prerequisite.
type Checkable struct {
	Name     string
	OK       bool
	Detail   string
	Critical bool
}

// Check verifies the host prerequisites and returns one entry per check.
func (m *Manager) Check(ctx context.Context) []Checkable {
	check := func(name string, critical bool, fn func() (bool, string)) Checkable {
		ok, detail := fn()
		return Checkable{Name: name, OK: ok, Detail: detail, Critical: critical}
	}

	var checks []Checkable
	checks = append(checks, check("root", true, func() (bool, string) {
		if IsRoot() {
			return true, "running as root"
		}
		return false, "must run as root"
	}))
	for _, tool := range []struct {
		name string
		pkg  string
	}{
		{"systemd-nspawn", "systemd"},
		{"machinectl", "systemd"},
		{"systemctl", "systemd"},
		{"pacstrap", "arch-install-scripts"},
		{"pacman-key", "pacman"},
		{"gpg", "gnupg"},
		{"mke2fs", "e2fsprogs"},
		{"e2fsck", "e2fsprogs"},
		{"resize2fs", "e2fsprogs"},
		{"nft", "nftables"},
		{"ip", "iproute2"},
		{"nsenter", "util-linux"},
		{"setpriv", "util-linux"},
		{"cp", "coreutils"},
	} {
		tool := tool
		checks = append(checks, check(tool.name, true, func() (bool, string) {
			path, err := exec.LookPath(tool.name)
			if err != nil {
				return false, "missing, install " + tool.pkg
			}
			return true, path
		}))
	}
	checks = append(checks, check("machined", true, func() (bool, string) {
		if _, err := os.Stat("/run/dbus/system_bus_socket"); err != nil {
			return false, "system D-Bus socket not found; is systemd running?"
		}
		out, err := exec.CommandContext(ctx, "systemctl", "is-active", "systemd-machined.service").Output()
		state := strings.TrimSpace(string(out))
		if err == nil || state == "active" {
			return true, state
		}
		return true, state + " (started on demand)"
	}))
	checks = append(checks, check("ip_forward", true, func() (bool, string) {
		b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
		if err != nil {
			return false, err.Error()
		}
		return strings.TrimSpace(string(b)) == "1", "net.ipv4.ip_forward=" + strings.TrimSpace(string(b))
	}))
	checks = append(checks, check("bridge "+m.cfg.Bridge, true, func() (bool, string) {
		iface, err := net.InterfaceByName(m.cfg.Bridge)
		if err != nil {
			return false, "not created yet (will be created on startup)"
		}
		state := "up"
		if iface.Flags&net.FlagUp == 0 {
			state = "down"
		}
		return state == "up", "present, state " + state
	}))
	checks = append(checks, check("gateway "+m.cfg.Addr().String(), true, func() (bool, string) {
		prefix, _ := m.cfg.Mask().Size()
		want := fmt.Sprintf("%s/%d", m.cfg.Addr(), prefix)
		out, err := m.run(ctx, "ip", "-o", "-4", "addr", "show", "dev", m.cfg.Bridge)
		if err != nil {
			return false, "cannot inspect the bridge (is it created?)"
		}
		addrs := parseAddresses(out)
		if !addrs[want] {
			return false, fmt.Sprintf("bridge does not have %s; restart the service to apply it", want)
		}
		for addr := range addrs {
			if addr != want && m.isStaleAddress(addr, want) {
				return false, fmt.Sprintf("bridge also has the stale address %s; restart the service to replace it", addr)
			}
		}
		return true, want
	}))
	checks = append(checks, check("forwarding", true, func() (bool, string) {
		chains, err := m.forwardChains(ctx)
		if err != nil {
			return false, err.Error()
		}
		var problems []string
		for _, chain := range chains {
			if chain.Policy != "drop" {
				continue
			}
			args := []string{"-a", "list", "chain", chain.Family, chain.Table, chain.Name}
			out, err := m.run(ctx, "nft", args...)
			if err != nil || !strings.Contains(out, forwardComment) {
				problems = append(problems, fmt.Sprintf("%s %s %s drops forwarded packets",
					chain.Family, chain.Table, chain.Name))
			}
		}
		if len(problems) > 0 {
			return false, strings.Join(problems, "; ") + " (restart the service to add the accept rules)"
		}
		if len(chains) == 0 {
			return true, "no forward chain is installed"
		}
		names := make([]string, 0, len(chains))
		for _, c := range chains {
			names = append(names, c.Family+" "+c.Table+" "+c.Name)
		}
		return true, "accept rules present in " + strings.Join(names, ", ")
	}))
	for _, dir := range []string{m.cfg.DataDir, m.cfg.ImageDir} {
		dir := dir
		checks = append(checks, check("dir "+dir, true, func() (bool, string) {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return false, err.Error()
			}
			return true, "ok"
		}))
	}
	if _, err := os.Stat("/dev/loop-control"); err != nil {
		checks = append(checks, Checkable{Name: "loop devices", OK: false, Detail: "/dev/loop-control missing", Critical: true})
	} else {
		checks = append(checks, Checkable{Name: "loop devices", OK: true, Detail: "/dev/loop-control present", Critical: true})
	}
	return checks
}
