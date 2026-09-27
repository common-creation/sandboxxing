// Package state stores the persistent metadata of sandboxxing containers.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/common-creation/sandboxxing/internal/config"
)

// NetConfigVersion is the version of the network configuration that
// sandboxxing writes into a container. A container whose recorded version is
// lower is reconfigured the next time it starts, which is how fixes reach
// containers created by an older release.
//
//	1: initial
//	2: the gateway became the first usable address of the subnet
//	3: /etc/resolv.conf no longer lists the host's loopback resolver
const NetConfigVersion = 3

// VM describes one sandbox container.
type VM struct {
	Name      string            `json:"name"`
	Image     string            `json:"image"`
	Created   time.Time         `json:"created"`
	CPU       int               `json:"cpu"`
	Memory    int64             `json:"memory"`
	Disk      int64             `json:"disk"`
	IP        string            `json:"ip,omitempty"`
	MachineID string            `json:"machine_id,omitempty"`
	NetConfig int               `json:"net_config_version,omitempty"`
	Comment   string            `json:"comment,omitempty"`
	Tags      []string          `json:"tags,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	// Shares are the host directories that this container mounts, on top of
	// the ones configured for the whole host.
	Shares []config.Share `json:"shares,omitempty"`
}

// State is the whole persisted state file.
type State struct {
	Version int            `json:"version"`
	NextIP  int            `json:"next_ip"`
	VMs     map[string]*VM `json:"vms"`

	path string
	base net.IP
	mu   sync.Mutex
}

const currentVersion = 1

// Open loads the state file, creating an empty state when it does not exist.
func Open(path string) (*State, error) {
	s := &State{Version: currentVersion, NextIP: 2, VMs: map[string]*VM{}, path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	if len(b) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	if s.VMs == nil {
		s.VMs = map[string]*VM{}
	}
	if s.NextIP < 2 || s.NextIP > 254 {
		s.NextIP = 2
	}
	if s.Version == 0 {
		s.Version = currentVersion
	}
	return s, nil
}

// SetSubnet configures the address range used by allocIP. It must be called
// before adding VMs.
func (s *State) SetSubnet(base net.IP) {
	s.base = base.To4()
}

// Save writes the state file atomically.
func (s *State) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *State) saveLocked() error {
	s.Version = currentVersion
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Add registers a new VM and persists the state. The caller must have filled
// in Name, Image, CPU, Memory and Disk; IP and MachineID are generated here.
func (s *State) Add(vm *VM) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.VMs[vm.Name]; ok {
		return fmt.Errorf("vm %q already exists", vm.Name)
	}
	if vm.Created.IsZero() {
		vm.Created = time.Now().UTC()
	}
	if vm.MachineID == "" {
		id, err := newMachineID()
		if err != nil {
			return err
		}
		vm.MachineID = id
	}
	if vm.IP == "" {
		ip, err := s.allocIPLocked()
		if err != nil {
			return err
		}
		vm.IP = ip
	}
	s.VMs[vm.Name] = vm
	return s.saveLocked()
}

// AllocIP reserves the next free address inside the configured subnet and
// persists the counter so that concurrent creations cannot clash.
func (s *State) AllocIP() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ip, err := s.allocIPLocked()
	if err != nil {
		return "", err
	}
	if err := s.saveLocked(); err != nil {
		return "", err
	}
	return ip, nil
}

// allocIPLocked returns the next free address inside the configured subnet.
func (s *State) allocIPLocked() (string, error) {
	if s.base == nil {
		return "", errors.New("container subnet is not configured")
	}
	used := map[int]bool{0: true, 1: true, 255: true} // network, bridge, broadcast
	for _, vm := range s.VMs {
		if ip := net.ParseIP(vm.IP).To4(); ip != nil {
			used[int(ip[3])] = true
		}
	}
	for i := 0; i < 253; i++ {
		octet := s.NextIP
		s.NextIP++
		if s.NextIP > 254 {
			s.NextIP = 2
		}
		if !used[octet] {
			ip := make(net.IP, 4)
			copy(ip, s.base)
			ip[3] = byte(octet)
			return ip.String(), nil
		}
	}
	return "", errors.New("no free IP address left in the container subnet")
}

// Get returns the VM with the given name.
func (s *State) Get(name string) (*VM, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vm, ok := s.VMs[name]
	return vm, ok
}

// List returns all VMs sorted by name.
func (s *State) List() []*VM {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*VM, 0, len(s.VMs))
	for _, vm := range s.VMs {
		out = append(out, vm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Remove deletes the VM metadata and persists the state.
func (s *State) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.VMs[name]; !ok {
		return fmt.Errorf("vm %q not found", name)
	}
	delete(s.VMs, name)
	return s.saveLocked()
}

// Update applies f to the named VM under the lock and persists the state.
func (s *State) Update(name string, f func(vm *VM) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	vm, ok := s.VMs[name]
	if !ok {
		return fmt.Errorf("vm %q not found", name)
	}
	if err := f(vm); err != nil {
		return err
	}
	return s.saveLocked()
}

// Path returns the state file location.
func (s *State) Path() string { return s.path }

// Lock acquires an advisory lock on the state file so that only one daemon
// instance manages the containers at a time.
func (s *State) Lock() (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another sandboxxing daemon is already running: %w", err)
	}
	return f, nil
}

// ValidName reports whether name is acceptable as a container name. The limit
// of 12 characters keeps the derived host side veth name ("vb-" + name) below
// the Linux interface name limit of 15 characters.
func ValidName(name string) bool {
	if name == "" || len(name) > 12 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// StripDomain reduces a DNS style user name to the container name. Container
// names can only contain letters, digits, '-' and '_', so everything from the
// first dot on is the DNS part: both `ssh demo@host` and `ssh demo.sbx`
// (configured with `User %n`) reach the container "demo".
func StripDomain(name string) string {
	if i := strings.IndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return name
}

func newMachineID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
