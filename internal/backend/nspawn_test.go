package backend

import (
	"strings"
	"testing"
)

// TestNsenterArgsWorkingDirectory covers the login directory: a session must
// start in the user's home, and an empty Dir must keep the previous behaviour
// of starting in the container root. nsenter refuses to run when the directory
// does not exist, so the value is passed through verbatim.
func TestNsenterArgsWorkingDirectory(t *testing.T) {
	args := nsenterArgs(1234, "/root")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--wdns=/root") {
		t.Errorf("a requested directory must be used: %v", args)
	}
	if args[len(args)-1] != "--" {
		t.Errorf("the argument list must end with --: %v", args)
	}

	args = nsenterArgs(1234, "")
	if !strings.Contains(strings.Join(args, " "), "--wdns=/") {
		t.Errorf("an empty directory must fall back to /: %v", args)
	}
}
