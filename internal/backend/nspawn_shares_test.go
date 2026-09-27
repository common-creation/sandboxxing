package backend

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
)

// TestSharesMergesGlobalAndContainer verifies how the bind mounts of one
// container are assembled: the shares configured for the whole host come
// first, a container specific share with the same target replaces it, and the
// order stays stable.
func TestSharesMergesGlobalAndContainer(t *testing.T) {
	n := &Nspawn{
		cfg: &config.Config{Shares: []config.Share{
			{Path: "/srv/projects"},
			{Path: "/srv/cache"},
			{Path: "/srv/readonly", ReadOnly: true},
		}},
		log: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	}
	vm := &state.VM{
		Name: "demo",
		Shares: []config.Share{
			{Path: "/srv/cache", ReadOnly: true},       // replaces the global one
			{Path: "/srv/private", Target: "/private"}, // extra
		},
	}

	got := n.shares(vm)
	var rendered []string
	for _, s := range got {
		rendered = append(rendered, s.String())
	}
	want := []string{
		"/srv/projects:/shared/projects",
		"/srv/cache:/shared/cache:ro",
		"/srv/readonly:/shared/readonly:ro",
		"/srv/private:/private",
	}
	if strings.Join(rendered, ",") != strings.Join(want, ",") {
		t.Errorf("shares = %v, want %v", rendered, want)
	}
}

// TestSharesOrderIsStable makes the generated command line reproducible.
func TestSharesOrderIsStable(t *testing.T) {
	n := &Nspawn{cfg: &config.Config{}, log: slog.New(slog.NewTextHandler(discardWriter{}, nil))}
	vm := &state.VM{Shares: []config.Share{{Path: "/a"}, {Path: "/b"}, {Path: "/c"}}}
	first := n.shares(vm)
	for i := 0; i < 5; i++ {
		next := n.shares(vm)
		if len(next) != len(first) {
			t.Fatalf("length changed: %d -> %d", len(first), len(next))
		}
		for j := range first {
			if first[j].Path != next[j].Path {
				t.Fatalf("order changed at %d: %q -> %q", j, first[j].Path, next[j].Path)
			}
		}
	}
}

// discardWriter swallows the log output of the tests.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestTmpSizeOption verifies the command line that resizes /tmp. nspawn mounts
// /tmp itself with 10% of the host memory, and the custom mount applied later
// is what raises the limit.
func TestTmpSizeOption(t *testing.T) {
	cases := map[string]string{
		"8G":  "--tmpfs=/tmp:mode=01777,size=8G",
		"50%": "--tmpfs=/tmp:mode=01777,size=50%",
	}
	for size, want := range cases {
		n := &Nspawn{
			cfg: &config.Config{TmpSize: size},
			log: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
		}
		args := n.startArgs(&state.VM{Name: "demo", MachineID: "id", Memory: 1 << 30, CPU: 1}, "/var/lib/x.img")
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, want) {
			t.Errorf("tmp_size %q did not produce %q:\n%s", size, want, joined)
		}
	}
}

// TestNoTmpSizeOption keeps the nspawn default when nothing is configured.
func TestNoTmpSizeOption(t *testing.T) {
	n := &Nspawn{cfg: &config.Config{}, log: slog.New(slog.NewTextHandler(discardWriter{}, nil))}
	args := n.startArgs(&state.VM{Name: "demo", MachineID: "id", Memory: 1 << 30, CPU: 1}, "/var/lib/x.img")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--tmpfs=/tmp") {
		t.Errorf("no --tmpfs option expected:\n%s", joined)
	}
	if strings.Contains(joined, "SYSTEMD_NSPAWN_TMPFS_TMP") {
		t.Errorf("no tmpfs opt-out expected:\n%s", joined)
	}
}

// TestTmpOnDiskOption turns the memory backed /tmp off so that /tmp lives on
// the container's disk image. The switch is the environment variable
// SYSTEMD_NSPAWN_TMPFS_TMP, and it must be passed to systemd-run (before the
// systemd-nspawn command) because it configures the unit.
func TestTmpOnDiskOption(t *testing.T) {
	n := &Nspawn{
		cfg: &config.Config{TmpSize: config.TmpSizeDisk},
		log: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	}
	args := n.startArgs(&state.VM{Name: "demo", MachineID: "id", Memory: 1 << 30, CPU: 1}, "/var/lib/x.img")

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "SYSTEMD_NSPAWN_TMPFS_TMP=0") {
		t.Fatalf("the tmpfs opt-out is missing:\n%s", joined)
	}
	if strings.Contains(joined, "--tmpfs=/tmp") {
		t.Errorf("a tmpfs mount must not be requested as well:\n%s", joined)
	}
	envIndex := indexOf(args, "SYSTEMD_NSPAWN_TMPFS_TMP=0")
	nspawnIndex := indexOf(args, "systemd-nspawn")
	if envIndex < 0 || nspawnIndex < 0 || envIndex > nspawnIndex {
		t.Errorf("the variable must precede the systemd-nspawn command:\n%s", joined)
	}
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}
