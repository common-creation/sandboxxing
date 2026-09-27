package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// Share describes one host directory that is made available inside the
// containers. It is bind mounted by systemd-nspawn, so the host and every
// container see the same files.
type Share struct {
	// Path is the host directory to share. It must be absolute.
	Path string `json:"path"`
	// Target is where the directory appears inside the container. It
	// defaults to /shared/<name>, where <name> is the last element of Path.
	Target string `json:"target,omitempty"`
	// ReadOnly mounts the directory read-only inside the container.
	ReadOnly bool `json:"read_only,omitempty"`
}

// UnmarshalJSON accepts either a plain string ("/srv/data") or an object
// ({"path": "/srv/data", "target": "/data", "read_only": true}). The short
// form is convenient for the common case of a writable share at the default
// target.
func (s *Share) UnmarshalJSON(b []byte) error {
	var plain string
	if err := json.Unmarshal(b, &plain); err == nil {
		s.Path = plain
		return nil
	}
	type shareAlias Share
	var value shareAlias
	if err := json.Unmarshal(b, &value); err != nil {
		return fmt.Errorf("invalid share %s: %w", b, err)
	}
	*s = Share(value)
	return nil
}

// MarshalJSON renders the short form for the default target.
func (s Share) MarshalJSON() ([]byte, error) {
	if s.Target == "" && !s.ReadOnly {
		return json.Marshal(s.Path)
	}
	return json.Marshal(struct {
		Path     string `json:"path"`
		Target   string `json:"target,omitempty"`
		ReadOnly bool   `json:"read_only,omitempty"`
	}{s.Path, s.Target, s.ReadOnly})
}

// Name returns the default target name of the share.
func (s Share) Name() string { return filepath.Base(s.Path) }

// MountTarget returns the path the share is mounted at inside a container.
func (s Share) MountTarget() string {
	if s.Target != "" {
		return s.Target
	}
	return "/shared/" + s.Name()
}

// String renders the share for display.
func (s Share) String() string {
	out := s.Path + ":" + s.MountTarget()
	if s.ReadOnly {
		out += ":ro"
	}
	return out
}

// Validate checks one share.
func (s *Share) Validate() error {
	if s.Path == "" {
		return fmt.Errorf("share path must not be empty")
	}
	if !filepath.IsAbs(s.Path) {
		return fmt.Errorf("share path %q must be absolute", s.Path)
	}
	if strings.Contains(s.Path, ":") {
		return fmt.Errorf("share path %q must not contain ':'", s.Path)
	}
	if s.Name() == "/" || s.Name() == "." {
		return fmt.Errorf("share path %q must name a directory below the root", s.Path)
	}
	if s.Target != "" {
		if !filepath.IsAbs(s.Target) {
			return fmt.Errorf("share target %q must be absolute", s.Target)
		}
		if s.Target == "/" {
			return fmt.Errorf("share target must not be the container root")
		}
		if strings.Contains(s.Target, ":") {
			return fmt.Errorf("share target %q must not contain ':'", s.Target)
		}
	}
	switch s.Target {
	case "/proc", "/sys", "/dev", "/usr", "/etc":
		return fmt.Errorf("share target %q would mask a system directory", s.Target)
	}
	return nil
}

// ParseShare parses the command line form "path[:target][:ro]".
func ParseShare(value string) (Share, error) {
	parts := strings.Split(value, ":")
	switch len(parts) {
	case 0, 1:
		s := Share{Path: value}
		if err := s.Validate(); err != nil {
			return Share{}, err
		}
		return s, nil
	case 2:
		s := Share{Path: parts[0], Target: parts[1]}
		if err := s.Validate(); err != nil {
			return Share{}, err
		}
		return s, nil
	case 3:
		if parts[2] != "ro" && parts[2] != "rw" {
			return Share{}, fmt.Errorf("invalid share option %q: use ro or rw", parts[2])
		}
		s := Share{Path: parts[0], Target: parts[1], ReadOnly: parts[2] == "ro"}
		if err := s.Validate(); err != nil {
			return Share{}, err
		}
		return s, nil
	default:
		return Share{}, fmt.Errorf("invalid share %q: use path[:target][:ro]", value)
	}
}
