package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// TestPasswordNullDisablesAuth verifies that "password": null is distinct
// from an absent or empty value. null turns password authentication off, while
// an empty value still makes the daemon generate a password.
func TestPasswordNullDisablesAuth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
		"data_dir": "`+dir+`",
		"password": null
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PasswordAuthDisabled {
		t.Error("password authentication should be disabled by null")
	}

	// An empty string must keep the generating behaviour.
	if err := os.WriteFile(path, []byte(`{
		"data_dir": "`+dir+`",
		"password": ""
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PasswordAuthDisabled {
		t.Error("an empty password must not disable password authentication")
	}

	// An absent value behaves like the default.
	if err := os.WriteFile(path, []byte(`{"data_dir": "`+dir+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PasswordAuthDisabled {
		t.Error("an absent password must not disable password authentication")
	}
	if cfg.PasswordFile == "" {
		t.Error("the password file default should still be filled in")
	}
}

// TestAuthorizedKeysConfiguration covers the three shapes of the option:
// absent (default path), null (disabled) and a path (explicit).
func TestAuthorizedKeysConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	write := func(body string) *Config {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	cfg := write(`{"data_dir": "` + dir + `"}`)
	if cfg.AuthorizedKeys == "" || cfg.AuthorizedKeysDisabled || cfg.AuthorizedKeysExplicit {
		t.Errorf("absent authorized_keys should use the default path: %+v", cfg)
	}
	if !strings.HasSuffix(cfg.AuthorizedKeys, "/authorized_keys") {
		t.Errorf("default path = %q", cfg.AuthorizedKeys)
	}

	cfg = write(`{"data_dir": "` + dir + `", "authorized_keys": null}`)
	if !cfg.AuthorizedKeysDisabled {
		t.Error("authorized_keys null should disable public key authentication")
	}

	keyFile := filepath.Join(dir, "keys.txt")
	cfg = write(`{"data_dir": "` + dir + `", "authorized_keys": "` + keyFile + `"}`)
	if cfg.AuthorizedKeys != keyFile || !cfg.AuthorizedKeysExplicit {
		t.Errorf("explicit path was not honoured: %+v", cfg)
	}

	// The old name is still accepted.
	cfg = write(`{"data_dir": "` + dir + `", "authorized_keys_file": "` + keyFile + `"}`)
	if cfg.AuthorizedKeys != keyFile || !cfg.AuthorizedKeysExplicit {
		t.Errorf("authorized_keys_file was not honoured: %+v", cfg)
	}
}

// TestBothAuthMethodsDisabledIsRejected prevents a configuration that locks
// everyone out.
func TestBothAuthMethodsDisabledIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
		"data_dir": "`+dir+`",
		"password": null,
		"authorized_keys": null
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("disabling both authentication methods must be rejected")
	}
}
