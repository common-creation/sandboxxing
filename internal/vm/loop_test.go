package vm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLoopTree builds a /sys/block and /proc tree in which loop3 is backed by
// the returned image and mounted in the namespace of pid 42.
func fakeLoopTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	image := filepath.Join(root, "pentest.img")
	other := filepath.Join(root, "other.img")
	for _, f := range []string{image, other} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, data string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sys := filepath.Join(root, "sys")
	write(filepath.Join(sys, "loop3", "loop", "backing_file"), image+"\n")
	write(filepath.Join(sys, "loop3", "dev"), "7:3\n")
	write(filepath.Join(sys, "loop4", "loop", "backing_file"), other+"\n")
	write(filepath.Join(sys, "loop4", "dev"), "7:4\n")

	proc := filepath.Join(root, "proc")
	process := func(pid, comm, ns, mountinfo string) {
		dir := filepath.Join(proc, pid)
		write(filepath.Join(dir, "comm"), comm+"\n")
		write(filepath.Join(dir, "mountinfo"), mountinfo)
		if err := os.MkdirAll(filepath.Join(dir, "ns"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(ns, filepath.Join(dir, "ns", "mnt")); err != nil {
			t.Fatal(err)
		}
	}
	process("1", "systemd", "mnt:[1]", "22 1 259:2 / / rw - ext4 /dev/nvme0n1p2 rw\n")
	process("42", "bash", "mnt:[9]", "500 400 7:3 / / rw - ext4 /dev/loop3 rw\n")

	sysBlock, procDir = sys, proc
	t.Cleanup(func() { sysBlock, procDir = "/sys/block", "/proc" })
	return image
}

func TestLoopDevicesOf(t *testing.T) {
	image := fakeLoopTree(t)
	loops, err := loopDevicesOf(image)
	if err != nil {
		t.Fatal(err)
	}
	if len(loops) != 1 || loops[0] != "/dev/loop3" {
		t.Errorf("loopDevicesOf = %v, want [/dev/loop3]", loops)
	}
}

func TestLoopHolders(t *testing.T) {
	fakeLoopTree(t)
	holders := loopHolders([]string{"/dev/loop3"})
	if len(holders) != 1 || holders[0] != "pid 42 (bash)" {
		t.Errorf("loopHolders = %v, want [pid 42 (bash)]", holders)
	}
}

func TestWaitImageReleasedFreeImage(t *testing.T) {
	fakeLoopTree(t)
	free := filepath.Join(t.TempDir(), "free.img")
	if err := os.WriteFile(free, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := waitImageReleased(context.Background(), free); err != nil {
		t.Errorf("free image reported busy: %v", err)
	}
}

func TestWaitImageReleasedHonoursContext(t *testing.T) {
	image := fakeLoopTree(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitImageReleased(ctx, image)
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Errorf("busy image with a canceled context: %v", err)
	}
}
