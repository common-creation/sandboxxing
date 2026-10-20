package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	efstypes "github.com/aws/aws-sdk-go-v2/service/efs/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"

	"github.com/common-creation/sandboxxing/internal/agent"
	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/progress"
	"github.com/common-creation/sandboxxing/internal/state"
)

// shareTag marks the access point of a shared directory with its path.
const shareTag = "sandboxxing:share"

// shareAP is the access point of a shared directory. Every container that
// mounts the directory uses the same one.
type shareAP struct {
	ID  string
	ARN string
}

// Shares returns the shared directories that a container mounts: the ones of
// every MicroVM followed by its own.
func (b *MicroVM) Shares(vm *state.VM) []config.Share {
	return config.MergeShares(b.cfg.Shares, vm.Shares)
}

// CheckShares validates the shared directories of a container. Besides the
// directories of the home directories, which are never shared, a share may
// not be mounted over the home directory or above it. A target inside the
// home directory, such as /root/.claude, is mounted after it.
func (b *MicroVM) CheckShares(shares []config.Share) error {
	seen := map[string]string{}
	for _, share := range shares {
		if err := b.cfg.CheckShare(share); err != nil {
			return err
		}
		target := path.Clean(share.MountTarget())
		if target == b.home || strings.HasPrefix(b.home, target+"/") {
			return fmt.Errorf("share %s cannot be mounted at %s, which would hide the home directory %s", share.Path, target, b.home)
		}
		if previous, ok := seen[target]; ok {
			return fmt.Errorf("shares %q and %q are both mounted at %s", previous, share.Path, target)
		}
		seen[target] = share.Path
	}
	return nil
}

// grant gives the execution role of a container its permissions, the home
// directory and the shared directories, and returns the shares for the run
// hook. The policy is only written when it changed, because IAM takes a
// moment to apply a new one.
func (b *MicroVM) grant(ctx context.Context, vm *state.VM, mv state.MicroVM) ([]agent.ShareMount, error) {
	shares := b.Shares(vm)
	if err := b.CheckShares(shares); err != nil {
		return nil, err
	}
	mounts := make([]agent.ShareMount, 0, len(shares))
	grants := make([]shareGrant, 0, len(shares))
	for _, share := range shares {
		ap, err := b.shareAccessPoint(ctx, path.Clean(share.Path))
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, agent.ShareMount{AccessPointID: ap.ID, Target: share.MountTarget(), ReadOnly: share.ReadOnly})
		grants = append(grants, shareGrant{ARN: ap.ARN, ReadOnly: share.ReadOnly})
	}
	policy, err := b.rolePolicy(mv, grants)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	unchanged := b.policies[mv.RoleName] == policy
	b.mu.Unlock()
	if unchanged {
		return mounts, nil
	}
	if _, err := b.iam.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       aws.String(mv.RoleName),
		PolicyName:     aws.String(rolePolicyName),
		PolicyDocument: aws.String(policy),
	}); err != nil {
		return nil, fmt.Errorf("grant the execution role of %s: %w", vm.Name, err)
	}
	b.mu.Lock()
	b.policies[mv.RoleName] = policy
	b.mu.Unlock()
	return mounts, nil
}

