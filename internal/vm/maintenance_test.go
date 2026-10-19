package vm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
)

func newMaintenanceManager(t *testing.T) *Manager {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	st, err := state.Open(filepath.Join(cfg.DataDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	st.SetSubnet(cfg.Addr())
	if err := st.Add(&state.VM{Name: "demo", Image: "arch", CPU: 1, Memory: 1 << 30, Disk: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	return New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), st)
}

func TestMaintenanceBlocksStartAndClosesConnections(t *testing.T) {
	m := newMaintenanceManager(t)
	var closed []string
	m.SetDisconnect(func(name string) int {
		closed = append(closed, name)
		return 1
	})
	ctx := context.Background()

	done, err := m.beginMaintenance(ctx, "demo", "resized")
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 || closed[0] != "demo" {
		t.Errorf("disconnected %v, want [demo]", closed)
	}

	var busy *BusyError
	if err := m.EnsureStarted(ctx, "demo"); !errors.As(err, &busy) {
		t.Errorf("EnsureStarted during maintenance = %v, want BusyError", err)
	}
	if err := m.Restart(ctx, "demo"); !errors.As(err, &busy) {
		t.Errorf("Restart during maintenance = %v, want BusyError", err)
	}
	if _, err := m.beginMaintenance(ctx, "demo", "resized"); !errors.As(err, &busy) {
		t.Errorf("second maintenance = %v, want BusyError", err)
	}
	if busy == nil || busy.Error() != "container demo is being resized, try again later" {
		t.Errorf("busy message = %v", busy)
	}

	done()
	if err := m.checkIdle("demo"); err != nil {
		t.Errorf("container still busy after the maintenance ended: %v", err)
	}
}
