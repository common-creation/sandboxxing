package sshserver

import (
	"strings"
	"testing"
)

// TestEnvironmentForwardsTermAndEnv covers the missing TERM propagation that
// made full screen applications (htop, vim, less) draw nothing inside a
// container: the terminal type from the pty request and the variables the
// client sent with env requests must be handed to the program.
func TestEnvironmentForwardsTermAndEnv(t *testing.T) {
	session := &sessionRequest{env: map[string]string{
		"LANG":      "ja_JP.UTF-8",
		"COLORTERM": "truecolor",
	}}
	session.term = "xterm-256color"

	env := session.environment()
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"TERM=xterm-256color",
		"LANG=ja_JP.UTF-8",
		"COLORTERM=truecolor",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in the environment:\n%s", want, joined)
		}
	}

	// TERM comes first so that it wins over a forwarded TERM.
	if len(env) == 0 || !strings.HasPrefix(env[0], "TERM=") {
		t.Errorf("TERM should be the first entry: %v", env)
	}
}

// TestEnvironmentIsEmptyWithoutRequests makes sure the container keeps its own
// defaults when the client sent neither a pty request nor env requests.
func TestEnvironmentIsEmptyWithoutRequests(t *testing.T) {
	session := &sessionRequest{env: map[string]string{}}
	if env := session.environment(); len(env) != 0 {
		t.Errorf("environment = %v, want none", env)
	}
}

// TestEnvironmentIsSorted keeps the output stable, which makes the generated
// command line predictable in tests and logs.
func TestEnvironmentIsSorted(t *testing.T) {
	session := &sessionRequest{env: map[string]string{
		"ZED":   "1",
		"ALPHA": "2",
		"MID":   "3",
	}}
	session.term = "vt100"
	env := session.environment()
	want := []string{"TERM=vt100", "ALPHA=2", "MID=3", "ZED=1"}
	if strings.Join(env, ",") != strings.Join(want, ",") {
		t.Errorf("environment = %v, want %v", env, want)
	}
}
