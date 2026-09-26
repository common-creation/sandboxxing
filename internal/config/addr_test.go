package config

import (
	"net"
	"testing"
)

// TestAddrIsFirstUsableAddress guards the bug that containers were given the
// network address of the subnet as their default gateway, which made every
// outbound packet fail with "Destination Host Unreachable".
func TestAddrIsFirstUsableAddress(t *testing.T) {
	cases := map[string]string{
		"10.100.0.0/16":  "10.100.0.1",
		"10.100.0.0/24":  "10.100.0.1",
		"192.168.7.0/24": "192.168.7.1",
		"172.16.4.0/22":  "172.16.4.1",
	}
	for subnet, want := range cases {
		c := Default()
		c.Subnet = subnet
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate(%s): %v", subnet, err)
		}
		got := c.Addr()
		if got.String() != want {
			t.Errorf("Addr(%s) = %s, want %s", subnet, got, want)
		}
		if _, network, err := net.ParseCIDR(subnet); err == nil && got.Equal(network.IP) {
			t.Errorf("Addr(%s) must not be the network address", subnet)
		}
		if !c.Gateway().Equal(got) {
			t.Errorf("Gateway(%s) = %s, want %s", subnet, c.Gateway(), got)
		}
	}
}

// TestAddrIsInsideSubnet makes sure the gateway is routable for the
// containers, which is what the /16 and /24 prefixes above rely on.
func TestAddrIsInsideSubnet(t *testing.T) {
	c := Default()
	c.Subnet = "10.100.0.0/16"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	_, network, err := net.ParseCIDR(c.Subnet)
	if err != nil {
		t.Fatal(err)
	}
	if !network.Contains(c.Addr()) {
		t.Errorf("gateway %s is outside %s", c.Addr(), c.Subnet)
	}
}
