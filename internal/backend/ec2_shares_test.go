package backend

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/common-creation/sandboxxing/internal/backend/microvmtest"
	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
)

type profileIAM struct {
	*microvmtest.Fake
	profiles map[string]*iamtypes.InstanceProfile
}

func (f *profileIAM) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	role := f.Roles[aws.ToString(in.RoleName)]
	if role == nil {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	return &iam.GetRoleOutput{Role: &iamtypes.Role{RoleName: in.RoleName, Path: role.Path, PermissionsBoundary: &iamtypes.AttachedPermissionsBoundary{PermissionsBoundaryArn: role.PermissionsBoundary}}}, nil
}

func (f *profileIAM) GetInstanceProfile(_ context.Context, in *iam.GetInstanceProfileInput, _ ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error) {
	profile := f.profiles[aws.ToString(in.InstanceProfileName)]
	if profile == nil {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	return &iam.GetInstanceProfileOutput{InstanceProfile: profile}, nil
}

func (f *profileIAM) CreateInstanceProfile(_ context.Context, in *iam.CreateInstanceProfileInput, _ ...func(*iam.Options)) (*iam.CreateInstanceProfileOutput, error) {
	profile := &iamtypes.InstanceProfile{InstanceProfileName: in.InstanceProfileName, Path: in.Path}
	f.profiles[aws.ToString(in.InstanceProfileName)] = profile
	return &iam.CreateInstanceProfileOutput{InstanceProfile: profile}, nil
}

func (f *profileIAM) AddRoleToInstanceProfile(_ context.Context, in *iam.AddRoleToInstanceProfileInput, _ ...func(*iam.Options)) (*iam.AddRoleToInstanceProfileOutput, error) {
	f.profiles[*in.InstanceProfileName].Roles = []iamtypes.Role{{RoleName: in.RoleName}}
	return &iam.AddRoleToInstanceProfileOutput{}, nil
}

func (f *profileIAM) RemoveRoleFromInstanceProfile(_ context.Context, in *iam.RemoveRoleFromInstanceProfileInput, _ ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	profile := f.profiles[*in.InstanceProfileName]
	if profile == nil {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	profile.Roles = nil
	return &iam.RemoveRoleFromInstanceProfileOutput{}, nil
}

func (f *profileIAM) DeleteInstanceProfile(_ context.Context, in *iam.DeleteInstanceProfileInput, _ ...func(*iam.Options)) (*iam.DeleteInstanceProfileOutput, error) {
	delete(f.profiles, *in.InstanceProfileName)
	return &iam.DeleteInstanceProfileOutput{}, nil
}

func TestEC2GuestProfileShares(t *testing.T) {
	f := &profileIAM{Fake: microvmtest.New(), profiles: map[string]*iamtypes.InstanceProfile{}}
	cfg := &config.EC2Config{FileSystemID: "fs-shares", PermissionsBoundary: "arn:aws:iam::123456789012:policy/guest",
		RolePath: "/sandboxxing/ec2/fs-shares/", AgentS3URI: "s3://assets/agent", EFSUtilsS3URI: "s3://assets/efs.deb"}
	p := newEC2Profiles(cfg, f, f)
	ctx := t.Context()
	writer := &state.VM{Name: "writer", Shares: []config.Share{{Path: "/data"}}}
	reader := &state.VM{Name: "reader", Shares: []config.Share{{Path: "/data", ReadOnly: true}}}
	w, rw, err := p.ensure(ctx, writer, "writer-token")
	if err != nil {
		t.Fatal(err)
	}
	r, ro, err := p.ensure(ctx, reader, "reader-token")
	if err != nil {
		t.Fatal(err)
	}
	if w == r || len(rw) != 1 || len(ro) != 1 || rw[0].AccessPointID != ro[0].AccessPointID || rw[0].ReadOnly || !ro[0].ReadOnly {
		t.Fatalf("wrong profile or shares: %s %+v %s %+v", w, rw, r, ro)
	}
	for name, writable := range map[string]bool{w: true, r: false} {
		var policy struct {
			Statement []struct {
				Action    json.RawMessage
				Resource  json.RawMessage
				Condition map[string]map[string][]string
			}
		}
		if err := json.Unmarshal([]byte(f.Policies[name]), &policy); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(f.Policies[name], "ssm") {
			t.Fatalf("guest policy registers with Systems Manager: %s", f.Policies[name])
		}
		var mountStatements int
		for _, stmt := range policy.Statement {
			var actions []string
			if json.Unmarshal(stmt.Action, &actions) != nil || !slices.Contains(actions, "elasticfilesystem:ClientMount") {
				continue
			}
			mountStatements++
			allowed := stmt.Condition["StringEquals"]["elasticfilesystem:AccessPointArn"]
			apARN := "arn:aws:elasticfilesystem:us-east-1:" + microvmtest.Account + ":access-point/" + rw[0].AccessPointID
			fsARN := "arn:aws:elasticfilesystem:us-east-1:" + microvmtest.Account + ":file-system/fs-shares"
			if !slices.Equal(allowed, []string{apARN}) || slices.Contains(actions, "elasticfilesystem:ClientWrite") != writable || string(stmt.Resource) != "\""+fsARN+"\"" {
				t.Fatalf("wrong EFS policy: %s", f.Policies[name])
			}
		}
		if mountStatements != 1 {
			t.Fatalf("wrong EFS statement count: %d", mountStatements)
		}
	}
	if _, _, err := p.ensure(ctx, reader, "reader-token"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := p.remove(ctx, "reader-token"); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.AccessPoints) != 1 || len(f.profiles) != 1 || f.profiles[w] == nil || f.Roles[w] == nil || f.Roles[r] != nil {
		t.Fatal("reader removal changed the shared access point or writer identity")
	}
}
