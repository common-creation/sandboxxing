package backend

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/smithy-go"

	"github.com/common-creation/sandboxxing/internal/agent"
	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/progress"
	"github.com/common-creation/sandboxxing/internal/state"
)

// EC2 uses the EC2 control plane for power and disk operations, even when
// the guest agent cannot be reached.
type EC2 struct {
	cfg            *config.EC2Config
	log            *slog.Logger
	state          *state.State
	region         string
	api            *ec2.Client
	profiles       ec2ProfileManager
	transport      *ec2Transport
	poll           time.Duration
	guestReady     func(context.Context, *state.VM, []agent.ShareMount) error
	growFileSystem func(context.Context, *state.VM) error
	bootIdentifier func(context.Context, *state.VM) (string, error)
}

type ec2ProfileManager interface {
	ensure(context.Context, *state.VM, string) (string, []agent.ShareMount, error)
	remove(context.Context, string) error
}

func NewEC2(_ context.Context, cfg *config.EC2Config, log *slog.Logger, st *state.State, awsCfg aws.Config) *EC2 {
	if cfg.Region != "" {
		awsCfg.Region = cfg.Region
	}
	b := &EC2{
		cfg: cfg, log: log, state: st, region: awsCfg.Region, poll: 3 * time.Second,
		api:       ec2.NewFromConfig(awsCfg),
		profiles:  newEC2Profiles(cfg, efs.NewFromConfig(awsCfg), iam.NewFromConfig(awsCfg)),
		transport: newEC2Transport(awsCfg, cfg, st),
	}
	b.guestReady = b.waitAgent
	b.growFileSystem = b.growRoot
	b.bootIdentifier = b.bootID
	return b
}

// logger returns the daemon log. A backend built without one, as in tests,
// discards the records.
func (b *EC2) logger() *slog.Logger {
	if b.log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return b.log
}

func ec2ErrCode(err error, code string) bool {
	var ae smithy.APIError
	return errors.As(err, &ae) && ae.ErrorCode() == code
}

