package backend_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	mvtypes "github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"

	"github.com/common-creation/sandboxxing/internal/backend"
	"github.com/common-creation/sandboxxing/internal/backend/microvmtest"
	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
)

const boundary = "arn:aws:iam::123456789012:policy/sandboxxing-boundary"

func testMicroVMConfig() *config.MicroVMConfig {
	return &config.MicroVMConfig{
		Images:                  map[string]config.MicroVMImage{"al2023": {ARN: "arn:aws:lambda:us-east-1:123456789012:microvm-image:sbx"}},
		DefaultImage:            "al2023",
		EgressNetworkConnectors: []string{"arn:aws:lambda:us-east-1:123456789012:network-connector:nc-1"},
		FileSystemID:            "fs-1",
		HomeRoot:                "/homes",
		RolePath:                "/sandboxxing/instances/",
		PermissionsBoundary:     boundary,
		MaximumDuration:         config.Duration(8 * time.Hour),
		TokenTTL:                config.Duration(30 * time.Minute),
		StartTimeout:            config.Duration(10 * time.Second),
	}
}

type fixture struct {
	fake    *microvmtest.Fake
	b       *backend.MicroVM
	st      *state.State
	vm      *state.VM
	home    string
	removed chan string
}

// newFixture registers one MicroVM container and provisions it.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	fake := microvmtest.New()
	t.Cleanup(fake.Close)
	home := t.TempDir()
	b := backend.NewMicroVMWith(ctx, testMicroVMConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), st,
		backend.MicroVMClients{Region: "us-east-1", VMs: fake, EFS: fake, IAM: fake, Poll: 20 * time.Millisecond, Home: home, UnmountedHome: true})
	f := &fixture{fake: fake, b: b, st: st, home: home, removed: make(chan string, 4)}
	b.OnTerminated = func(name string) { f.removed <- name }

	vm := &state.VM{Name: "box", Backend: state.BackendMicroVM, Env: map[string]string{"SBX_TEST": "env"},
		MicroVM: &state.MicroVM{InstanceID: backend.NewInstanceID(), ImageARN: "arn:aws:lambda:us-east-1:123456789012:microvm-image:sbx"}}
	if err := st.Add(vm); err != nil {
		t.Fatal(err)
	}
	f.vm = vm
	if err := b.Provision(ctx, vm); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) current(t *testing.T) state.MicroVM {
	t.Helper()
	mv, ok := f.st.MicroVMOf("box")
	if !ok {
		t.Fatal("the container has no MicroVM state")
	}
	return mv
}

// TestProvisionConfinesTheContainer checks the resources that keep a
// container out of the home directories of the others: its own directory,
// and an execution role that may only mount the access point of that
// directory.
func TestProvisionConfinesTheContainer(t *testing.T) {
	f := newFixture(t)
	mv := f.current(t)
	ap, ok := f.fake.AccessPoints[mv.AccessPointID]
	if !ok {
		t.Fatalf("access point %q was not created", mv.AccessPointID)
	}
	if got, want := aws.ToString(ap.RootDirectory.Path), "/homes/"+mv.InstanceID; got != want {
		t.Errorf("access point root = %q, want %q", got, want)
	}
	if aws.ToInt64(ap.PosixUser.Uid) != 0 {
		t.Errorf("access point user = %d, want root", aws.ToInt64(ap.PosixUser.Uid))
	}

	role, ok := f.fake.Roles[mv.RoleName]
	if !ok {
		t.Fatalf("role %q was not created", mv.RoleName)
	}
	if aws.ToString(role.PermissionsBoundary) != boundary {
		t.Errorf("role boundary = %q", aws.ToString(role.PermissionsBoundary))
	}
	if aws.ToString(role.Path) != "/sandboxxing/instances/" {
		t.Errorf("role path = %q", aws.ToString(role.Path))
	}
	var policy struct {
		Statement []struct {
			Action    any
			Resource  string
			Condition map[string]map[string]string
		}
	}
	if err := json.Unmarshal([]byte(f.fake.Policies[mv.RoleName]), &policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.Statement) != 1 {
		t.Fatalf("policy has %d statements, want only the mount permission: %s", len(policy.Statement), f.fake.Policies[mv.RoleName])
	}
	stmt := policy.Statement[0]
	if got := stmt.Condition["StringEquals"]["elasticfilesystem:AccessPointArn"]; got != mv.AccessPointARN {
		t.Errorf("the mount permission is not limited to the access point of the container: %q", got)
	}
	if !strings.HasSuffix(stmt.Resource, ":file-system/fs-1") {
		t.Errorf("the mount permission covers %q", stmt.Resource)
	}
}

