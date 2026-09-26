package host

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/common-creation/sandboxxing/internal/config"
)

// TestIsStaleAddress guards the migration from the bad gateway address that
// made containers unable to reach the network: the network address of the
// subnet was used instead of the first usable address.
func TestIsStaleAddress(t *testing.T) {
	m := &Manager{cfg: &config.Config{Subnet: "10.100.0.0/16"}}

	cases := []struct {
		address string
		want    string
		stale   bool
	}{
		{"10.100.0.0/16", "10.100.0.1/16", true},   // the buggy gateway
		{"10.100.0.1/16", "10.100.0.1/16", false},  // the correct gateway
		{"10.100.5.7/16", "10.100.0.1/16", true},   // another leftover address
		{"192.168.1.1/24", "10.100.0.1/16", false}, // outside the subnet
		{"garbage", "10.100.0.1/16", false},
		{"10.100.0.1/16", "garbage", false}, // an unparsable want changes nothing
	}
	for _, c := range cases {
		if got := m.isStaleAddress(c.address, c.want); got != c.stale {
			t.Errorf("isStaleAddress(%q, %q) = %v, want %v", c.address, c.want, got, c.stale)
		}
	}
}

// TestBridgeAddressIsWithinSubnet documents why stale detection can rely on
// subnet membership: the corrected gateway is always inside the subnet, so
// any other address inside it is a leftover.
func TestBridgeAddressIsWithinSubnet(t *testing.T) {
	cfg := config.Default()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	network, err := parseCIDR(cfg.Subnet)
	if err != nil {
		t.Fatal(err)
	}
	if !network.Contains(cfg.Addr()) {
		t.Errorf("gateway %s is not inside %s", cfg.Addr(), cfg.Subnet)
	}
}

func parseCIDR(s string) (*net.IPNet, error) {
	_, n, err := net.ParseCIDR(s)
	return n, err
}