func (b *EC2) pause(ctx context.Context) error {
	t := time.NewTimer(b.poll)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (b *EC2) checkLocation(e *state.EC2) error {
	if e == nil || e.ClientToken == "" {
		return errors.New("missing immutable EC2 identity")
	}
	if e.Region != "" && e.Region != b.region {
		return fmt.Errorf("EC2 sandbox belongs to region %s, configured region is %s", e.Region, b.region)
	}
	if e.FileSystemID != "" && e.FileSystemID != b.cfg.FileSystemID {
		return fmt.Errorf("EC2 sandbox belongs to filesystem %s, configured filesystem is %s", e.FileSystemID, b.cfg.FileSystemID)
	}
	return nil
}

// lookup uses the launch token if the daemon stopped after RunInstances
// succeeded but before the instance ID was saved.
func (b *EC2) lookup(ctx context.Context, e *state.EC2) (*types.Instance, error) {
	if err := b.checkLocation(e); err != nil {
		return nil, err
	}
	in := &ec2.DescribeInstancesInput{}
	if e.InstanceID != "" {
		in.InstanceIds = []string{e.InstanceID}
	} else {
		in.Filters = []types.Filter{{Name: aws.String("client-token"), Values: []string{e.ClientToken}}}
	}
	out, err := b.api.DescribeInstances(ctx, in)
	if ec2ErrCode(err, "InvalidInstanceID.NotFound") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, res := range out.Reservations {
		for _, instance := range res.Instances {
			if aws.ToString(instance.ClientToken) != e.ClientToken {
				return nil, fmt.Errorf("EC2 instance %s has a different launch token", aws.ToString(instance.InstanceId))
			}
			return &instance, nil
		}
	}
	return nil, nil
}

func (b *EC2) recordInstance(vm *state.VM, instance *types.Instance) error {
	root := ""
	for _, mapping := range instance.BlockDeviceMappings {
		if aws.ToString(mapping.DeviceName) == aws.ToString(instance.RootDeviceName) && mapping.Ebs != nil {
			root = aws.ToString(mapping.Ebs.VolumeId)
			break
		}
	}
	return b.state.UpdateEC2(vm.Name, func(v *state.VM) {
		v.EC2.InstanceID = aws.ToString(instance.InstanceId)
		v.EC2.LaunchPending = false
		if ip := aws.ToString(instance.PrivateIpAddress); ip != "" {
			v.EC2.PrivateIP = ip
		}
		if instance.InstanceType != "" {
			v.EC2.InstanceType = string(instance.InstanceType)
		}
		if root != "" {
			v.EC2.RootVolumeID = root
		}
	})
}

func instanceState(i *types.Instance) types.InstanceStateName {
	if i == nil || i.State == nil {
		return ""
	}
	return i.State.Name
}

// At launch EC2 is eventually consistent: even an instance whose ID was
// returned by RunInstances may be temporarily absent.
func (b *EC2) waitState(ctx context.Context, e *state.EC2, want types.InstanceStateName) (*types.Instance, error) {
	for {
		i, err := b.lookup(ctx, e)
		if err != nil {
			return nil, err
		}
		got := instanceState(i)
		if got == want || (i == nil && want == types.InstanceStateNameTerminated) {
			return i, nil
		}
		if (got == types.InstanceStateNameTerminated || got == types.InstanceStateNameShuttingDown) && want != types.InstanceStateNameTerminated {
			return nil, fmt.Errorf("EC2 instance %s is %s", e.InstanceID, got)
		}
		if err := b.pause(ctx); err != nil {
			return nil, fmt.Errorf("waiting for EC2 instance %s to be %s (last state %s): %w", e.InstanceID, want, got, err)
		}
	}
}

func (b *EC2) image(ctx context.Context, vm *state.VM) (*types.Image, error) {
	id := vm.EC2.ImageID
	if id == "" {
		id = b.cfg.Images[vm.Image]
	}
	in := &ec2.DescribeImagesInput{}
	if id != "" {
		in.ImageIds = []string{id}
	} else {
		if vm.Image != config.DefaultEC2Image {
			return nil, fmt.Errorf("unknown EC2 image %q", vm.Image)
		}
		owner := "099720109477"
		switch {
		case strings.HasPrefix(b.region, "cn-"):
			owner = "837727238323"
		case strings.HasPrefix(b.region, "us-gov-"):
			owner = "513442679011"
		}
		in.Owners = []string{owner}
		in.Filters = []types.Filter{{Name: aws.String("name"), Values: []string{"ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"}}, {Name: aws.String("state"), Values: []string{"available"}}}
	}
	var latest *types.Image
	for {
		out, err := b.api.DescribeImages(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, img := range out.Images {
			if img.Architecture == types.ArchitectureValuesX8664 && img.RootDeviceType == types.DeviceTypeEbs && (latest == nil || aws.ToString(img.CreationDate) > aws.ToString(latest.CreationDate)) {
				latest = &img
			}
		}
		if aws.ToString(out.NextToken) == "" {
			break
		}
		in.NextToken = out.NextToken
	}
	if latest == nil {
		return nil, fmt.Errorf("no x86_64 EBS-backed AMI for %s", vm.Image)
	}
	return latest, nil
}

func sizeGiB(size int64) (int32, error) {
	const gib = 1 << 30
	if size <= 0 || size%gib != 0 || size/gib > 65536 {
		return 0, errors.New("EC2 disk size must be a positive whole number of GiB, up to 65536 GiB")
	}
	return int32(size / gib), nil
}

func (b *EC2) launch(ctx context.Context, vm *state.VM, profile string, mounts []agent.ShareMount) error {
	img, err := b.image(ctx, vm)
	if err != nil {
		return err
	}
	var block types.BlockDeviceMapping
	for _, m := range img.BlockDeviceMappings {
		if aws.ToString(m.DeviceName) == aws.ToString(img.RootDeviceName) && m.Ebs != nil && aws.ToInt32(m.Ebs.VolumeSize) > 0 {
			size := aws.ToInt32(m.Ebs.VolumeSize)
			if vm.Disk != 0 {
				req, err := sizeGiB(vm.Disk)
				if err != nil {
					return err
				}
				if req < size {
					return fmt.Errorf("EC2 root disk must be at least %d GiB for %s", size, vm.Image)
				}
				size = req
			}
			block = types.BlockDeviceMapping{DeviceName: img.RootDeviceName, Ebs: &types.EbsBlockDevice{VolumeSize: aws.Int32(size), VolumeType: types.VolumeTypeGp3, DeleteOnTermination: aws.Bool(true), Encrypted: aws.Bool(true)}}
			break
		}
	}
	if block.Ebs == nil {
		return errors.New("EC2 image has no root EBS block device")
	}
	if err := b.state.UpdateEC2(vm.Name, func(v *state.VM) {
		v.EC2.ImageID = aws.ToString(img.ImageId)
		v.Disk = int64(*block.Ebs.VolumeSize) << 30
	}); err != nil {
		return err
	}
	data, err := b.userData(ctx, vm, mounts)
	if err != nil {
		return err
	}
	tags := []types.Tag{
		{Key: aws.String("Name"), Value: aws.String(vm.Name)},
		{Key: aws.String("sandboxxing:filesystem"), Value: aws.String(b.cfg.FileSystemID)},
		{Key: aws.String("sandboxxing:instance"), Value: aws.String(vm.EC2.ClientToken)},
	}
	in := &ec2.RunInstancesInput{
		ClientToken: aws.String(vm.EC2.ClientToken),
		MinCount:    aws.Int32(1), MaxCount: aws.Int32(1),
		InstanceType:        types.InstanceType(vm.EC2.InstanceType),
		ImageId:             img.ImageId,
		IamInstanceProfile:  &types.IamInstanceProfileSpecification{Name: aws.String(profile)},
		BlockDeviceMappings: []types.BlockDeviceMapping{block},
		NetworkInterfaces: []types.InstanceNetworkInterfaceSpecification{{
			SubnetId: aws.String(b.cfg.SubnetID), Groups: b.cfg.SecurityGroupIDs, DeviceIndex: aws.Int32(0),
			AssociatePublicIpAddress: aws.Bool(b.cfg.AssociatePublicIP), DeleteOnTermination: aws.Bool(true),
		}},
		MetadataOptions: &types.InstanceMetadataOptionsRequest{HttpTokens: types.HttpTokensStateRequired},
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: tags},
			{ResourceType: types.ResourceTypeVolume, Tags: tags},
		},
		UserData: aws.String(base64.StdEncoding.EncodeToString(data)),
	}
	// A timeout may mean EC2 accepted the request. Keep the launch token
	// recoverable until either RunInstances or DescribeInstances finds it.
	if err := b.state.UpdateEC2(vm.Name, func(v *state.VM) { v.EC2.LaunchPending = true }); err != nil {
		return err
	}
	for {
		out, err := b.api.RunInstances(ctx, in)
		if err != nil {
			if ec2ErrCode(err, "InvalidParameterValue") && strings.Contains(strings.ToLower(err.Error()), "instance profile") {
				if waitErr := b.pause(ctx); waitErr != nil {
					saveErr := b.state.UpdateEC2(vm.Name, func(v *state.VM) { v.EC2.LaunchPending = false })
					return errors.Join(fmt.Errorf("instance profile propagation: %w", err), saveErr)
				}
				continue
			}
			var apiErr smithy.APIError
			if errors.As(err, &apiErr) && apiErr.ErrorFault() == smithy.FaultClient && apiErr.ErrorCode() != "IdempotentParameterMismatch" {
				if saveErr := b.state.UpdateEC2(vm.Name, func(v *state.VM) { v.EC2.LaunchPending = false }); saveErr != nil {
					return errors.Join(err, saveErr)
				}
			}
			return err
		}
		if len(out.Instances) != 1 || aws.ToString(out.Instances[0].InstanceId) == "" {
			return errors.New("RunInstances did not return the new instance ID")
		}
		return b.recordInstance(vm, &out.Instances[0])
	}
}

