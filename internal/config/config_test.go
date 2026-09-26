package config

import "testing"

func TestParseDiskSize(t *testing.T) {
	cases := map[string]int64{
		"20":    20,
		"20GB":  20 << 30,
		"50G":   50 << 30,
		"512M":  512 << 20,
		"1GIB":  1 << 30,
		"4g":    4 << 30,
		"1024K": 1024 << 10,
	}
	for input, want := range cases {
		got, err := ParseDiskSize(input)
		if err != nil {
			t.Fatalf("ParseDiskSize(%q): %v", input, err)
		}
		if int64(got) != want {
			t.Errorf("ParseDiskSize(%q) = %d, want %d", input, got, want)
		}
	}
	for _, bad := range []string{"", "-5G", "abc"} {
		if _, err := ParseDiskSize(bad); err == nil {
			t.Errorf("ParseDiskSize(%q) should fail", bad)
		}
	}
}

func TestDiskSizeString(t *testing.T) {
	cases := map[int64]string{
		0:          "0",
		2 << 30:    "2G",
		1536 << 20: "1536M",
		3 << 30:    "3G",
		1024 << 10: "1M",
	}
	for in, want := range cases {
		if got := DiskSize(in).String(); got != want {
			t.Errorf("DiskSize(%d).String() = %q, want %q", in, got, want)
		}
	}
}

func TestValidateFillsDefaults(t *testing.T) {
	c := &Config{DataDir: "/tmp/sbx", SSHAddr: ":0", AdminUsers: []string{"sandbox"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.ImageDir == "" || c.StateFile == "" || c.PasswordFile == "" {
		t.Error("derived paths were not filled in")
	}
	if c.Bridge == "" || c.Subnet == "" {
		t.Error("network defaults were not filled in")
	}
	if c.DefaultCPU <= 0 || c.DefaultMemory <= 0 || c.DefaultDisk <= 0 {
		t.Error("resource defaults were not filled in")
	}
}

func TestIsAdminUser(t *testing.T) {
	c := Default()
	if !c.IsAdminUser("sandbox") {
		t.Error("sandbox should be a control user")
	}
	if c.IsAdminUser("demo") {
		t.Error("demo must be treated as a container name")
	}
}
