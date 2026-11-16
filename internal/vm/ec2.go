package vm

import (
	"context"
	"errors"
	"fmt"

	"github.com/common-creation/sandboxxing/internal/backend"
	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
)

type ec2Backend interface {
	backend.Backend
	Provision(context.Context, *state.VM) error
	Restart(context.Context, *state.VM) error
	StopForce(context.Context, *state.VM, bool) error
	RemoveForce(context.Context, *state.VM, bool) error
	Resize(context.Context, *state.VM, int64, string) error
}

func (m *Manager) EnableEC2(b ec2Backend) {
	m.ec2 = b
	m.backends[state.BackendEC2] = b
}

// currentEC2 reloads the record under maintenance. A deleted name can have
// been reused while the operation was waiting for an in-flight create/start.
func (m *Manager) currentEC2(previous *state.VM) (*state.VM, error) {
	v, ok := m.state.Get(previous.Name)
	if !ok || v.EC2 == nil || previous.EC2 == nil || v.EC2.ClientToken != previous.EC2.ClientToken {
		return nil, fmt.Errorf("EC2 sandbox %s was removed or replaced", previous.Name)
	}
	return v, nil
}

func (m *Manager) createEC2(ctx context.Context, opts Options) (*state.VM, error) {
	cfg := m.cfg.EC2
	if m.ec2 == nil || cfg == nil {
		return nil, errors.New("the ec2 backend is not configured")
	}
	if opts.CPU != 0 || opts.Memory != 0 || opts.Nested {
		return nil, errors.New("EC2 does not support --cpu, --memory or --nested; select --instance-type")
	}
	if opts.Disk < 0 || opts.Disk%(1<<30) != 0 {
		return nil, errors.New("EC2 disk must be a positive whole number of GiB")
	}
	if opts.Image == "" {
		opts.Image = cfg.DefaultImage
	}
	if _, ok := cfg.Images[opts.Image]; !ok && opts.Image != config.DefaultEC2Image {
		return nil, fmt.Errorf("unknown EC2 image %q", opts.Image)
	}
	if opts.InstanceType == "" {
		opts.InstanceType = cfg.DefaultInstanceType
	}
	after := config.Duration(state.DefaultAutoStop)
	if opts.AutoStop != nil {
		if err := checkAutoStop(state.BackendEC2, *opts.AutoStop); err != nil {
			return nil, err
		}
		after = config.Duration(*opts.AutoStop)
	}
	if err := cfg.CheckShares(config.MergeShares(cfg.Shares, opts.Shares)); err != nil {
		return nil, err
	}
	name, err := m.reserveName(opts.Name)
	if err != nil {
		return nil, err
	}
	l := m.startLock(name)
	l.Lock()
	defer l.Unlock()
	if err := m.checkIdle(name); err != nil {
		return nil, err
	}
	vm := &state.VM{
		Name: name, Image: opts.Image, Disk: opts.Disk, Backend: state.BackendEC2,
		Tags: opts.Tags, Comment: opts.Comment, Env: opts.Env, Shares: opts.Shares, AutoStop: &after,
		EC2: &state.EC2{
			Region:       cfg.Region,
			InstanceType: opts.InstanceType,
			ClientToken:  backend.NewInstanceID(),
			Secret:       backend.NewInstanceID() + backend.NewInstanceID(),
		},
	}
	if err := m.state.Add(vm); err != nil {
		return nil, err
	}
	if err := m.ec2.Provision(ctx, vm); err != nil {
		return nil, fmt.Errorf("provision EC2 sandbox %s: %w (use start to retry or rm to clean up)", name, err)
	}
	m.resetIdle(name)
	created, _ := m.state.Get(name)
	return created, nil
}

// resumeEC2Resizes finishes interrupted operations using their recorded power state.
func (m *Manager) resumeEC2Resizes(ctx context.Context) {
	if m.ec2 == nil {
		return
	}
	for _, vm := range m.state.List() {
		if vm.EC2 == nil || vm.EC2.PendingResize == nil {
			continue
		}
		pending := vm.EC2.PendingResize
		err := m.ec2.Resize(ctx, vm, pending.Disk, pending.InstanceType)
		if err != nil {
			m.log.Error("resume EC2 resize", "name", vm.Name, "err", err)
		}
	}
}

func (m *Manager) resizeEC2(ctx context.Context, vm *state.VM, cpu int, memory, disk int64, instanceType string) error {
	if _, err := m.backendFor(vm); err != nil {
		return err
	}
	if cpu != 0 || memory != 0 || (disk == 0 && instanceType == "") {
		return errors.New("EC2 resize accepts --disk and --instance-type")
	}
	if disk < 0 || disk%(1<<30) != 0 {
		return errors.New("EC2 disk must be a positive whole number of GiB")
	}
	if disk > 0 && disk < vm.Disk {
		return errors.New("disk can only grow")
	}
	typeChange := instanceType != "" && vm.EC2 != nil && instanceType != vm.EC2.InstanceType
	done, err := m.beginOperation(ctx, vm.Name, "resized", typeChange)
	if err != nil {
		return err
	}
	defer done()
	vm, err = m.currentEC2(vm)
	if err != nil {
		return err
	}
	if err := m.ec2.Resize(ctx, vm, disk, instanceType); err != nil {
		return err
	}
	if typeChange {
		m.resetIdle(vm.Name)
	}
	return nil
}
