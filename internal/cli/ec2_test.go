package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/common-creation/sandboxxing/internal/backend"
	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
	"github.com/common-creation/sandboxxing/internal/vm"
)

// Only the operations used by the control commands are implemented.
type ec2Control struct {
	backend.Backend
	st     *state.State
	calls  []string
	secret string
}

func (b *ec2Control) Provision(_ context.Context, v *state.VM) error {
	b.calls = append(b.calls, "create "+v.EC2.InstanceType)
	b.secret = v.EC2.Secret
	return b.st.UpdateEC2(v.Name, func(v *state.VM) { v.EC2.InstanceID = "i-test"; v.EC2.Ready = true; v.Disk = 8 << 30 })
}
func (b *ec2Control) Resize(_ context.Context, v *state.VM, d int64, t string) error {
	b.calls = append(b.calls, "resize "+t)
	return b.st.UpdateEC2(v.Name, func(v *state.VM) { v.EC2.InstanceType = t; v.Disk = d })
}
func (b *ec2Control) Restart(context.Context, *state.VM) error {
	b.calls = append(b.calls, "restart")
	return nil
}
func (b *ec2Control) StopForce(_ context.Context, _ *state.VM, force bool) error {
	if force {
		b.calls = append(b.calls, "force-stop")
	}
	return nil
}
func (b *ec2Control) RemoveForce(_ context.Context, _ *state.VM, force bool) error {
	if force {
		b.calls = append(b.calls, "force-rm")
	}
	return nil
}
func (b *ec2Control) StatusOf(context.Context, *state.VM) backend.Status { return backend.Status{} }

func TestEC2ControlCommands(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
		"default_backend": "ec2",
		"ec2": {
			"subnet_id": "subnet-test",
			"security_group_ids": ["sg-test"],
			"efs_file_system_id": "fs-test",
			"permissions_boundary_arn": "arn:aws:iam::123:policy/limit",
			"agent_s3_uri": "s3://assets/agent",
			"efs_utils_s3_uri": "s3://assets/efs.deb",
			"shares": [{"path": "/common"}]
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := vm.New(cfg, log, st)
	fake := &ec2Control{st: st}
	manager.EnableEC2(fake)
	r := New(cfg, log, manager, st, "test")
	run := func(want int, args ...string) string {
		t.Helper()
		var out, errs bytes.Buffer
		code := r.Run(context.Background(), args, &IO{In: strings.NewReader(""), Out: &out, Err: &errs})
		if code != want {
			t.Fatalf("%v: exit %d want %d, %s", args, code, want, errs.String())
		}
		if fake.secret != "" && strings.Contains(out.String()+errs.String(), fake.secret) {
			t.Fatalf("%v disclosed the agent secret", args)
		}
		return out.String()
	}
	name := strings.TrimSpace(run(0, "new"))
	if v, ok := st.Get(name); !ok || v.BackendName() != state.BackendEC2 {
		t.Fatalf("new did not create %q with the configured EC2 backend", name)
	}
	out := run(0, "new", "--name=cloud", "--share=/tools:/opt/tools:ro", "--json")
	for _, want := range []string{"m7i-flex.large", "/common", "/opt/tools"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	run(1, "cp", "cloud", "copy")
	run(1, "resize", "cloud", "--cpu=2")
	run(1, "resize", "cloud", "--disk=1G")
	run(1, "start", "--force", "cloud")
	run(0, "resize", "cloud", "--instance-type=m7i-flex.xlarge", "--disk=20G", "--json")
	run(0, "restart", "cloud")
	run(0, "stop", "--force", "cloud")
	run(0, "rm", "cloud", "--force")
	want := []string{"create m7i-flex.large", "create m7i-flex.large", "resize m7i-flex.xlarge", "restart", "force-stop", "force-rm"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("calls %v, want %v", fake.calls, want)
	}
	if _, ok := st.Get("cloud"); ok {
		t.Error("removed EC2 is still present")
	}
}
