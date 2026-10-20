package vm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/common-creation/sandboxxing/internal/backend"
	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
)

// EnableMicroVM registers the backend that runs containers as AWS Lambda
// MicroVMs. A container whose MicroVM terminates on its own, which is what
// happens when it reaches its maximum duration, is moved to a new one.
func (m *Manager) EnableMicroVM(b *backend.MicroVM) {
	m.microvm = b
	m.backends[state.BackendMicroVM] = b
	b.OnTerminated = m.migrate
}

// remover is a backend that deletes more than the running container.
type remover interface {
	Remove(ctx context.Context, vm *state.VM) error
}

// createMicroVM registers a container run by the microvm backend. The sizes of
// a MicroVM come from its image and nothing is stored on the host, so only
// the options that apply to it are accepted.
func (m *Manager) createMicroVM(ctx context.Context, opts Options) (*state.VM, error) {
	if m.microvm == nil {
		return nil, errors.New("the microvm backend is not configured")
	}
	switch {
	case opts.CPU > 0 || opts.Memory > 0 || opts.Disk > 0:
		return nil, errors.New("--cpu, --memory and --disk are not available for MicroVMs: the size is set by the MicroVM image")
	case len(opts.Shares) > 0:
		return nil, errors.New("--share is not available for MicroVMs: they cannot reach the host")
	case opts.Nested:
		return nil, errors.New("--nested is not available for MicroVMs")
	}
	autoStop := config.Duration(state.DefaultAutoStop)
	if opts.AutoStop != nil {
		if err := checkAutoStop(state.BackendMicroVM, *opts.AutoStop); err != nil {
			return nil, err
		}
		autoStop = config.Duration(*opts.AutoStop)
	}
	img, err := m.microvm.Image(opts.Image)
	if err != nil {
		return nil, err
	}
	if opts.Image == "" {
		opts.Image = m.cfg.MicroVM.DefaultImage
	}
	name, err := m.reserveName(opts.Name)
	if err != nil {
		return nil, err
	}
	vm := &state.VM{
		Name:     name,
		Image:    opts.Image,
		Comment:  opts.Comment,
		Tags:     opts.Tags,
		Env:      opts.Env,
		Backend:  state.BackendMicroVM,
		AutoStop: &autoStop,
		MicroVM: &state.MicroVM{
			InstanceID:   backend.NewInstanceID(),
			ImageARN:     img.ARN,
			ImageVersion: img.Version,
		},
	}
	if err := m.state.Add(vm); err != nil {
		return nil, err
	}
	if err := m.microvm.Provision(ctx, vm); err != nil {
		// The resources created so far are removed with the record, so that
		// a failed create leaves nothing behind.
		if rmErr := m.Remove(context.WithoutCancel(ctx), name); rmErr != nil {
			m.log.Error("clean up a container that could not be created", "name", name, "err", rmErr)
		}
		return nil, err
	}
	m.log.Info("created container", "name", vm.Name, "backend", vm.Backend, "image", vm.Image)
	if err := m.microvm.Start(ctx, vm); err != nil {
		return vm, fmt.Errorf("container created but failed to start: %w", err)
	}
	m.resetIdle(name)
	return vm, nil
}

// migrateTimeout bounds the start of the next MicroVM.
const migrateTimeout = 10 * time.Minute

// migrate moves a container whose MicroVM terminated to a new MicroVM. The
// home directory lives on EFS and is mounted by the new VM; the processes of
// the old one are gone. Logins wait for the move instead of starting a second
// VM, and the SSH connections to the old VM are closed.
func (m *Manager) migrate(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), migrateTimeout)
	defer cancel()
	done, err := m.beginMaintenance(ctx, name, "migrated to a new MicroVM")
	if err != nil {
		m.log.Warn("cannot migrate a container", "name", name, "err", err)
		return
	}
	defer done()
	vm, ok := m.state.Get(name)
	if !ok {
		return
	}
	m.log.Info("migrating container to a new MicroVM", "name", name)
	if err := m.microvm.Start(ctx, vm); err != nil {
		// The next login tries again.
		m.log.Error("migrate container to a new MicroVM", "name", name, "err", err)
		return
	}
	m.log.Info("migrated container to a new MicroVM", "name", name)
}

// Reconcile brings the recorded MicroVMs up to date after a start of the
// daemon.
func (m *Manager) Reconcile(ctx context.Context) {
	if m.microvm != nil {
		m.microvm.Reconcile(ctx)
	}
}