func (b *EC2) Provision(ctx context.Context, vm *state.VM) error {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.StartTimeout.Duration())
	defer cancel()
	if err := b.checkLocation(vm.EC2); err != nil {
		return err
	}
	if vm.EC2.Region == "" || vm.EC2.FileSystemID == "" {
		if err := b.state.UpdateEC2(vm.Name, func(v *state.VM) { v.EC2.Region = b.region; v.EC2.FileSystemID = b.cfg.FileSystemID }); err != nil {
			return err
		}
		vm, _ = b.state.Get(vm.Name)
	}
	profile, mounts, err := b.profiles.ensure(ctx, vm, vm.EC2.ClientToken)
	if err != nil {
		return err
	}
	i, err := b.lookup(ctx, vm.EC2)
	if err != nil {
		return err
	}
	if i == nil && vm.EC2.InstanceID == "" {
		if err := b.launch(ctx, vm, profile, mounts); err != nil {
			return err
		}
	} else if i != nil {
		if err := b.recordInstance(vm, i); err != nil {
			return err
		}
	}
	vm, _ = b.state.Get(vm.Name)
	return b.startReady(ctx, vm, mounts)
}

func (b *EC2) markReady(ctx context.Context, vm *state.VM, mounts []agent.ShareMount) error {
	waited := time.Now()
	if err := b.guestReady(ctx, vm, mounts); err != nil {
		return err
	}
	b.logger().Info("EC2 guest agent is ready", "name", vm.Name, "instance", vm.EC2.InstanceID,
		"elapsed", time.Since(waited).Round(time.Millisecond))
	progress.From(ctx).Step("connected to the agent of %s", vm.Name)
	if vm.EC2.GrowOnBoot {
		if err := b.growFileSystem(ctx, vm); err != nil {
			return err
		}
	}
	return b.state.UpdateEC2(vm.Name, func(v *state.VM) { v.EC2.Ready = true; v.EC2.GrowOnBoot = false })
}

