package vm

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
)

type idleEC2 struct {
	ec2Backend
	running bool
	stops   int
}

func (b *idleEC2) Running(context.Context, *state.VM) bool { return b.running }
func (b *idleEC2) Stop(context.Context, *state.VM) error   { b.stops++; b.running = false; return nil }

func TestEC2AutoStop(t *testing.T) {
	ctx := context.Background()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := New(config.Default(), slog.New(slog.NewTextHandler(io.Discard, nil)), st)
	b := &idleEC2{running: true}
	m.EnableEC2(b)
	if err := st.Add(&state.VM{Name: "box", Backend: state.BackendEC2, EC2: &state.EC2{InstanceID: "i-test", Ready: true}}); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * state.DefaultAutoStop)
	disconnect := m.Connected("box")
	m.stopIdle(ctx, later)
	disconnect()
	if b.stops != 0 {
		t.Fatal("stopped during SSH session")
	}
	end, err := m.beginMaintenance(ctx, "box", "resized")
	if err != nil {
		t.Fatal(err)
	}
	m.stopIdle(ctx, later)
	end()
	if b.stops != 0 {
		t.Fatal("stopped during resize")
	}
	if err := m.SetAutoStop("box", 0); err != nil {
		t.Fatal(err)
	}
	m.stopIdle(ctx, later)
	if b.stops != 0 {
		t.Fatal("stopped with autostop disabled")
	}
	if err := m.SetAutoStop("box", state.DefaultAutoStop); err != nil {
		t.Fatal(err)
	}
	m.stopIdle(ctx, later)
	m.stopIdle(ctx, later)
	if b.stops != 1 {
		t.Fatalf("stop calls = %d, want 1", b.stops)
	}
}