func TestStartRunsWithTheContainerRole(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.b.Start(ctx, f.vm); err != nil {
		t.Fatal(err)
	}
	mv := f.current(t)
	if mv.ID == "" || mv.Generation != 1 {
		t.Fatalf("state after start = %+v", mv)
	}
	run := f.fake.Runs[0]
	if aws.ToString(run.ExecutionRoleArn) != mv.RoleARN {
		t.Errorf("the MicroVM runs with role %q, want %q", aws.ToString(run.ExecutionRoleArn), mv.RoleARN)
	}
	cfg := f.fake.Get(mv.ID).Config
	if cfg.AccessPointID != mv.AccessPointID || cfg.FileSystemID != "fs-1" {
		t.Errorf("run hook payload = %+v", cfg)
	}
	if !f.b.Running(ctx, f.vm) {
		t.Error("the container is not running")
	}

	var out bytes.Buffer
	code, err := f.b.Run(ctx, f.vm, backend.Exec{Argv: []string{"sh", "-c", "echo $SBX_TEST"}, Stdout: &out})
	if err != nil || code != 0 {
		t.Fatalf("run exited with %d, %v", code, err)
	}
	if out.String() != "env\n" {
		t.Errorf("output = %q", out.String())
	}
}

// TestExpiryStartsTheNextMicroVM covers the migration: the platform ends the
// MicroVM, the backend notices and reports it, and the next start runs a new
// MicroVM that mounts the same access point.
func TestExpiryStartsTheNextMicroVM(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.b.Start(ctx, f.vm); err != nil {
		t.Fatal(err)
	}
	first := f.current(t)
	if err := f.fake.Expire(first.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case name := <-f.removed:
		if name != "box" {
			t.Errorf("OnTerminated(%q)", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the termination was not noticed")
	}
	if mv := f.current(t); mv.ID != "" {
		t.Errorf("the terminated MicroVM is still recorded: %+v", mv)
	}
	if home := f.fake.Get(first.ID).Home; home.Unmounts != 1 {
		t.Errorf("the terminate hook unmounted the home directory %d times", home.Unmounts)
	}

	if err := f.b.Start(ctx, f.vm); err != nil {
		t.Fatal(err)
	}
	second := f.current(t)
	if second.ID == first.ID || second.Generation != 2 {
		t.Fatalf("state after the migration = %+v", second)
	}
	if second.Secret == first.Secret {
		t.Error("the new MicroVM reuses the secret of the old one")
	}
	if got := f.fake.Get(second.ID).Config.AccessPointID; got != first.AccessPointID {
		t.Errorf("the new MicroVM mounts %q, want the same access point %q", got, first.AccessPointID)
	}
}

func TestStopDoesNotMigrate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.b.Start(ctx, f.vm); err != nil {
		t.Fatal(err)
	}
	id := f.current(t).ID
	if err := f.b.Stop(ctx, f.vm); err != nil {
		t.Fatal(err)
	}
	if f.fake.StateOf(id) != mvtypes.MicrovmStateTerminated {
		t.Errorf("the MicroVM is %s after stop", f.fake.StateOf(id))
	}
	select {
	case <-f.removed:
		t.Error("a stop by the daemon was treated as a termination")
	case <-time.After(500 * time.Millisecond):
	}
	if f.b.Running(ctx, f.vm) {
		t.Error("the container still runs after stop")
	}
}

// TestAnnouncementAloneIsIgnored makes sure that a termination announced from
// inside the VM, which any user there could fake, does not make the daemon
// start more MicroVMs while the VM is in fact still running.
func TestAnnouncementAloneIsIgnored(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.b.Start(ctx, f.vm); err != nil {
		t.Fatal(err)
	}
	id := f.current(t).ID
	time.Sleep(100 * time.Millisecond) // let the watcher connect
	if err := f.fake.AnnounceTermination(id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.removed:
		t.Fatal("a fake announcement replaced the MicroVM")
	case <-time.After(time.Second):
	}
	if got := f.current(t).ID; got != id {
		t.Errorf("the MicroVM changed from %q to %q", id, got)
	}
	if f.fake.RunCount() != 1 {
		t.Errorf("%d MicroVMs were started, want 1", f.fake.RunCount())
	}
}

func TestRemoveDeletesHomeAndResources(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.b.Start(ctx, f.vm); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.home, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "dir", "file"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	mv := f.current(t)
	if err := f.b.Remove(ctx, f.vm); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the home directory still holds %d entries", len(entries))
	}
	if _, ok := f.fake.AccessPoints[mv.AccessPointID]; ok {
		t.Error("the access point was not deleted")
	}
	if _, ok := f.fake.Roles[mv.RoleName]; ok {
		t.Error("the execution role was not deleted")
	}
	if id := f.current(t).ID; id != "" {
		t.Errorf("MicroVM %s is still recorded", id)
	}
}

