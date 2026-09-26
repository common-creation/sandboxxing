package vm

import (
	"testing"

	"github.com/common-creation/sandboxxing/internal/config"
)

func TestCompilePattern(t *testing.T) {
	re, err := compilePattern("web-*")
	if err != nil {
		t.Fatal(err)
	}
	if !re.MatchString("web-1") || re.MatchString("db-1") {
		t.Error("wildcard pattern does not match as expected")
	}
	if re, err := compilePattern(""); err != nil || re != nil {
		t.Error("empty pattern must match everything")
	}
	if re, err := compilePattern("*"); err != nil || re != nil {
		t.Error("'*' must match everything")
	}
	if re, err = compilePattern("a?c"); err != nil || !re.MatchString("abc") {
		t.Error("'?' must match one character")
	}
}

func TestApplyDefaults(t *testing.T) {
	cfg := config.Default()
	m := &Manager{cfg: cfg}
	opts := &Options{}
	m.applyDefaults(opts)
	if opts.Image != cfg.Image {
		t.Errorf("image = %q, want %q", opts.Image, cfg.Image)
	}
	if opts.CPU != cfg.DefaultCPU {
		t.Errorf("cpu = %d, want %d", opts.CPU, cfg.DefaultCPU)
	}
	if opts.Memory != int64(cfg.DefaultMemory) || opts.Disk != int64(cfg.DefaultDisk) {
		t.Error("memory and disk defaults were not applied")
	}

	custom := &Options{CPU: 8, Memory: 1 << 30, Disk: 2 << 30, Image: "other"}
	m.applyDefaults(custom)
	if custom.CPU != 8 || custom.Memory != 1<<30 || custom.Disk != 2<<30 || custom.Image != "other" {
		t.Error("explicit values must not be overwritten")
	}
}
