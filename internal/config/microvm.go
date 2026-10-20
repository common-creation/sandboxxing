package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// MicroVMConfig configures the backend that runs containers as AWS Lambda
// MicroVMs. The AWS resources it refers to are created by the CDK stack in
// deploy/cdk, whose outputs list the values.
type MicroVMConfig struct {
	// Region is the AWS region of the MicroVMs. Empty uses the region of the
	// AWS configuration of the daemon.
	Region string `json:"region"`
	// Images maps image names (`new --image`) to MicroVM images.
	Images map[string]MicroVMImage `json:"images"`
	// DefaultImage is the image used when `new` names none. It defaults to the
	// only image when there is exactly one.
	DefaultImage string `json:"default_image"`
	// EgressNetworkConnectors are the outbound connectors of every MicroVM.
	// One of them must reach the EFS mount targets.
	EgressNetworkConnectors []string `json:"egress_network_connectors"`
	// FileSystemID is the EFS file system that holds the home directories.
	FileSystemID string `json:"efs_file_system_id"`
	// HomeRoot is the directory of the file system under which every
	// instance gets its own directory.
	HomeRoot string `json:"efs_home_root"`
	// RolePath is the IAM path of the execution roles that the daemon
	// creates, one per instance.
	RolePath string `json:"role_path"`
	// PermissionsBoundary is the policy every execution role is created with.
	PermissionsBoundary string `json:"permissions_boundary_arn"`
	// LogGroup receives the output of the agents. Empty disables the logs.
	LogGroup string `json:"log_group"`
	// MaximumDuration is the lifetime of one MicroVM, at most 8 hours. A
	// MicroVM that reaches it is replaced by a new one.
	MaximumDuration Duration `json:"maximum_duration"`
	// TokenTTL is the lifetime of the tokens that authenticate the daemon at
	// the MicroVM endpoints.
	TokenTTL Duration `json:"token_ttl"`
	// StartTimeout limits how long a MicroVM may take to answer.
	StartTimeout Duration `json:"start_timeout"`
	// IdlePolicy suspends idle MicroVMs. Nil keeps them running.
	IdlePolicy *MicroVMIdlePolicy `json:"idle_policy"`
}

// MicroVMImage is one MicroVM image.
type MicroVMImage struct {
	ARN string `json:"arn"`
	// Version pins an image version. Empty runs the latest active one.
	Version string `json:"version"`
}

// MicroVMIdlePolicy is the automatic suspension of a MicroVM.
type MicroVMIdlePolicy struct {
	// MaxIdle is the time without traffic after which the VM is suspended.
	MaxIdle Duration `json:"max_idle"`
	// SuspendedDuration is how long a suspended VM is kept.
	SuspendedDuration Duration `json:"suspended_duration"`
	// AutoResume resumes a suspended VM when traffic arrives.
	AutoResume bool `json:"auto_resume"`
}

// MaxMicroVMDuration is the longest lifetime Lambda allows a MicroVM.
const MaxMicroVMDuration = 8 * time.Hour

// validate fills in the defaults and checks the settings.
func (c *MicroVMConfig) validate() error {
	if len(c.Images) == 0 {
		return errors.New("microvm.images must name at least one MicroVM image")
	}
	for name, img := range c.Images {
		if name == "" || strings.ContainsAny(name, " \t/") {
			return fmt.Errorf("invalid microvm image name %q", name)
		}
		if !strings.HasPrefix(img.ARN, "arn:") {
			return fmt.Errorf("microvm image %q needs the ARN of a MicroVM image", name)
		}
	}
	if c.DefaultImage == "" && len(c.Images) == 1 {
		for name := range c.Images {
			c.DefaultImage = name
		}
	}
	if c.DefaultImage == "" {
		return fmt.Errorf("microvm.default_image must name one of %s", strings.Join(c.ImageNames(), ", "))
	}
	if _, ok := c.Images[c.DefaultImage]; !ok {
		return fmt.Errorf("microvm.default_image %q is not one of %s", c.DefaultImage, strings.Join(c.ImageNames(), ", "))
	}
	if c.FileSystemID == "" {
		return errors.New("microvm.efs_file_system_id must name the file system of the home directories")
	}
	if len(c.EgressNetworkConnectors) == 0 {
		return errors.New("microvm.egress_network_connectors must list the connector that reaches the file system")
	}
	if c.PermissionsBoundary == "" {
		return errors.New("microvm.permissions_boundary_arn must name the boundary of the execution roles")
	}
	if c.HomeRoot == "" {
		c.HomeRoot = "/homes"
	}
	if !strings.HasPrefix(c.HomeRoot, "/") {
		return fmt.Errorf("microvm.efs_home_root %q must be an absolute path", c.HomeRoot)
	}
	c.HomeRoot = strings.TrimSuffix(c.HomeRoot, "/")
	if c.RolePath == "" {
		c.RolePath = "/sandboxxing/instances/"
	}
	if !strings.HasPrefix(c.RolePath, "/") || !strings.HasSuffix(c.RolePath, "/") {
		return fmt.Errorf("microvm.role_path %q must start and end with /", c.RolePath)
	}
	if c.MaximumDuration <= 0 {
		c.MaximumDuration = Duration(MaxMicroVMDuration)
	}
	if c.MaximumDuration.Duration() > MaxMicroVMDuration || c.MaximumDuration.Duration() < time.Minute {
		return fmt.Errorf("microvm.maximum_duration must be between 1m and %s", MaxMicroVMDuration)
	}
	if c.TokenTTL <= 0 {
		c.TokenTTL = Duration(30 * time.Minute)
	}
	if c.TokenTTL.Duration() < 10*time.Minute {
		return errors.New("microvm.token_ttl must be at least 10m")
	}
	if c.StartTimeout <= 0 {
		c.StartTimeout = Duration(3 * time.Minute)
	}
	if p := c.IdlePolicy; p != nil {
		if p.MaxIdle <= 0 || p.MaxIdle.Duration() > MaxMicroVMDuration {
			return fmt.Errorf("microvm.idle_policy.max_idle must be between 1s and %s", MaxMicroVMDuration)
		}
		if p.SuspendedDuration < 0 {
			return errors.New("microvm.idle_policy.suspended_duration must not be negative")
		}
	}
	return nil
}

// ImageNames returns the configured image names, sorted.
func (c *MicroVMConfig) ImageNames() []string {
	names := make([]string, 0, len(c.Images))
	for name := range c.Images {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