func (b *EC2) startReady(ctx context.Context, vm *state.VM, mounts []agent.ShareMount) error {
	i, err := b.lookup(ctx, vm.EC2)
	if err != nil {
		return err
	}
	log := b.logger().With("name", vm.Name, "instance", vm.EC2.InstanceID)
	if instanceState(i) == types.InstanceStateNameStopping {
		log.Info("waiting for the EC2 instance to finish stopping before starting it")
		progress.From(ctx).Step("waiting for the EC2 instance of %s to finish stopping", vm.Name)
		i, err = b.waitState(ctx, vm.EC2, types.InstanceStateNameStopped)
		if err != nil {
			return err
		}
	}
	started := time.Now()
	if instanceState(i) == types.InstanceStateNameStopped {
		log.Info("starting the EC2 instance")
		progress.From(ctx).Step("starting the EC2 instance %s of %s", vm.EC2.InstanceID, vm.Name)
		_, err = b.api.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{vm.EC2.InstanceID}})
		if err != nil {
			return err
		}
	} else {
		log.Info("waiting for the EC2 instance to run", "state", instanceState(i))
	}
	i, err = b.waitState(ctx, vm.EC2, types.InstanceStateNameRunning)
	if err != nil {
		return err
	}
	log.Info("EC2 instance is running, waiting for its guest agent", "elapsed", time.Since(started).Round(time.Millisecond))
	progress.From(ctx).Step("the EC2 instance of %s is running, connecting to its agent", vm.Name)
	if err := b.recordInstance(vm, i); err != nil {
		return err
	}
	vm, _ = b.state.Get(vm.Name)
	return b.markReady(ctx, vm, mounts)
}

func (b *EC2) Start(ctx context.Context, vm *state.VM) error {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.StartTimeout.Duration())
	defer cancel()
	if err := b.checkLocation(vm.EC2); err != nil {
		return err
	}
	if vm.EC2.InstanceID == "" {
		return b.Provision(ctx, vm)
	}
	log := b.logger().With("name", vm.Name, "instance", vm.EC2.InstanceID)
	if pending := vm.EC2.PendingResize; pending != nil {
		log.Info("applying the pending resize of the EC2 sandbox before starting it")
		if err := b.Resize(ctx, vm, pending.Disk, pending.InstanceType); err != nil {
			return err
		}
		vm, _ = b.state.Get(vm.Name)
		if pending.WasRunning && vm.EC2.Ready {
			return nil
		}
	}
	log.Info("preparing the instance profile and shared directories of the EC2 sandbox")
	progress.From(ctx).Step("preparing the IAM role and shared directories of %s", vm.Name)
	prepared := time.Now()
	_, mounts, err := b.profiles.ensure(ctx, vm, vm.EC2.ClientToken)
	if err != nil {
		return err
	}
	log.Info("prepared the instance profile and shared directories of the EC2 sandbox",
		"elapsed", time.Since(prepared).Round(time.Millisecond))
	return b.startReady(ctx, vm, mounts)
}

func (b *EC2) Stop(ctx context.Context, vm *state.VM) error { return b.StopForce(ctx, vm, false) }

func (b *EC2) StopForce(ctx context.Context, vm *state.VM, force bool) error {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.StartTimeout.Duration())
	defer cancel()
	i, err := b.lookup(ctx, vm.EC2)
	if err != nil {
		return err
	}
	// An explicit stop supersedes the previous power intent of an interrupted resize.
	if vm.EC2.PendingResize != nil && vm.EC2.PendingResize.WasRunning {
		if err := b.state.UpdateEC2(vm.Name, func(v *state.VM) { v.EC2.PendingResize.WasRunning = false }); err != nil {
			return err
		}
	}
	return b.stopInstance(ctx, vm, i, force)
}

