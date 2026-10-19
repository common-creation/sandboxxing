package vm

import (
	"context"
	"fmt"
	"sync"

	"github.com/common-creation/sandboxxing/internal/progress"
)

// BusyError reports that a container is in the middle of an offline operation
// on its disk image and cannot be started or entered right now.
type BusyError struct {
	Name      string
	Operation string
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("container %s is being %s, try again later", e.Name, e.Operation)
}

// SetDisconnect installs the function that closes the SSH connections of a
// container and returns how many were closed. The SSH server installs it, so
// that an offline operation can make sure nobody is left inside the container.
func (m *Manager) SetDisconnect(fn func(name string) int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disconnect = fn
}

// startLock returns the mutex that serialises starting a container with the
// beginning of an offline operation, so that a container booted on demand by
// a new connection cannot slip in after the operation has checked it.
func (m *Manager) startLock(name string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks == nil {
		m.locks = map[string]*sync.Mutex{}
	}
	l, ok := m.locks[name]
	if !ok {
		l = &sync.Mutex{}
		m.locks[name] = l
	}
	return l
}

// checkIdle fails with a BusyError while an offline operation runs on the
// container. Callers that start the container hold its startLock.
func (m *Manager) checkIdle(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if op, ok := m.busy[name]; ok {
		return &BusyError{Name: name, Operation: op}
	}
	return nil
}

// beginMaintenance marks the container as busy with an offline operation on
// its disk image, described by op ("resized"). From then on the container is
// not started on demand, and the SSH connections that are already open are
// closed, so that neither a new nor an existing login can keep the file system
// mounted while the image is worked on. The returned function ends the
// operation.
func (m *Manager) beginMaintenance(ctx context.Context, name, op string) (func(), error) {
	l := m.startLock(name)
	l.Lock()
	m.mu.Lock()
	if current, ok := m.busy[name]; ok {
		m.mu.Unlock()
		l.Unlock()
		return nil, &BusyError{Name: name, Operation: current}
	}
	if m.busy == nil {
		m.busy = map[string]string{}
	}
	m.busy[name] = op
	disconnect := m.disconnect
	m.mu.Unlock()
	l.Unlock()

	if disconnect != nil {
		if n := disconnect(name); n > 0 {
			progress.From(ctx).Step("closed %d SSH connection(s) to %s", n, name)
			m.log.Info("closed the SSH connections of a container", "name", name, "connections", n, "reason", op)
		}
	}
	return func() {
		m.mu.Lock()
		delete(m.busy, name)
		m.mu.Unlock()
	}, nil
}
