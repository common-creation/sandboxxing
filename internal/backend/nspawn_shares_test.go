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
