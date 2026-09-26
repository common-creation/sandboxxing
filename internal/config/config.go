// Package config defines the daemon configuration and its defaults.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Config is the runtime configuration of the sandboxxing daemon.
type Config struct {
	// DataDir stores container images and the state file.
	DataDir string `json:"data_dir"`
	// StateFile stores the JSON metadata of all containers.
	StateFile string `json:"state_file"`

	// SSHAddr is the listen address of the SSH protocol endpoint.
	SSHAddr string `json:"ssh_addr"`
	// Domain is the DNS domain advertised to clients. Only used for display
	// and for the documented naming scheme, not for name resolution.
	Domain string `json:"domain"`
	// AdminUsers are the SSH user names allowed to run the control commands
	// (ls, new, rm, ...). Any other user name is interpreted as a container
	// name for direct access.
	AdminUsers []string `json:"admin_users"`
	// Password is the shared password for the control users. When empty the
	// daemon generates one on first start and stores it in PasswordFile.
	Password string `json:"password"`
	// PasswordFile is where the generated password is kept.
	PasswordFile string `json:"password_file"`
	// AuthorizedKeysFile lists the SSH public keys that may connect. When the
	// file does not exist, everyone who knows the password may connect.
	AuthorizedKeysFile string `json:"authorized_keys_file"`

	// Bridge is the host bridge name that connects containers to the host.
	Bridge string `json:"bridge"`
	// Subnet is the CIDR of the container network (the bridge address is the
	// first usable address and is used as the default gateway).
	Subnet string `json:"subnet"`

	// ImageDir is where built root file system images are cached.
	ImageDir string `json:"image_dir"`
	// Image is the default image used by `new` when --image is omitted.
	Image string `json:"image"`
	// Mirror is the pacman mirror URL used by pacstrap.
	Mirror string `json:"mirror"`
	// Arch is the architecture used by pacstrap.
	Arch string `json:"arch"`
	// PacmanConfig is the pacman configuration file passed to pacstrap.
	PacmanConfig string `json:"pacman_config"`
	// ImagePackages are the packages installed into every image.
	ImagePackages []string `json:"image_packages"`
	// PacstrapTimeout limits the duration of one image build.
	PacstrapTimeout Duration `json:"pacstrap_timeout"`
	// BootTimeout limits how long sandboxxing waits for a container to boot.
	BootTimeout Duration `json:"boot_timeout"`

	// DefaultCPU, DefaultMemory and DefaultDisk are the defaults applied by
	// `new` when the corresponding option is omitted.
	DefaultCPU    int      `json:"default_cpu"`
	DefaultMemory DiskSize `json:"default_memory"`
	DefaultDisk   DiskSize `json:"default_disk"`

	// LogLevel controls daemon verbosity: debug, info, warn, error.
	LogLevel string `json:"log_level"`

	// Path is the file the configuration was loaded from.
	Path string `json:"-"`
}

// Duration wraps time.Duration so that configuration files can use strings
// such as "10m".
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch t := v.(type) {
	case string:
		parsed, err := time.ParseDuration(t)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", t, err)
		}
		*d = Duration(parsed)
	case float64:
		*d = Duration(time.Duration(t) * time.Second)
	default:
		return fmt.Errorf("invalid duration value %v", v)
	}
	return nil
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Duration returns the wrapped duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// Default returns the built-in configuration.
func Default() *Config {
	return &Config{
		DataDir:         "/var/lib/sandboxxing",
		SSHAddr:         ":2222",
		Domain:          "",
		AdminUsers:      []string{"sandbox", "admin"},
		Password:        "",
		Bridge:          "sbx0",
		Subnet:          "10.100.0.0/16",
		ImageDir:        "/var/lib/sandboxxing/images",
		Image:           "arch",
		Mirror:          "https://geo.mirror.pkgbuild.com/$repo/os/$arch",
		Arch:            defaultArch(),
		ImagePackages:   []string{"base", "systemd", "openssh", "sudo", "vim", "iproute2", "iputils", "dnsutils", "net-tools", "git", "curl", "ca-certificates"},
		PacstrapTimeout: Duration(30 * time.Minute),
		BootTimeout:     Duration(2 * time.Minute),
		DefaultCPU:      2,
		DefaultMemory:   DiskSize(2 << 30),
		DefaultDisk:     DiskSize(10 << 30),
		LogLevel:        "info",
	}
}

