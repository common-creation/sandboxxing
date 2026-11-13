package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/common-creation/sandboxxing/internal/agent"
	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
)

// EC2IAMAPI manages the instance profile and its bounded role.
type EC2IAMAPI interface {
	IAMAPI
	GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	GetInstanceProfile(context.Context, *iam.GetInstanceProfileInput, ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error)
	CreateInstanceProfile(context.Context, *iam.CreateInstanceProfileInput, ...func(*iam.Options)) (*iam.CreateInstanceProfileOutput, error)
	AddRoleToInstanceProfile(context.Context, *iam.AddRoleToInstanceProfileInput, ...func(*iam.Options)) (*iam.AddRoleToInstanceProfileOutput, error)
	RemoveRoleFromInstanceProfile(context.Context, *iam.RemoveRoleFromInstanceProfileInput, ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error)
	DeleteInstanceProfile(context.Context, *iam.DeleteInstanceProfileInput, ...func(*iam.Options)) (*iam.DeleteInstanceProfileOutput, error)
}

type ec2Profiles struct {
	cfg    *config.EC2Config
	iam    EC2IAMAPI
	shared *sharedDirectories
}

func newEC2Profiles(cfg *config.EC2Config, efs EFSAPI, iam EC2IAMAPI) *ec2Profiles {
	return &ec2Profiles{cfg: cfg, iam: iam, shared: newSharedDirectories(efs, cfg.FileSystemID, 2*time.Second, true)}
}

const ec2Trust = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
const ec2PolicyName = "sandboxxing-guest"

func missingIAMResource(err error) bool {
	var missing *iamtypes.NoSuchEntityException
	return errors.As(err, &missing)
}

// Identity names follow the persistent launch token so provisioning and cleanup
// are repeatable even when the process exits between two IAM calls.
func (p *ec2Profiles) ensure(ctx context.Context, vm *state.VM, token string) (string, []agent.ShareMount, error) {
	name := "sbx-ec2-" + token
	shares := config.MergeShares(p.cfg.Shares, vm.Shares)
	if err := p.cfg.CheckShares(shares); err != nil {
		return "", nil, err
	}
	mounts, grants, err := p.shared.resolve(ctx, shares)
	if err != nil {
		return "", nil, err
	}
	policy, err := p.policy(grants)
	if err != nil {
		return "", nil, err
	}
	// A newly created role/profile is not immediately visible to the next
	// IAM operation. Re-read it with its stable name before retrying.
	for {
		err = p.ensureIdentity(ctx, name, token, policy)
		var exists *iamtypes.EntityAlreadyExistsException
		if !missingIAMResource(err) && !errors.As(err, &exists) {
			break
		}
		if err := sleep(ctx, 2*time.Second); err != nil {
			return "", nil, err
		}
	}
	if err != nil {
		return "", nil, err
	}
	return name, mounts, nil
}