func TestRunHookPayloadLimit(t *testing.T) {
	f := newFixture(t)
	big := map[string]string{}
	for i := range 100 {
		big["VAR"+strings.Repeat("X", 10)+string(rune('A'+i%26))+string(rune('a'+i/26))] = strings.Repeat("v", 60)
	}
	if err := f.st.Update("box", func(v *state.VM) error { v.Env = big; return nil }); err != nil {
		t.Fatal(err)
	}
	vm, _ := f.st.Get("box")
	err := f.b.Start(context.Background(), vm)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("start with an oversized environment = %v", err)
	}
	if f.fake.RunCount() != 0 {
		t.Error("a MicroVM was started with an oversized payload")
	}
}

// TestMicroVMShares mounts the shared directories through one access point per
// directory, which the execution roles may mount, read-only ones without the
// permission to write.
func TestMicroVMShares(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	fake := microvmtest.New()
	t.Cleanup(fake.Close)
	cfg := testMicroVMConfig()
	cfg.Shares = []config.Share{{Path: "/shared/data"}}
	home := t.TempDir()
	b := backend.NewMicroVMWith(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), st,
		backend.MicroVMClients{Region: "us-east-1", VMs: fake, EFS: fake, IAM: fake, Poll: 20 * time.Millisecond, Home: home, UnmountedHome: true})

	start := func(name string, shares []config.Share) *state.VM {
		t.Helper()
		vm := &state.VM{Name: name, Backend: state.BackendMicroVM, Shares: shares,
			MicroVM: &state.MicroVM{InstanceID: backend.NewInstanceID(), ImageARN: "arn:aws:lambda:us-east-1:123456789012:microvm-image:sbx"}}
		if err := st.Add(vm); err != nil {
			t.Fatal(err)
		}
		if err := b.Provision(ctx, vm); err != nil {
			t.Fatal(err)
		}
		if err := b.Start(ctx, vm); err != nil {
			t.Fatal(err)
		}
		return vm
	}
	start("one", nil)
	two := start("two", []config.Share{{Path: "/tools", Target: "/opt/tools", ReadOnly: true}})

	shareAPs := map[string]string{}
	for id, ap := range fake.AccessPoints {
		for _, tag := range ap.Tags {
			if aws.ToString(tag.Key) == "sandboxxing:share" {
				if previous, ok := shareAPs[aws.ToString(tag.Value)]; ok {
					t.Errorf("%s has two access points, %s and %s", aws.ToString(tag.Value), previous, id)
				}
				shareAPs[aws.ToString(tag.Value)] = id
			}
		}
	}
	if len(shareAPs) != 2 || shareAPs["/shared/data"] == "" || shareAPs["/tools"] == "" {
		t.Fatalf("access points of the shares = %v", shareAPs)
	}
	if path := aws.ToString(fake.AccessPoints[shareAPs["/tools"]].RootDirectory.Path); path != "/tools" {
		t.Errorf("the access point of /tools is rooted at %s", path)
	}

	mv, _ := st.MicroVMOf("two")
	mounts := fake.Get(mv.ID).Config.Shares
	if len(mounts) != 2 ||
		mounts[0].AccessPointID != shareAPs["/shared/data"] || mounts[0].Target != "/shared/data" || mounts[0].ReadOnly ||
		mounts[1].AccessPointID != shareAPs["/tools"] || mounts[1].Target != "/opt/tools" || !mounts[1].ReadOnly {
		t.Fatalf("shares of two = %+v", mounts)
	}

	var policy struct {
		Statement []struct {
			Action    []string
			Condition map[string]map[string]any
		}
	}
	if err := json.Unmarshal([]byte(fake.Policies[mv.RoleName]), &policy); err != nil {
		t.Fatal(err)
	}
	writable := map[string]bool{}
	mountable := map[string]bool{}
	for _, s := range policy.Statement {
		arns := s.Condition["StringEquals"]["elasticfilesystem:AccessPointArn"]
		var list []string
		switch v := arns.(type) {
		case string:
			list = []string{v}
		case []any:
			for _, a := range v {
				list = append(list, a.(string))
			}
		}
		for _, arn := range list {
			for _, action := range s.Action {
				switch action {
				case "elasticfilesystem:ClientMount":
					mountable[arn] = true
				case "elasticfilesystem:ClientWrite":
					writable[arn] = true
				}
			}
		}
	}
	tools := "arn:aws:elasticfilesystem:us-east-1:" + microvmtest.Account + ":access-point/" + shareAPs["/tools"]
	data := "arn:aws:elasticfilesystem:us-east-1:" + microvmtest.Account + ":access-point/" + shareAPs["/shared/data"]
	if !mountable[tools] || writable[tools] {
		t.Errorf("the read-only share: mount %v, write %v", mountable[tools], writable[tools])
	}
	if !mountable[data] || !writable[data] {
		t.Errorf("the writable share: mount %v, write %v", mountable[data], writable[data])
	}

	// Removing a container keeps the shared directories.
	if err := b.Remove(ctx, two); err != nil {
		t.Fatal(err)
	}
	for dir, id := range shareAPs {
		if _, ok := fake.AccessPoints[id]; !ok {
			t.Errorf("removing a container deleted the access point of %s", dir)
		}
	}

	for _, shares := range [][]config.Share{
		{{Path: "/homes"}},
		{{Path: "/homes/x"}},
		{{Path: "/srv", Target: home}},
		{{Path: "/srv", Target: filepath.Dir(home)}},
		{{Path: "/a"}, {Path: "/b", Target: "/shared/a"}},
		{{Path: "/root/.claude"}},
		{{Path: "/a/b/c/d/e"}},
	} {
		if err := b.CheckShares(shares); err == nil {
			t.Errorf("CheckShares(%v) accepted", shares)
		}
	}
	// A dot directory is a target inside the home directory.
	if err := b.CheckShares([]config.Share{{Path: "/claude", Target: home + "/.claude"}}); err != nil {
		t.Errorf("a share inside the home directory: %v", err)
	}
}

// TestMicroVMRemoveNeverStarted removes a container whose first MicroVM never
// ran without starting one, which is how a failed create is cleaned up.
func TestMicroVMRemoveNeverStarted(t *testing.T) {
	f := newFixture(t)
	if err := f.b.Remove(context.Background(), f.vm); err != nil {
		t.Fatal(err)
	}
	if f.fake.RunCount() != 0 {
		t.Errorf("%d MicroVMs were started to remove an unused container", f.fake.RunCount())
	}
	if len(f.fake.AccessPoints) != 0 || len(f.fake.Roles) != 0 {
		t.Errorf("left behind %d access points and %d roles", len(f.fake.AccessPoints), len(f.fake.Roles))
	}
}