// DefaultPath is the system configuration file.
const DefaultPath = "/etc/sandboxxing/config.json"

// Load reads configuration from path. A missing file is not an error: the
// defaults are returned instead. Fields that are not present in the file keep
// their default value, and paths derived from data_dir follow it.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		path = DefaultPath
	}
	cfg.Path = path
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, cfg.Validate()
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	// Keep the derived paths consistent with data_dir unless the file names
	// them explicitly.
	if _, ok := raw["data_dir"]; ok {
		if _, ok := raw["image_dir"]; !ok {
			cfg.ImageDir = ""
		}
		if _, ok := raw["state_file"]; !ok {
			cfg.StateFile = ""
		}
		if _, ok := raw["password_file"]; !ok {
			cfg.PasswordFile = ""
		}
		if _, ok := raw["authorized_keys_file"]; !ok {
			cfg.AuthorizedKeysFile = ""
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate normalizes derived fields and checks the configuration.
func (c *Config) Validate() error {
	if c.DataDir == "" {
		return errors.New("data_dir must not be empty")
	}
	if c.SSHAddr == "" {
		return errors.New("ssh_addr must not be empty")
	}
	if len(c.AdminUsers) == 0 {
		return errors.New("admin_users must not be empty")
	}
	if c.ImageDir == "" {
		c.ImageDir = filepath.Join(c.DataDir, "images")
	}
	if c.StateFile == "" {
		c.StateFile = filepath.Join(c.DataDir, "state.json")
	}
	if c.PasswordFile == "" {
		c.PasswordFile = filepath.Join(c.DataDir, "password")
	}
	if c.AuthorizedKeysFile == "" {
		c.AuthorizedKeysFile = filepath.Join(c.DataDir, "authorized_keys")
	}
	if c.Bridge == "" {
		c.Bridge = "sbx0"
	}
	if c.Subnet == "" {
		c.Subnet = "10.100.0.0/16"
	}
	if _, netw, err := net.ParseCIDR(c.Subnet); err != nil {
		return fmt.Errorf("invalid subnet %q: %w", c.Subnet, err)
	} else if ones, _ := netw.Mask.Size(); ones > 24 {
		return fmt.Errorf("subnet %q is too narrow: use a prefix of /24 or shorter (for example /16 or /24)", c.Subnet)
	}
	if c.Arch == "" {
		c.Arch = defaultArch()
	}
	if c.Image == "" {
		c.Image = "arch"
	}
	if c.DefaultCPU <= 0 {
		c.DefaultCPU = 2
	}
	if c.DefaultMemory <= 0 {
		c.DefaultMemory = DiskSize(2 << 30)
	}
	if c.DefaultDisk <= 0 {
		c.DefaultDisk = DiskSize(10 << 30)
	}
	if c.PacstrapTimeout <= 0 {
		c.PacstrapTimeout = Duration(30 * time.Minute)
	}
	if c.BootTimeout <= 0 {
		c.BootTimeout = Duration(2 * time.Minute)
	}
	switch c.LogLevel {
	case "", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid log_level %q", c.LogLevel)
	}
	for _, u := range c.AdminUsers {
		if u == "" || strings.ContainsAny(u, " \t@") {
			return fmt.Errorf("invalid admin user name %q", u)
		}
	}
	return nil
}

// IsAdminUser reports whether name is one of the control users.
func (c *Config) IsAdminUser(name string) bool {
	for _, u := range c.AdminUsers {
		if u == name {
			return true
		}
	}
	return false
}

// Addr returns the address assigned to the container bridge. It is the first
// usable address of the subnet, which is what the containers use as their
// default gateway. The network address itself (for example 10.100.0.0/16) can
// never be assigned to an interface, so one is added to it.
func (c *Config) Addr() net.IP {
	_, n, err := net.ParseCIDR(c.Subnet)
	if err != nil {
		return net.IPv4(10, 100, 0, 1)
	}
	ip := make(net.IP, len(n.IP))
	copy(ip, n.IP)
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			break
		}
	}
	return ip
}

// Gateway returns the default gateway address handed to containers.
func (c *Config) Gateway() net.IP { return c.Addr() }

// Mask returns the subnet mask of the container network.
func (c *Config) Mask() net.IPMask {
	_, n, err := net.ParseCIDR(c.Subnet)
	if err != nil {
		return net.CIDRMask(16, 32)
	}
	return n.Mask
}

func defaultArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return runtime.GOARCH
	}
}
