package backend

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
)

func testNspawn(cfg *config.Config) *Nspawn {
	return &Nspawn{cfg: cfg, log: slog.New(slog.NewTextHandler(discardWriter{}, nil))}
}

func testVM() *state.VM {
	return &state.VM{Name: "demo", MachineID: "id", Memory: 1 << 30, CPU: 1}
}

// TestNestedSyscallFilterIsRequired covers the setting that makes Docker work
// inside a container: nspawn's seccomp filter blocks bpf(), which runc needs
// for the cgroup v2 device controller. The host wide option must add the
// filter even when the container itself did not opt in.
func TestNestedSyscallFilterIsRequired(t *testing.T) {
	args := testNspawn(&config.Config{Nested: true}).startArgs(testVM(), "/var/lib/x.img")
	joined := strings.Join(args, " ")
	for _, want := range []string{"--system-call-filter=@keyring", "--system-call-filter=bpf"} {
		if !strings.Contains(joined, want) {
			t.Errorf("nested start is missing %q:\n%s", want, joined)
		}
	}
}

// TestNestedPerContainer verifies `new --nested`: the flag recorded on the
// container is enough, even without the host wide option.
func TestNestedPerContainer(t *testing.T) {
	vm := testVM()
	vm.Nested = true
	args := testNspawn(&config.Config{}).startArgs(vm, "/var/lib/x.img")
	if !strings.Contains(strings.Join(args, " "), "--system-call-filter=bpf") {
		t.Errorf("a container created with --nested must allow bpf:\n%v", args)
	}
}

// TestPlainContainerKeepsSeccomp verifies that nothing is relaxed unless
// nesting was requested, so a normal sandbox keeps the default filter.
func TestPlainContainerKeepsSeccomp(t *testing.T) {
	args := testNspawn(&config.Config{}).startArgs(testVM(), "/var/lib/x.img")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--system-call-filter") {
		t.Errorf("a plain container must keep the seccomp filter:\n%s", joined)
	}
	if strings.Contains(joined, "DeviceAllow="+fuseDevice) {
		t.Errorf("a plain container must not be granted /dev/fuse:\n%s", joined)
	}
}

// TestNestedFuseHandling ties the /dev/fuse pass-through to the host: the bind
// mount and the device permission are only added when the device exists.
func TestNestedFuseHandling(t *testing.T) {
	args := nestedArgs()
	joined := strings.Join(args, " ")
	want := fuseAvailable()
	if got := strings.Contains(joined, "--bind="+fuseDevice); got != want {
		t.Errorf("/dev/fuse bind = %v, want %v (host has device: %v): %s", got, want, want, joined)
	}
}