// TestParseAddresses pins the parsing of `ip -o -4 addr show` output, which
// the bridge migration depends on.
func TestParseAddresses(t *testing.T) {
	out := "187: sbx0    inet 10.100.0.0/16 scope global sbx0\\       valid_lft forever preferred_lft forever\n" +
		"187: sbx0    inet 10.100.0.1/16 scope global secondary sbx0\n" +
		"188: eth0    inet 192.168.1.5/24 scope global eth0\n"

	got := parseAddresses(out)
	want := map[string]bool{
		"10.100.0.0/16":  true,
		"10.100.0.1/16":  true,
		"192.168.1.5/24": true,
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for addr := range want {
		if !got[addr] {
			t.Errorf("address %s was not parsed: %v", addr, got)
		}
	}
	if len(parseAddresses("")) != 0 {
		t.Error("empty output must yield no addresses")
	}
}

// TestAddressCommandLine runs the command through a fake ip(8) that records
// its arguments. A missing dash in the address family option ("ip -o 4"
// instead of "ip -o -4") makes ip fail, and because EnsureHost only logs a
// warning the bridge migration silently stops working.
func TestAddressCommandLine(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "args.log")
	fake := filepath.Join(dir, "ip")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + logPath + "\n" +
		"printf '1: sbx0    inet 10.100.0.1/16 scope global sbx0\\n'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m := &Manager{cfg: &config.Config{Bridge: "sbx0", Subnet: "10.100.0.0/16"}}
	addrs, err := m.addresses(context.Background())
	if err != nil {
		t.Fatalf("addresses: %v", err)
	}
	if !addrs["10.100.0.1/16"] {
		t.Errorf("parsed addresses = %v", addrs)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(raw)), "\n")
	want := []string{"-o", "-4", "addr", "show", "dev", "sbx0"}
	if len(got) != len(want) {
		t.Fatalf("ip called with %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argument %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestForwardChainsParse verifies that chains attached to the forward hook are
// found regardless of the case of their name: the Arch Linux firewall uses
// "forward" in "inet filter" while docker/iptables-nft uses "FORWARD" in
// "ip filter". Missing one of them leaves the container traffic dropped.
func TestForwardChainsParse(t *testing.T) {
	jsonOut := `{"nftables":[{"metainfo":{"version":"1.1.7"}},
		{"table":{"family":"ip","name":"filter"}},
		{"chain":{"family":"ip","table":"filter","name":"FORWARD","hook":"forward","policy":"drop"}},
		{"chain":{"family":"ip","table":"filter","name":"INPUT","hook":"input","policy":"accept"}},
		{"table":{"family":"inet","name":"filter"}},
		{"chain":{"family":"inet","table":"filter","name":"forward","hook":"forward","policy":"drop"}}]}`

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nft"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The JSON is provided by a script that ignores its arguments.
	script := "#!/bin/sh\ncat <<'JSON'\n" + jsonOut + "\nJSON\n"
	if err := os.WriteFile(filepath.Join(dir, "nft"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m := &Manager{cfg: &config.Config{Subnet: "10.100.0.0/16"}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	chains, err := m.forwardChains(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(chains) != 2 {
		t.Fatalf("found %d forward chains, want 2: %+v", len(chains), chains)
	}
	found := map[string]bool{}
	for _, c := range chains {
		found[c.Family+"/"+c.Table+"/"+c.Name] = true
	}
	for _, want := range []string{"ip/filter/FORWARD", "inet/filter/forward"} {
		if !found[want] {
			t.Errorf("chain %s was not detected: %v", want, found)
		}
	}
}

// TestHandleOf pins the parsing of `nft -a list` output, which is how the
// previously installed forwarding rules are found and replaced.
func TestHandleOf(t *testing.T) {
	cases := map[string]string{
		"		ip saddr 10.100.0.0/16 accept comment \"sandboxxing\" # handle 3": "3",
		"		ip daddr 10.100.0.0/16 accept # handle 42":                        "42",
		"		type filter hook forward priority filter; policy drop;":           "",
		"": "",
	}
	for line, want := range cases {
		if got := handleOf(line); got != want {
			t.Errorf("handleOf(%q) = %q, want %q", line, got, want)
		}
	}
}

// TestForwardChainsSkipsUnusableFamilies covers the failure that aborted the
// host setup: an IPv4 rule ("ip saddr ...") was added to the ip6 family,
// which nft rejects with "conflicting network layer protocols", and the whole
// preparation stopped.
func TestForwardChainsSkipsUnusableFamilies(t *testing.T) {
	jsonOut := `{"nftables":[{"metainfo":{"version":"1.1.7"}},
		{"chain":{"family":"ip","table":"filter","name":"FORWARD","hook":"forward","policy":"drop"}},
		{"chain":{"family":"inet","table":"filter","name":"forward","hook":"forward","policy":"drop"}},
		{"chain":{"family":"ip6","table":"filter","name":"FORWARD","hook":"forward","policy":"drop"}},
		{"chain":{"family":"bridge","table":"filter","name":"forward","hook":"forward","policy":"accept"}},
		{"chain":{"family":"ip","table":"sandboxxing","name":"forward","hook":"forward","policy":"accept"}}]}`

	dir := t.TempDir()
	script := "#!/bin/sh\ncat <<'JSON'\n" + jsonOut + "\nJSON\n"
	if err := os.WriteFile(filepath.Join(dir, "nft"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m := &Manager{cfg: &config.Config{Subnet: "10.100.0.0/16"}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	chains, err := m.forwardChains(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chains {
		if c.Family == "ip6" {
			t.Errorf("the ip6 family cannot carry an IPv4 rule: %+v", c)
		}
		if c.Family == "bridge" {
			t.Errorf("the bridge family filters frames, not IP packets: %+v", c)
		}
		if c.Table == "sandboxxing" {
			t.Errorf("sandboxxing's own table must not be modified: %+v", c)
		}
	}
	if len(chains) != 2 {
		t.Fatalf("found %d usable chains, want 2: %+v", len(chains), chains)
	}
}
