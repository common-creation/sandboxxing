package state

import (
	"net"
	"path/filepath"
	"testing"
)

func TestAddAllocatesNamesAndIPs(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetSubnet(parseIP(t, "10.100.0.1"))

	first := &VM{Name: "one", Image: "arch", CPU: 2, Memory: 1 << 30, Disk: 10 << 30}
	if err := s.Add(first); err != nil {
		t.Fatal(err)
	}
	second := &VM{Name: "two", Image: "arch", CPU: 2, Memory: 1 << 30, Disk: 10 << 30}
	if err := s.Add(second); err != nil {
		t.Fatal(err)
	}
	if first.IP != "10.100.0.2" {
		t.Errorf("first IP = %q, want 10.100.0.2", first.IP)
	}
	if second.IP != "10.100.0.3" {
		t.Errorf("second IP = %q, want 10.100.0.3", second.IP)
	}
	if first.MachineID == "" || first.MachineID == second.MachineID {
		t.Error("machine IDs must be unique and non-empty")
	}

	reloaded, err := Open(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.List()) != 2 {
		t.Errorf("reloaded %d containers, want 2", len(reloaded.List()))
	}
}

func TestAddRejectsDuplicate(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetSubnet(parseIP(t, "10.100.0.1"))
	vm := &VM{Name: "dup", Image: "arch", CPU: 1, Memory: 1, Disk: 1}
	if err := s.Add(vm); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(&VM{Name: "dup", Image: "arch", CPU: 1, Memory: 1, Disk: 1}); err == nil {
		t.Error("adding the same name twice must fail")
	}
}

func TestRemoveAndUpdate(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetSubnet(parseIP(t, "10.100.0.1"))
	vm := &VM{Name: "demo", Image: "arch", CPU: 1, Memory: 1 << 30, Disk: 1 << 30}
	if err := s.Add(vm); err != nil {
		t.Fatal(err)
	}
	if err := s.Update("demo", func(v *VM) error { v.CPU = 8; return nil }); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("demo")
	if got.CPU != 8 {
		t.Errorf("CPU = %d, want 8", got.CPU)
	}
	if err := s.Remove("demo"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("demo"); ok {
		t.Error("container should be gone")
	}
	if err := s.Remove("demo"); err == nil {
		t.Error("removing a missing container must fail")
	}
}

func TestValidName(t *testing.T) {
	for _, name := range []string{"demo", "sbx-abc123", "web_1", "web-2"} {
		if !ValidName(name) {
			t.Errorf("%q should be valid", name)
		}
	}
	for _, name := range []string{"", "-lead", ".dot", "a.b", "with space", "ümlaut", "a/b", "way-too-long-name"} {
		if ValidName(name) {
			t.Errorf("%q should be rejected", name)
		}
	}
}

func TestStripDomain(t *testing.T) {
	cases := map[string]string{
		"demo":             "demo",
		"demo.example.com": "demo",
		"web-1.sbx":        "web-1",
		"sbx-abc":          "sbx-abc",
	}
	for in, want := range cases {
		if got := StripDomain(in); got != want {
			t.Errorf("StripDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAllocIPWrapsAndSkipsUsed(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetSubnet(parseIP(t, "192.168.7.1"))
	if err := s.Add(&VM{Name: "a", Image: "arch", CPU: 1, Memory: 1, Disk: 1, IP: "192.168.7.2"}); err != nil {
		t.Fatal(err)
	}
	s.NextIP = 2
	ip, err := s.AllocIP()
	if err != nil {
		t.Fatal(err)
	}
	if ip != "192.168.7.3" {
		t.Errorf("AllocIP = %q, want 192.168.7.3", ip)
	}
}

func parseIP(t *testing.T, s string) (ip net.IP) {
	t.Helper()
	ip = net.ParseIP(s)
	if ip == nil {
		t.Fatalf("invalid IP %q", s)
	}
	return ip
}
