package config

import (
	"strings"
	"testing"
	"time"
)

func validMicroVM() *MicroVMConfig {
	return &MicroVMConfig{
		Images:                  map[string]MicroVMImage{"al2023": {ARN: "arn:aws:lambda:us-east-1:123456789012:microvm-image:sbx"}},
		EgressNetworkConnectors: []string{"arn:aws:lambda:us-east-1:123456789012:network-connector:nc-1"},
		FileSystemID:            "fs-1",
		PermissionsBoundary:     "arn:aws:iam::123456789012:policy/boundary",
	}
}

func TestMicroVMDefaults(t *testing.T) {
	cfg := Default()
	cfg.MicroVM = validMicroVM()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	mv := cfg.MicroVM
	if mv.DefaultImage != "al2023" {
		t.Errorf("default_image = %q, want the only image", mv.DefaultImage)
	}
	if mv.MaximumDuration.Duration() != 8*time.Hour {
		t.Errorf("maximum_duration = %s, want 8h", mv.MaximumDuration.Duration())
	}
	if mv.HomeRoot != "/homes" || mv.RolePath != "/sandboxxing/instances/" {
		t.Errorf("efs_home_root = %q, role_path = %q", mv.HomeRoot, mv.RolePath)
	}
	if cfg.DefaultBackend != "nspawn" {
		t.Errorf("default_backend = %q, want nspawn", cfg.DefaultBackend)
	}
}

func TestMicroVMValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Config)
		want string
	}{
		"no images":       {func(c *Config) { c.MicroVM.Images = nil }, "microvm.images"},
		"no file system":  {func(c *Config) { c.MicroVM.FileSystemID = "" }, "efs_file_system_id"},
		"no connector":    {func(c *Config) { c.MicroVM.EgressNetworkConnectors = nil }, "egress_network_connectors"},
		"no boundary":     {func(c *Config) { c.MicroVM.PermissionsBoundary = "" }, "permissions_boundary_arn"},
		"too long":        {func(c *Config) { c.MicroVM.MaximumDuration = Duration(9 * time.Hour) }, "maximum_duration"},
		"bad role path":   {func(c *Config) { c.MicroVM.RolePath = "/x" }, "role_path"},
		"unknown default": {func(c *Config) { c.MicroVM.DefaultImage = "missing" }, "default_image"},
		"backend without section": {func(c *Config) {
			c.MicroVM = nil
			c.DefaultBackend = "microvm"
		}, "microvm section"},
		"unknown backend": {func(c *Config) { c.DefaultBackend = "qemu" }, "default_backend"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			cfg.MicroVM = validMicroVM()
			tc.edit(cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %v, want an error about %s", err, tc.want)
			}
		})
	}
}
