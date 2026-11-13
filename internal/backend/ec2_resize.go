package backend

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/common-creation/sandboxxing/internal/state"
)

func (b *EC2) checkInstanceType(ctx context.Context, name string) error {
	out, err := b.api.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{InstanceTypes: []types.InstanceType{types.InstanceType(name)}})
	if err != nil {
		return err
	}
	for _, t := range out.InstanceTypes {
		if string(t.InstanceType) == name && t.ProcessorInfo != nil && slices.Contains(t.ProcessorInfo.SupportedArchitectures, types.ArchitectureTypeX8664) {
			return nil
		}
	}
	return fmt.Errorf("instance type %s is unavailable or incompatible with an x86_64 sandbox", name)
}

func (b *EC2) volumeSize(ctx context.Context, id string) (int32, error) {
	if id == "" {
		return 0, errors.New("no root EBS volume recorded")
	}
	out, err := b.api.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{id}})
	if err != nil {
		return 0, err
	}
	if len(out.Volumes) != 1 || out.Volumes[0].Size == nil {
		return 0, fmt.Errorf("root EBS volume %s not found", id)
	}
	return *out.Volumes[0].Size, nil
}

// growVolume resumes a modification already accepted by EC2 after a daemon
// restart. Size becomes usable in optimizing; completion may take hours.
func (b *EC2) growVolume(ctx context.Context, id string, size int32) error {
	requested := false
	for {
		out, err := b.api.DescribeVolumesModifications(ctx, &ec2.DescribeVolumesModificationsInput{VolumeIds: []string{id}})
		// With explicit volume IDs, a volume which has never been modified
		// has no modification record and EC2 returns a NotFound error.
		if err != nil && !ec2ErrCode(err, "InvalidVolumeModification.NotFound") {
			return err
		}
		if out != nil && len(out.VolumesModifications) > 0 {
			m := out.VolumesModifications[0]
			if aws.ToInt32(m.TargetSize) == size {
				switch m.ModificationState {
				case types.VolumeModificationStateOptimizing, types.VolumeModificationStateCompleted:
					return nil
				case types.VolumeModificationStateModifying:
					requested = true
				case types.VolumeModificationStateFailed:
					if requested {
						return fmt.Errorf("EBS resize failed: %s", aws.ToString(m.StatusMessage))
					}
				}
			} else if m.ModificationState == types.VolumeModificationStateModifying || m.ModificationState == types.VolumeModificationStateOptimizing {
				return fmt.Errorf("another modification of EBS volume %s is still %s", id, m.ModificationState)
			}
		}
		if !requested {
			current, err := b.volumeSize(ctx, id)
			if err != nil {
				return err
			}
			if current > size {
				return fmt.Errorf("disk can only grow: root EBS volume is %d GiB", current)
			}
			if current == size {
				return nil
			}
			if _, err := b.api.ModifyVolume(ctx, &ec2.ModifyVolumeInput{VolumeId: aws.String(id), Size: aws.Int32(size)}); err != nil {
				return err
			}
			requested = true
		}
		if err := b.pause(ctx); err != nil {
			return err
		}
	}
}

// Resize journals the original power state. On retry the AWS instance and
// volume are read afresh, so a completed stop, type change, or grow is not
// repeated. A stopped guest grows its partition on the next boot.
func (b *EC2) Resize(ctx context.Context, vm *state.VM, disk int64, instanceType string) error {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.StartTimeout.Duration())
	defer cancel()
	if disk == 0 && instanceType == "" {
		return errors.New("EC2 resize requires --disk or --instance-type")
	}
	var size int32
	var err error
	if disk != 0 {
		size, err = sizeGiB(disk)
		if err != nil {
			return err
		}
	}
	i, err := b.lookup(ctx, vm.EC2)
	if err != nil {
		return err
	}
	switch instanceState(i) {
	case types.InstanceStateNamePending:
		i, err = b.waitState(ctx, vm.EC2, types.InstanceStateNameRunning)
	case types.InstanceStateNameStopping:
		i, err = b.waitState(ctx, vm.EC2, types.InstanceStateNameStopped)
	case types.InstanceStateNameStopped, types.InstanceStateNameRunning:
	default:
		return fmt.Errorf("cannot resize %s in EC2 state %q", vm.Name, instanceState(i))
	}
	if err != nil {
		return err
	}
	if err := b.recordInstance(vm, i); err != nil {
		return err
	}
	vm, _ = b.state.Get(vm.Name)
	pending := vm.EC2.PendingResize
	// A subsequent explicit resize may replace an invalid or unavailable
	// target type. Keep the disk intent (an accepted EBS extension cannot
	// be rolled back) and the power state from the first attempt.
	if pending != nil && disk == 0 && pending.Disk != 0 {
		disk = pending.Disk
		size, err = sizeGiB(disk)
		if err != nil {
			return err
		}
	}
	if pending == nil || pending.Disk != disk || pending.InstanceType != instanceType {
		if instanceType != "" && string(i.InstanceType) != instanceType {
			if err := b.checkInstanceType(ctx, instanceType); err != nil {
				return err
			}
		}
		if size != 0 {
			current, err := b.volumeSize(ctx, vm.EC2.RootVolumeID)
			if err != nil {
				return err
			}
			if size < current || (pending != nil && disk < pending.Disk) {
				return fmt.Errorf("disk can only grow: root EBS volume is %d GiB", current)
			}
		}
		wasRunning := instanceState(i) == types.InstanceStateNameRunning
		if pending != nil {
			wasRunning = pending.WasRunning
		}
		pending = &state.EC2Resize{Disk: disk, InstanceType: instanceType, WasRunning: wasRunning}
		if err := b.state.UpdateEC2(vm.Name, func(v *state.VM) { v.EC2.PendingResize = pending }); err != nil {
			return err
		}
	}
	if size != 0 {
		if err := b.growVolume(ctx, vm.EC2.RootVolumeID, size); err != nil {
			return err
		}
		if err := b.state.UpdateEC2(vm.Name, func(v *state.VM) { v.EC2.GrowOnBoot = v.EC2.GrowOnBoot || disk > v.Disk; v.Disk = disk }); err != nil {
			return err
		}
		vm, _ = b.state.Get(vm.Name)
		if instanceState(i) == types.InstanceStateNameRunning && vm.EC2.GrowOnBoot {
			if err := b.growFileSystem(ctx, vm); err != nil {
				return err
			}
			if err := b.state.UpdateEC2(vm.Name, func(v *state.VM) { v.EC2.GrowOnBoot = false }); err != nil {
				return err
			}
		}
	}
	if instanceType != "" && string(i.InstanceType) != instanceType {
		if err := b.stopInstance(ctx, vm, i, false); err != nil {
			return err
		}
		_, err = b.api.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{InstanceId: aws.String(vm.EC2.InstanceID), InstanceType: &types.AttributeValue{Value: aws.String(instanceType)}})
		if err != nil {
			return err
		}
		if err := b.state.UpdateEC2(vm.Name, func(v *state.VM) { v.EC2.InstanceType = instanceType }); err != nil {
			return err
		}
	}
	vm, _ = b.state.Get(vm.Name)
	if pending.WasRunning && (!vm.EC2.Ready || instanceState(i) != types.InstanceStateNameRunning) {
		_, mounts, err := b.profiles.ensure(ctx, vm, vm.EC2.ClientToken)
		if err != nil {
			return err
		}
		if err := b.startReady(ctx, vm, mounts); err != nil {
			return err
		}
	}
	return b.state.UpdateEC2(vm.Name, func(v *state.VM) { v.EC2.PendingResize = nil })
}
