package config

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

const DefaultEC2InstanceType = "m7i-flex.large"
const DefaultEC2Image = "ubuntu-24.04"

// EC2Config describes the private subnet, AMIs and bootstrap resources
// used by the EC2 backend. The optional images map pins aliases to AMI IDs.
type EC2Config struct {
	Region              string            `json:"region"`
	SubnetID            string            `json:"subnet_id"`
	SecurityGroupIDs    []string          `json:"security_group_ids"`
	FileSystemID        string            `json:"efs_file_system_id"`
	RolePath            string            `json:"role_path"`
	PermissionsBoundary string            `json:"permissions_boundary_arn"`
	AgentS3URI          string            `json:"agent_s3_uri"`
	EFSUtilsS3URI       string            `json:"efs_utils_s3_uri"`
	Shares              []Share           `json:"shares"`
	StartTimeout        Duration          `json:"start_timeout"`
	DefaultImage        string            `json:"default_image"`
	Images              map[string]string `json:"images"`
	// AssociatePublicIP gives sandboxes public IPv4 addresses for outbound
	// traffic in subnets without NAT, such as those of the default VPC.
	AssociatePublicIP bool `json:"associate_public_ip_address"`
}

func (c *EC2Config) CheckShare(s Share) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if path.Clean(s.Path) != s.Path || path.Clean(s.MountTarget()) != s.MountTarget() {
		return errors.New("EC2 share paths and targets must be clean absolute paths")
	}
	if s.MountTarget() == "/root" {
		return errors.New("EC2 share cannot cover the home directory")
	}
	target := s.MountTarget()
	for _, reserved := range []string{"/etc/sandboxxing", "/run/sandboxxing-agent-mounts.json"} {
		if target == reserved || strings.HasPrefix(reserved, target+"/") || strings.HasPrefix(target, reserved+"/") {
			return fmt.Errorf("EC2 share target %s overlaps the agent configuration or mount journal", target)
		}
	}
	return checkAccessPointPath(s.Path)
}

// CheckShares validates the effective mounts after overrides. EFS IAM grants
// apply to the source access point, not to its in-guest mount path.
func (c *EC2Config) CheckShares(shares []Share) error {
	modes := map[string]bool{}
	for _, s := range shares {
		if err := c.CheckShare(s); err != nil {
			return err
		}
		if ro, ok := modes[s.Path]; ok && ro != s.ReadOnly {
			return fmt.Errorf("EC2 share %s has conflicting read-only and read-write mounts", s.Path)
		}
		modes[s.Path] = s.ReadOnly
	}
	return nil
}

func (c *EC2Config) validate() error {
	if c.SubnetID == "" || len(c.SecurityGroupIDs) == 0 {
		return errors.New("ec2.subnet_id and ec2.security_group_ids are required")
	}
	if c.FileSystemID == "" || c.PermissionsBoundary == "" {
		return errors.New("ec2.efs_file_system_id and ec2.permissions_boundary_arn are required")
	}
	for k, v := range map[string]string{"agent_s3_uri": c.AgentS3URI, "efs_utils_s3_uri": c.EFSUtilsS3URI} {
		bucket, key, ok := strings.Cut(strings.TrimPrefix(v, "s3://"), "/")
		if !strings.HasPrefix(v, "s3://") || bucket == "" || !ok || key == "" {
			return fmt.Errorf("ec2.%s must identify an S3 object", k)
		}
	}
	if c.RolePath == "" {
		c.RolePath = "/sandboxxing/ec2/"
	}
	if !validIAMPath(c.RolePath) {
		return fmt.Errorf("ec2.role_path %q must start and end with / and contain only alphanumerics and /", c.RolePath)
	}
	if c.StartTimeout <= 0 {
		c.StartTimeout = Duration(10 * time.Minute)
	}
	for alias, id := range c.Images {
		if alias == "" || !strings.HasPrefix(id, "ami-") {
			return fmt.Errorf("ec2.images[%q] must specify an AMI ID", alias)
		}
	}
	if c.DefaultImage == "" && len(c.Images) == 1 {
		for alias := range c.Images {
			c.DefaultImage = alias
		}
	}
	if c.DefaultImage == "" {
		c.DefaultImage = DefaultEC2Image
	}
	if _, ok := c.Images[c.DefaultImage]; !ok && c.DefaultImage != DefaultEC2Image {
		return fmt.Errorf("invalid ec2.default_image %q", c.DefaultImage)
	}
	seen := map[string]bool{}
	for _, s := range c.Shares {
		if err := c.CheckShare(s); err != nil {
			return fmt.Errorf("ec2.shares: %w", err)
		}
		if seen[s.MountTarget()] {
			return fmt.Errorf("duplicate EC2 share target %q", s.MountTarget())
		}
		seen[s.MountTarget()] = true
	}
	return c.CheckShares(c.Shares)
}

// validIAMPath reports whether IAM accepts p as the path of a role or an
// instance profile it creates: alphanumerics between a leading and a
// trailing slash. CreateRole rejects other characters, such as the hyphen.
func validIAMPath(p string) bool {
	if len(p) > 512 || !strings.HasPrefix(p, "/") || !strings.HasSuffix(p, "/") {
		return false
	}
	for _, r := range p {
		if r != '/' && !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9') {
			return false
		}
	}
	return true
}
