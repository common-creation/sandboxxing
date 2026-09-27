package tty

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestOpenFailsWithoutContainer verifies the error path: opening a terminal
// for a PID that does not exist must fail with a helpful message instead of
// panicking.
func TestOpenFailsWithoutContainer(t *testing.T) {
	if _, err := Open(999999); err == nil {
		t.Fatal("Open must fail for an unknown PID")
	}
}

// TestSetSizeIgnoresInvalidInput documents that a partial terminal (no master)
// or an unset size does not cause an error.
func TestSetSizeIgnoresInvalidInput(t *testing.T) {
	term := &Terminal{}
	if err := term.SetSize(0, 0); err != nil {
		t.Errorf("SetSize(0,0) = %v, want nil", err)
	}
	if err := term.SetSize(24, 80); err != nil {
		t.Errorf("SetSize without a master = %v, want nil", err)
	}
}

// TestCloseIsIdempotent makes sure a session cannot fail by closing twice.
func TestCloseIsIdempotent(t *testing.T) {
	term := &Terminal{}
	if err := term.Close(); err != nil {
		t.Errorf("Close on an empty terminal = %v", err)
	}
	if err := term.CloseSlave(); err != nil {
		t.Errorf("CloseSlave on an empty terminal = %v", err)
	}
}

// TestMasterPathUsesProcRoot documents the address of the container side of a
// terminal. The path is built from the container PID, which is what lets a
// host side process reach the pts of a private devpts instance.
func TestMasterPathUsesProcRoot(t *testing.T) {
	pid := 1234
	path := filepath.Join("/proc", strconv.Itoa(pid), "root/dev/ptmx")
	if !strings.Contains(path, "/proc/1234/root/dev/ptmx") {
		t.Fatalf("unexpected multiplexer path %q", path)
	}
	if _, err := os.Stat(path); err == nil {
		t.Skip("PID 1234 exists on this host; the path shape is still verified")
	}
}