// shareAccessPoint returns the access point of a shared directory, creating
// it when no container used the directory before.
func (b *MicroVM) shareAccessPoint(ctx context.Context, dir string) (shareAP, error) {
	b.shareMu.Lock()
	defer b.shareMu.Unlock()
	if ap, ok := b.shareAPs[dir]; ok {
		return ap, nil
	}
	ap, err := b.findShareAccessPoint(ctx, dir)
	if err != nil {
		return shareAP{}, err
	}
	if ap.ID == "" {
		progress.From(ctx).Step("creating the access point of the shared directory %s", dir)
		sum := sha256.Sum256([]byte(dir))
		out, err := b.efs.CreateAccessPoint(ctx, &efs.CreateAccessPointInput{
			ClientToken:  aws.String("sandboxxing-share-" + hex.EncodeToString(sum[:16])),
			FileSystemId: aws.String(b.cfg.FileSystemID),
			// No user is enforced, so the files keep their owners as they
			// would on a bind mount.
			RootDirectory: &efstypes.RootDirectory{
				Path: aws.String(dir),
				CreationInfo: &efstypes.CreationInfo{
					OwnerUid: aws.Int64(0), OwnerGid: aws.Int64(0), Permissions: aws.String("0755"),
				},
			},
			Tags: efsTags(map[string]string{shareTag: dir}),
		})
		var exists *efstypes.AccessPointAlreadyExists
		switch {
		case errors.As(err, &exists):
			// An earlier attempt created it, and the listing did not show
			// it yet.
			if ap, err = b.describeShare(ctx, aws.ToString(exists.AccessPointId)); err != nil {
				return shareAP{}, err
			}
		case err != nil:
			return shareAP{}, fmt.Errorf("create the access point of the shared directory %s: %w", dir, err)
		default:
			ap = shareAP{ID: aws.ToString(out.AccessPointId), ARN: aws.ToString(out.AccessPointArn)}
		}
		if err := b.waitAccessPoint(ctx, ap.ID); err != nil {
			return shareAP{}, err
		}
	}
	if b.shareAPs == nil {
		b.shareAPs = map[string]shareAP{}
	}
	b.shareAPs[dir] = ap
	return ap, nil
}

// findShareAccessPoint looks up the access point of a shared directory by its
// tag. It returns an empty one when there is none.
func (b *MicroVM) findShareAccessPoint(ctx context.Context, dir string) (shareAP, error) {
	pages := efs.NewDescribeAccessPointsPaginator(b.efs, &efs.DescribeAccessPointsInput{FileSystemId: aws.String(b.cfg.FileSystemID)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return shareAP{}, fmt.Errorf("list the access points of %s: %w", b.cfg.FileSystemID, err)
		}
		for _, ap := range page.AccessPoints {
			switch ap.LifeCycleState {
			case efstypes.LifeCycleStateDeleting, efstypes.LifeCycleStateDeleted, efstypes.LifeCycleStateError:
				continue
			}
			for _, tag := range ap.Tags {
				if aws.ToString(tag.Key) == shareTag && aws.ToString(tag.Value) == dir {
					return shareAP{ID: aws.ToString(ap.AccessPointId), ARN: aws.ToString(ap.AccessPointArn)}, nil
				}
			}
		}
	}
	return shareAP{}, nil
}

func (b *MicroVM) describeShare(ctx context.Context, id string) (shareAP, error) {
	out, err := b.efs.DescribeAccessPoints(ctx, &efs.DescribeAccessPointsInput{AccessPointId: aws.String(id)})
	if err != nil {
		return shareAP{}, fmt.Errorf("describe access point %s: %w", id, err)
	}
	if len(out.AccessPoints) != 1 {
		return shareAP{}, fmt.Errorf("access point %s not found", id)
	}
	return shareAP{ID: id, ARN: aws.ToString(out.AccessPoints[0].AccessPointArn)}, nil
}

// shareGrant is the permission of an execution role on a shared directory.
type shareGrant struct {
	ARN      string
	ReadOnly bool
}

// shareStatements are the statements of the shared directories in the policy
// of an execution role. A read-only share lacks ClientWrite, so that EFS
// refuses writes whatever the VM does.
func shareStatements(fileSystemARN string, grants []shareGrant) []map[string]any {
	var rw, ro []string
	for _, g := range grants {
		if g.ReadOnly {
			ro = append(ro, g.ARN)
		} else {
			rw = append(rw, g.ARN)
		}
	}
	var statements []map[string]any
	add := func(arns []string, actions ...string) {
		if len(arns) == 0 {
			return
		}
		sort.Strings(arns)
		statements = append(statements, map[string]any{
			"Effect":    "Allow",
			"Action":    actions,
			"Resource":  fileSystemARN,
			"Condition": map[string]any{"StringEquals": map[string][]string{"elasticfilesystem:AccessPointArn": arns}},
		})
	}
	add(rw, "elasticfilesystem:ClientMount", "elasticfilesystem:ClientWrite", "elasticfilesystem:ClientRootAccess")
	add(ro, "elasticfilesystem:ClientMount", "elasticfilesystem:ClientRootAccess")
	return statements
}
