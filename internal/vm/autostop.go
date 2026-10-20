package vm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/common-creation/sandboxxing/internal/state"
)

// autoStopInterval is how often the containers are checked for autostop.
const autoStopInterval = 30 * time.Second

// MinAutoStop is the shortest autostop time. The containers are only checked
// every autoStopInterval.
const MinAutoStop = time.Minute

// activity is what the SSH gateway knows about the connections of a
// container.
type activity struct {
	// conns counts the open connections.
	conns int
	// idleSince is when the last connection closed, or when the container
	// was started or first seen without one.
	idleSince time.Time
}

// activityLocked returns the activity of a container. A container without a
// record is seen for the first time, which starts its idle time. m.mu is
// held.
func (m *Manager) activityLocked(name string) *activity {
	if m.activity == nil {
		m.activity = map[string]*activity{}
	}
	a, ok := m.activity[name]
	if !ok {
		a = &activity{idleSince: time.Now()}
		m.activity[name] = a
	}
	return a
}

// Connected records an SSH connection to a container and returns the
// function that records its end. A container run by the microvm backend is
// stopped once it had no connection for its autostop time, and every
// connection starts that time anew.
func (m *Manager) Connected(name string) func() {
	m.mu.Lock()
	m.activityLocked(name).conns++
	m.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			a := m.activityLocked(name)
			a.conns--
			a.idleSince = time.Now()
		})
	}
}

// resetIdle starts the idle time of a container that was just started.
func (m *Manager) resetIdle(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activityLocked(name).idleSince = time.Now()
}

// forgetActivity drops the record of a removed container.
func (m *Manager) forgetActivity(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.activity[name]; ok && a.conns == 0 {
		delete(m.activity, name)
	}
}

// idleLocked reports whether a container had no connection for its autostop
// time at now. m.mu is held.
func (m *Manager) idleLocked(name string, after time.Duration, now time.Time) bool {
	if after <= 0 {
		return false
	}
	a := m.activityLocked(name)
	return a.conns == 0 && now.Sub(a.idleSince) >= after
}

// SetAutoStop changes how long a container keeps running without an SSH
// connection. Zero keeps it running.
func (m *Manager) SetAutoStop(name string, d time.Duration) error {
	vm, ok := m.state.Get(name)
	if !ok {
		return fmt.Errorf("container %q not found", name)
	}
	if err := checkAutoStop(vm.BackendName(), d); err != nil {
		return err
	}
	if err := m.state.SetAutoStop(name, d); err != nil {
		return err
	}
	m.log.Info("changed the autostop time", "name", name, "autostop", d)
	return nil
}

// checkAutoStop validates an autostop time for a container of the backend.
func checkAutoStop(backendName string, d time.Duration) error {
	if backendName != state.BackendMicroVM {
		return fmt.Errorf("autostop is only available for the %s backend", state.BackendMicroVM)
	}
	if d != 0 && d < MinAutoStop {
		return fmt.Errorf("autostop must be 0 (never) or at least %s", MinAutoStop)
	}
	return nil
}

// AutoStop stops the containers of the microvm backend that had no SSH
// connection for their autostop time, until ctx ends. The next login starts
// them again.
func (m *Manager) AutoStop(ctx context.Context) {
	if m.microvm == nil {
		return
	}
	t := time.NewTicker(autoStopInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.stopIdle(ctx, now)
		}
	}
}

// stopIdle stops the running MicroVMs that are idle at now.
func (m *Manager) stopIdle(ctx context.Context, now time.Time) {
	for _, vm := range m.state.List() {
		if vm.BackendName() != state.BackendMicroVM {
			continue
		}
		if mv, ok := m.state.MicroVMOf(vm.Name); !ok || mv.ID == "" {
			continue
		}
		after, ok := m.state.AutoStopOf(vm.Name)
		if !ok {
			continue
		}
		m.mu.Lock()
		idle := m.idleLocked(vm.Name, after, now)
		m.mu.Unlock()
		if !idle {
			continue
		}
		if err := m.stopIfIdle(ctx, vm.Name, now); err != nil {
			m.log.Error("stop an idle container", "name", vm.Name, "err", err)
		}
	}
}

// stopIfIdle stops a container that is still idle at now. It holds the
// startLock, so a login that arrives meanwhile waits for the stop and then
// starts the container again.
func (m *Manager) stopIfIdle(ctx context.Context, name string, now time.Time) error {
	l := m.startLock(name)
	l.Lock()
	defer l.Unlock()
	vm, ok := m.state.Get(name)
	if !ok {
		return nil
	}
	after, ok := m.state.AutoStopOf(name)
	if !ok {
		return nil
	}
	// The checks are repeated under the lock: a connection that came in
	// since keeps the container, and so does a move to a new MicroVM.
	m.mu.Lock()
	_, busy := m.busy[name]
	idle := m.idleLocked(name, after, now)
	m.mu.Unlock()
	if busy || !idle {
		return nil
	}
	m.log.Info("stopping a container without SSH connections", "name", name, "autostop", after)
	return m.microvm.Stop(ctx, vm)
}
