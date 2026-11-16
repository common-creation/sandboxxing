package config

import (
	"testing"
	"time"
)

func TestEC2ConfigDefaultsAndShareGrants(t *testing.T) {
	c := Default()
	c.DefaultBackend = "ec2"
	c.EC2 = &EC2Config{FileSystemID: "fs-test", SubnetID: "subnet-private", SecurityGroupIDs: []string{"sg-closed"}, PermissionsBoundary: "arn:aws:iam::123:policy/limit", AgentS3URI: "s3://assets/agent", EFSUtilsS3URI: "s3://assets/efs.deb"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.EC2.DefaultImage != DefaultEC2Image || c.EC2.StartTimeout.Duration() != 10*time.Minute || c.EC2.DefaultInstanceType != DefaultEC2InstanceType {
		t.Fatalf("missing defaults %+v", c.EC2)
	}
	ro := Share{Path: "/source", Target: "/data", ReadOnly: true}
	rw := Share{Path: "/source", Target: "/other"}
	if err := c.EC2.CheckShares([]Share{ro, rw}); err == nil {
		t.Fatal("one source has conflicting IAM rights")
	}
	if err := c.EC2.CheckShares([]Share{{Path: "/source", Target: "/root"}}); err == nil {
		t.Fatal("accepted share over home")
	}
	for _, target := range []string{"/etc/sandboxxing", "/etc/sandboxxing/assets", "/run", "/run/sandboxxing-agent-mounts.json"} {
		if err := c.EC2.CheckShare(Share{Path: "/tools", Target: target}); err == nil {
			t.Errorf("accepted target %s over agent state", target)
		}
	}
	rw.Target = ro.Target
	if err := c.EC2.CheckShares(MergeShares([]Share{ro}, []Share{rw})); err != nil {
		t.Fatal(err)
	}
}

func TestEC2RolePath(t *testing.T) {
	for path, ok := range map[string]bool{
		"/sandboxxing/ec2/":                  true,
		"/sandboxxing/ec2/fs07ad0fabd051c7/": true,
		"/sandboxxing/ec2/fs-07ad0fabd051/":  false,
		"/sandboxxing/ec2":                   false,
	} {
		c := Default()
		c.EC2 = &EC2Config{FileSystemID: "fs-test", SubnetID: "subnet-private", SecurityGroupIDs: []string{"sg-closed"}, PermissionsBoundary: "arn:aws:iam::123:policy/limit", AgentS3URI: "s3://assets/agent", EFSUtilsS3URI: "s3://assets/efs.deb", RolePath: path}
		if err := c.Validate(); (err == nil) != ok {
			t.Errorf("role_path %q: err = %v, want ok = %v", path, err, ok)
		}
	}
}
