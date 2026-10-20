package vm

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/common-creation/sandboxxing/internal/backend"
	"github.com/common-creation/sandboxxing/internal/backend/microvmtest"
	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
)

func newMicroVMManager(t *testing.T) (*Manager, *microvmtest.Fake, *state.State) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.MicroVM = &config.MicroVMConfig{
		Images:                  map[string]config.MicroVMImage{"al2023": {ARN: "arn:aws:lambda:us-east-1:123456789012:microvm-image:sbx"}},
		EgressNetworkConnectors: []string{"arn:aws:lambda:us-east-1:123456789012:network-connector:nc-1"},
		FileSystemID:            "fs-1",
		PermissionsBoundary:     "arn:aws:iam::123456789012:policy/boundary",
		StartTimeout:            config.Duration(10 * time.Second),
	}
	cfg.DefaultBackend = state.BackendMicroVM
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(cfg.DataDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	st.SetSubnet(cfg.Addr())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := microvmtest.New()
	t.Cleanup(fake.Close)
	m := New(cfg, log, st)
	m.EnableMicroVM(backend.NewMicroVMWith(ctx, cfg.MicroVM, log, st, backend.MicroVMClients{
		Region: "us-east-1", VMs: fake, EFS: fake, IAM: fake, Poll: 20 * time.Millisecond, Home: t.TempDir(), UnmountedHome: true,
	}))
	return m, fake, st
}

// TestMicroVMIsMigratedWhenItExpires follows a container through the end of
// its MicroVM: the daemon starts the next one by itself, closes the logins to
// the old one, and commands keep working.
func TestMicroVMIsMigratedWhenItExpires(t *testing.T) {
	m, fake, st := newMicroVMManager(t)
	var closed []string
	m.SetDisconnect(func(name string) int {
		closed = append(closed, name)
		return 1
	})
	ctx := context.Background()
	vm, err := m.Create(ctx, Options{Name: "box"})
	if err != nil {
		t.Fatal(err)
	}
	if vm.IP != "" {
		t.Errorf("a MicroVM got bridge address %s", vm.IP)
	}
	first, _ := st.MicroVMOf("box")
	if err := fake.Expire(first.ID); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		mv, _ := st.MicroVMOf("box")
		if mv.ID != "" && mv.ID != first.ID && m.checkIdle("box") == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the container was not migrated: %+v", mv)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(closed) != 1 || closed[0] != "box" {
		t.Errorf("closed connections of %v, want box", closed)
	}
	var out bytes.Buffer
	if code, err := m.Run(ctx, "box", Exec{Argv: []string{"echo", "ok"}, Stdout: &out}); err != nil || code != 0 {
		t.Fatalf("run after the migration exited with %d, %v", code, err)
	}
	if out.String() != "ok\n" {
		t.Errorf("output = %q", out.String())
	}
	if fake.RunCount() != 2 {
		t.Errorf("%d MicroVMs were started, want 2", fake.RunCount())
	}
}

