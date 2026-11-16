package state

import (
	"fmt"
	"maps"
	"slices"
)

type EC2 struct {
	LaunchPending bool       `json:"launch_pending,omitempty"`
	Region        string     `json:"region"`
	FileSystemID  string     `json:"efs_file_system_id,omitempty"`
	ClientToken   string     `json:"client_token"`
	Secret        string     `json:"secret,omitempty"`
	InstanceID    string     `json:"instance_id,omitempty"`
	PrivateIP     string     `json:"private_ip,omitempty"`
	RootVolumeID  string     `json:"root_volume_id,omitempty"`
	ImageID       string     `json:"image_id,omitempty"`
	InstanceType  string     `json:"instance_type"`
	Ready         bool       `json:"ready,omitempty"`
	PendingResize *EC2Resize `json:"pending_resize,omitempty"`
	GrowOnBoot    bool       `json:"grow_on_boot,omitempty"`
}

// EC2Resize records the intended final state before the first AWS change.
type EC2Resize struct {
	Disk         int64  `json:"disk,omitempty"`
	InstanceType string `json:"instance_type,omitempty"`
	WasRunning   bool   `json:"was_running"`
}

func ec2Snapshot(vm *VM) *VM {
	if vm == nil || vm.EC2 == nil {
		return vm
	}
	v, e := *vm, *vm.EC2
	if e.PendingResize != nil {
		r := *e.PendingResize
		e.PendingResize = &r
	}
	v.EC2 = &e
	v.Tags = slices.Clone(v.Tags)
	v.Shares = slices.Clone(v.Shares)
	v.Env = maps.Clone(v.Env)
	if v.AutoStop != nil {
		a := *v.AutoStop
		v.AutoStop = &a
	}
	return &v
}

func (s *State) EC2Of(name string) (EC2, bool) {
	v, ok := s.Get(name)
	if !ok || v.EC2 == nil {
		return EC2{}, false
	}
	return *v.EC2, true
}

// UpdateEC2 commits a complete snapshot only if the state file can be saved.
func (s *State) UpdateEC2(name string, f func(*VM)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.VMs[name]
	if old == nil || old.EC2 == nil {
		return fmt.Errorf("no EC2 state for %s", name)
	}
	v := ec2Snapshot(old)
	f(v)
	s.VMs[name] = v
	if err := s.saveLocked(); err != nil {
		s.VMs[name] = old
		return err
	}
	return nil
}
