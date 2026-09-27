package backend

import (
	"strings"
	"testing"
)

// TestMergeEnvReplacesInsteadOfDuplicating verifies the environment that is
// handed to nsenter: the daemon environment forms the base so that PATH and
// HOME survive, and the values collected from the SSH session replace the
// entries with the same name. A duplicate would leave the effective value to
// the reader, which is not something a login can rely on.
func TestMergeEnvReplacesInsteadOfDuplicating(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/root", "TERM=dumb", "LANG=C"}
	extra := []string{"TERM=xterm-256color", "LANG=ja_JP.UTF-8"}

	merged := mergeEnv(base, extra)
	joined := strings.Join(merged, "\n")

	if strings.Count(joined, "TERM=") != 1 {
		t.Errorf("TERM appears more than once:\n%s", joined)
	}
	if !strings.Contains(joined, "TERM=xterm-256color") {
		t.Errorf("the session TERM did not win:\n%s", joined)
	}
	if !strings.Contains(joined, "LANG=ja_JP.UTF-8") {
		t.Errorf("the session LANG did not win:\n%s", joined)
	}
	if !strings.Contains(joined, "HOME=/root") || !strings.Contains(joined, "PATH=/usr/bin") {
		t.Errorf("base variables were lost:\n%s", joined)
	}
}

// TestMergeEnvWithoutExtra keeps the original environment untouched.
func TestMergeEnvWithoutExtra(t *testing.T) {
	base := []string{"PATH=/usr/bin", "TERM=xterm"}
	merged := mergeEnv(base, nil)
	if strings.Join(merged, ",") != strings.Join(base, ",") {
		t.Errorf("mergeEnv = %v, want %v", merged, base)
	}
}

// TestMergeEnvIgnoresMalformedEntries makes a broken request harmless.
func TestMergeEnvIgnoresMalformedEntries(t *testing.T) {
	merged := mergeEnv([]string{"PATH=/usr/bin"}, []string{"NOEQUALS", "A=1"})
	joined := strings.Join(merged, "\n")
	if !strings.Contains(joined, "PATH=/usr/bin") || !strings.Contains(joined, "A=1") {
		t.Errorf("unexpected result:\n%s", joined)
	}
}