func TestMicroVMRejectsHostOptions(t *testing.T) {
	m, _, _ := newMicroVMManager(t)
	ctx := context.Background()
	for _, opts := range []Options{
		{Name: "a", CPU: 2},
		{Name: "b", Disk: 1 << 30},
		{Name: "c", Shares: []config.Share{{Path: "/homes"}}},
		{Name: "d", Nested: true},
		{Name: "e", Image: "missing"},
	} {
		if _, err := m.Create(ctx, opts); err == nil {
			t.Errorf("Create(%+v) succeeded", opts)
		}
	}
	if _, err := m.Create(ctx, Options{Name: "f"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Resize(ctx, "f", 2, 0, 0); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("resize of a MicroVM = %v", err)
	}
	if _, err := m.Copy(ctx, "f", Options{Name: "g"}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("copy of a MicroVM = %v", err)
	}
}

func TestMicroVMRemove(t *testing.T) {
	m, fake, st := newMicroVMManager(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, Options{Name: "box"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx, "box"); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Get("box"); ok {
		t.Error("the container is still recorded")
	}
	if len(fake.AccessPoints) != 0 || len(fake.Roles) != 0 {
		t.Errorf("left behind %d access points and %d roles", len(fake.AccessPoints), len(fake.Roles))
	}
}

// TestMicroVMAutoStop stops a MicroVM that had no SSH connection for its
// autostop time and starts it again on the next use.
func TestMicroVMAutoStop(t *testing.T) {
	m, fake, st := newMicroVMManager(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, Options{Name: "box"}); err != nil {
		t.Fatal(err)
	}
	if after, _ := st.AutoStopOf("box"); after != state.DefaultAutoStop {
		t.Fatalf("autostop = %s, want %s", after, state.DefaultAutoStop)
	}
	running := func() bool {
		mv, _ := st.MicroVMOf("box")
		return mv.ID != ""
	}
	m.stopIdle(ctx, time.Now().Add(state.DefaultAutoStop-time.Minute))
	if !running() {
		t.Fatal("stopped before the autostop time")
	}
	m.stopIdle(ctx, time.Now().Add(state.DefaultAutoStop+time.Minute))
	if running() {
		t.Fatal("not stopped after the autostop time")
	}
	if code, err := m.Run(ctx, "box", Exec{Argv: []string{"true"}}); err != nil || code != 0 {
		t.Fatalf("run after the stop exited with %d, %v", code, err)
	}
	if !running() || fake.RunCount() != 2 {
		t.Errorf("running %v after %d MicroVMs, want a second one", running(), fake.RunCount())
	}
}

// TestMicroVMAutoStopWaitsForConnections keeps a MicroVM while a connection
// is open and counts the autostop time from its end.
func TestMicroVMAutoStopWaitsForConnections(t *testing.T) {
	m, _, st := newMicroVMManager(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, Options{Name: "box"}); err != nil {
		t.Fatal(err)
	}
	running := func() bool {
		mv, _ := st.MicroVMOf("box")
		return mv.ID != ""
	}
	// The container has been idle for long when the connection comes in.
	m.mu.Lock()
	m.activityLocked("box").idleSince = time.Now().Add(-2 * time.Hour)
	m.mu.Unlock()
	release := m.Connected("box")
	m.stopIdle(ctx, time.Now().Add(10*time.Hour))
	if !running() {
		t.Fatal("stopped while connected")
	}
	release()
	release()
	m.stopIdle(ctx, time.Now().Add(state.DefaultAutoStop-time.Minute))
	if !running() {
		t.Fatal("the end of the connection did not restart the autostop time")
	}
	m.stopIdle(ctx, time.Now().Add(state.DefaultAutoStop+time.Minute))
	if running() {
		t.Fatal("not stopped after the autostop time")
	}
}

func TestMicroVMAutoStopSetting(t *testing.T) {
	m, _, st := newMicroVMManager(t)
	ctx := context.Background()
	never := time.Duration(0)
	if _, err := m.Create(ctx, Options{Name: "box", AutoStop: &never}); err != nil {
		t.Fatal(err)
	}
	m.stopIdle(ctx, time.Now().Add(100*time.Hour))
	if mv, _ := st.MicroVMOf("box"); mv.ID == "" {
		t.Fatal("stopped with autostop 0")
	}
	if err := m.SetAutoStop("box", time.Hour); err != nil {
		t.Fatal(err)
	}
	if after, _ := st.AutoStopOf("box"); after != time.Hour {
		t.Errorf("autostop = %s after the edit, want 1h", after)
	}
	m.stopIdle(ctx, time.Now().Add(2*time.Hour))
	if mv, _ := st.MicroVMOf("box"); mv.ID != "" {
		t.Error("not stopped after the edited autostop time")
	}

	short := 10 * time.Second
	if err := m.SetAutoStop("box", short); err == nil {
		t.Error("accepted an autostop time below the minimum")
	}
	if _, err := m.Create(ctx, Options{Name: "short", AutoStop: &short}); err == nil {
		t.Error("created a container with an autostop time below the minimum")
	}
	hour := time.Hour
	if _, err := m.Create(ctx, Options{Name: "host", Backend: state.BackendNspawn, AutoStop: &hour}); err == nil ||
		!strings.Contains(err.Error(), "only available") {
		t.Errorf("autostop for a systemd-nspawn container = %v", err)
	}
}

// TestMicroVMStartStop stops a MicroVM on request, closing its connections,
// and starts it again.
func TestMicroVMStartStop(t *testing.T) {
	m, fake, st := newMicroVMManager(t)
	var closed []string
	m.SetDisconnect(func(name string) int {
		closed = append(closed, name)
		return 1
	})
	ctx := context.Background()
	if _, err := m.Create(ctx, Options{Name: "box"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(ctx, "box"); err != nil {
		t.Fatal(err)
	}
	if mv, _ := st.MicroVMOf("box"); mv.ID != "" {
		t.Fatalf("MicroVM %s still recorded after stop", mv.ID)
	}
	if len(closed) != 1 || closed[0] != "box" {
		t.Errorf("closed connections of %v, want box", closed)
	}
	// Stopping a stopped container does nothing.
	if err := m.Stop(ctx, "box"); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx, "box"); err != nil {
		t.Fatal(err)
	}
	if mv, _ := st.MicroVMOf("box"); mv.ID == "" || fake.RunCount() != 2 {
		t.Errorf("not running after start: %+v, %d MicroVMs", mv, fake.RunCount())
	}
	if err := m.Stop(ctx, "missing"); err == nil {
		t.Error("stopped a container that does not exist")
	}
}