func (p *ec2Profiles) ensureIdentity(ctx context.Context, name, token, policy string) error {
	tags := iamTags(map[string]string{"sandboxxing:instance": token, "sandboxxing:filesystem": p.cfg.FileSystemID})
	role, err := p.iam.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
	switch {
	case missingIAMResource(err):
		_, err = p.iam.CreateRole(ctx, &iam.CreateRoleInput{
			RoleName:                 aws.String(name),
			Path:                     aws.String(p.cfg.RolePath),
			PermissionsBoundary:      aws.String(p.cfg.PermissionsBoundary),
			AssumeRolePolicyDocument: aws.String(ec2Trust),
			Tags:                     tags,
		})
	case err == nil:
		if role.Role == nil || aws.ToString(role.Role.Path) != p.cfg.RolePath || role.Role.PermissionsBoundary == nil ||
			aws.ToString(role.Role.PermissionsBoundary.PermissionsBoundaryArn) != p.cfg.PermissionsBoundary {
			return fmt.Errorf("role %s has another IAM path or permissions boundary", name)
		}
	}
	if err != nil {
		return fmt.Errorf("prepare IAM role %s: %w", name, err)
	}
	_, err = p.iam.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName: aws.String(name), PolicyName: aws.String(ec2PolicyName),
		PolicyDocument: aws.String(policy),
	})
	if err != nil {
		return fmt.Errorf("grant IAM role %s: %w", name, err)
	}

	profile, err := p.iam.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(name)})
	switch {
	case missingIAMResource(err):
		_, err = p.iam.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{
			InstanceProfileName: aws.String(name),
			Path:                aws.String(p.cfg.RolePath),
			Tags:                tags,
		})
	case err == nil:
		if profile.InstanceProfile == nil || aws.ToString(profile.InstanceProfile.Path) != p.cfg.RolePath {
			return fmt.Errorf("profile %s has another IAM path", name)
		}
		if len(profile.InstanceProfile.Roles) > 0 {
			if aws.ToString(profile.InstanceProfile.Roles[0].RoleName) != name {
				return fmt.Errorf("profile %s already contains another role", name)
			}
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("prepare instance profile %s: %w", name, err)
	}
	_, err = p.iam.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(name), RoleName: aws.String(name),
	})
	if err != nil {
		return fmt.Errorf("add role to profile %s: %w", name, err)
	}
	return nil
}

func (p *ec2Profiles) policy(grants []shareGrant) (string, error) {
	partition, _, err := arnAccount(p.cfg.PermissionsBoundary)
	if err != nil {
		return "", err
	}
	var objects []string
	for _, uri := range []string{p.cfg.AgentS3URI, p.cfg.EFSUtilsS3URI} {
		object, ok := strings.CutPrefix(uri, "s3://")
		if !ok || !strings.Contains(object, "/") || strings.ContainsAny(object, "*?$") {
			return "", fmt.Errorf("invalid S3 asset URI %q", uri)
		}
		objects = append(objects, "arn:"+partition+":s3:::"+object)
	}
	statements := []map[string]any{
		{
			"Effect": "Allow",
			"Action": []string{
				"ssm:UpdateInstanceInformation",
				"ssmmessages:CreateControlChannel", "ssmmessages:OpenControlChannel",
				"ssmmessages:CreateDataChannel", "ssmmessages:OpenDataChannel",
			},
			"Resource": "*",
		},
		{"Effect": "Allow", "Action": "s3:GetObject", "Resource": objects},
	}
	if len(grants) > 0 {
		prefix, _, ok := strings.Cut(grants[0].ARN, ":access-point/")
		if !ok {
			return "", fmt.Errorf("invalid EFS access point ARN %q", grants[0].ARN)
		}
		statements = append(statements, shareStatements(prefix+":file-system/"+p.cfg.FileSystemID, grants)...)
	}
	doc, err := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": statements})
	return string(doc), err
}

// remove only deletes the identity of this instance. The access points and
// the shared directories behind them live for the lifetime of the stack.
func (p *ec2Profiles) remove(ctx context.Context, token string) error {
	name := aws.String("sbx-ec2-" + token)
	_, err := p.iam.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{InstanceProfileName: name, RoleName: name})
	if err != nil && !missingIAMResource(err) {
		return fmt.Errorf("detach role %s: %w", *name, err)
	}
	_, err = p.iam.DeleteInstanceProfile(ctx, &iam.DeleteInstanceProfileInput{InstanceProfileName: name})
	if err != nil && !missingIAMResource(err) {
		return fmt.Errorf("delete profile %s: %w", *name, err)
	}
	_, err = p.iam.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{RoleName: name, PolicyName: aws.String(ec2PolicyName)})
	if err != nil && !missingIAMResource(err) {
		return fmt.Errorf("delete policy of %s: %w", *name, err)
	}
	_, err = p.iam.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: name})
	if err != nil && !missingIAMResource(err) {
		return fmt.Errorf("delete role %s: %w", *name, err)
	}
	return nil
}