func (b *EC2) stopInstance(ctx context.Context, vm *state.VM, i *types.Instance, force bool) error {
	if err := b.state.UpdateEC2(vm.Name, func(v *state.VM) { v.EC2.Ready = false }); err != nil {
		return err
	}
	if i == nil {
		if vm.EC2.LaunchPending {
			return errors.New("EC2 launch outcome is unknown; retry stop when the instance has appeared")
		}
		return nil
	}
	e := *vm.EC2
	e.InstanceID = aws.ToString(i.InstanceId)
	if instanceState(i) == types.InstanceStateNamePending {
		var err error
		i, err = b.waitState(ctx, &e, types.InstanceStateNameRunning)
		if err != nil {
			return err
		}
	}
	switch instanceState(i) {
	case types.InstanceStateNameStopped, types.InstanceStateNameTerminated, types.InstanceStateNameShuttingDown:
		return nil
	case types.InstanceStateNameStopping:
	case types.InstanceStateNameRunning:
		_, err := b.api.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{e.InstanceID}, SkipOsShutdown: aws.Bool(force)})
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("cannot stop EC2 instance %s in state %q", e.InstanceID, instanceState(i))
	}
	_, err := b.waitState(ctx, &e, types.InstanceStateNameStopped)
	return err
}

func (b *EC2) RemoveForce(ctx context.Context, vm *state.VM, force bool) error {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.StartTimeout.Duration())
	defer cancel()
	i, err := b.lookup(ctx, vm.EC2)
	if err != nil {
		return err
	}
	for i == nil && vm.EC2.LaunchPending {
		// Do not discard the only launch token after an ambiguous RunInstances
		// response. An accepted launch can initially be invisible to Describe.
		if err := b.pause(ctx); err != nil {
			return fmt.Errorf("EC2 launch outcome is still unknown; use start to retry with its original client token: %w", err)
		}
		i, err = b.lookup(ctx, vm.EC2)
		if err != nil {
			return err
		}
	}
	// An interrupted remove must not leave a pending resize that might
	// restart the instance during reconciliation.
	if vm.EC2.PendingResize != nil {
		if err := b.state.UpdateEC2(vm.Name, func(v *state.VM) { v.EC2.PendingResize = nil }); err != nil {
			return err
		}
	}
	if i != nil && instanceState(i) != types.InstanceStateNameTerminated {
		e := *vm.EC2
		e.InstanceID = aws.ToString(i.InstanceId)
		if instanceState(i) != types.InstanceStateNameShuttingDown {
			if err := b.stopInstance(ctx, vm, i, force); err != nil {
				return err
			}
			_, err = b.api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{e.InstanceID}})
			if err != nil {
				return err
			}
		}
		if _, err = b.waitState(ctx, &e, types.InstanceStateNameTerminated); err != nil {
			return err
		}
	}
	return b.profiles.remove(ctx, vm.EC2.ClientToken)
}

func (b *EC2) Restart(ctx context.Context, vm *state.VM) error {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.StartTimeout.Duration())
	defer cancel()
	i, err := b.lookup(ctx, vm.EC2)
	if err != nil {
		return err
	}
	if instanceState(i) != types.InstanceStateNameRunning {
		return b.Start(ctx, vm)
	}
	probe, cancelProbe := context.WithTimeout(ctx, 15*time.Second)
	oldBoot, probeErr := b.bootIdentifier(probe, vm)
	cancelProbe()
	if err := b.state.UpdateEC2(vm.Name, func(v *state.VM) { v.EC2.Ready = false }); err != nil {
		return err
	}
	if _, err := b.api.RebootInstances(ctx, &ec2.RebootInstancesInput{InstanceIds: []string{vm.EC2.InstanceID}}); err != nil {
		return err
	}
	// RebootInstances is asynchronous and the instance state remains running.
	// If the old guest could not identify its boot, a successful ping could
	// still be the old OS before AWS has acted on the request.
	if probeErr != nil {
		return fmt.Errorf("EC2 accepted the reboot of %s; could not verify a new boot because the previous guest was unreachable: %w", vm.Name, probeErr)
	}
	for {
		id, err := b.bootIdentifier(ctx, vm)
		if err == nil && id != "" && id != oldBoot {
			break
		}
		if err := b.pause(ctx); err != nil {
			return fmt.Errorf("waiting for EC2 reboot of %s: %w", vm.Name, err)
		}
	}
	_, mounts, err := b.profiles.ensure(ctx, vm, vm.EC2.ClientToken)
	if err != nil {
		return err
	}
	return b.markReady(ctx, vm, mounts)
}

func (b *EC2) Running(ctx context.Context, vm *state.VM) bool {
	return b.StatusOf(ctx, vm).Running
}
func (b *EC2) StatusOf(ctx context.Context, vm *state.VM) Status {
	i, err := b.lookup(ctx, vm.EC2)
	if err != nil || instanceState(i) != types.InstanceStateNameRunning {
		return Status{}
	}
	return Status{Running: true, Since: aws.ToTime(i.LaunchTime)}
}
