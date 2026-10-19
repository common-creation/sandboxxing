package vm

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// sysBlock and procDir are variables so that tests can point them at a fake
// tree.
var (
	sysBlock = "/sys/block"
	procDir  = "/proc"
)

// imageReleaseTimeout is how long a stopped container may take to release its
// disk image. The loop device of systemd-nspawn is detached once the last
// mount of the image is gone, which happens shortly after the unit stopped.
const imageReleaseTimeout = 30 * time.Second

// waitImageReleased waits until no loop device is backed by the disk image.
// e2fsck(8), resize2fs(8) and a loop mount of the daemon do not notice a mount
// that lives in the mount namespace of a container, so they would work on a
// file system that the kernel still writes to and corrupt it. The image is
// only touched offline once it is free.
func waitImageReleased(ctx context.Context, diskPath string) error {
	deadline := time.Now().Add(imageReleaseTimeout)
	for {
		loops, err := loopDevicesOf(diskPath)
		if err != nil {
			return err
		}
		if len(loops) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			msg := fmt.Sprintf("disk image %s is still attached to %s", diskPath, strings.Join(loops, ", "))
			if holders := loopHolders(loops); len(holders) > 0 {
				msg += "; it is mounted by " + strings.Join(holders, ", ")
			}
			return fmt.Errorf("%s: stop these processes and try again", msg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// loopDevicesOf returns the loop devices (e.g. "/dev/loop3") whose backing
// file is the given file.
func loopDevicesOf(path string) ([]string, error) {
	target, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect disk image %s: %w", path, err)
	}
	files, err := filepath.Glob(filepath.Join(sysBlock, "loop*", "loop", "backing_file"))
	if err != nil {
		return nil, err
	}
	var loops []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			// The device was detached while we were looking.
			continue
		}
		backing := strings.TrimSpace(string(b))
		fi, err := os.Stat(backing)
		if err != nil || !os.SameFile(target, fi) {
			continue
		}
		name := filepath.Base(filepath.Dir(filepath.Dir(f)))
		loops = append(loops, "/dev/"+name)
	}
	sort.Strings(loops)
	return loops, nil
}

// loopHolders describes the processes whose mount namespace has one of the
// loop devices mounted, as "pid 123 (bash)". Processes started in a container
// with nsenter(1) belong to the daemon rather than to the container unit, so
// they can outlive the container and keep its file system mounted. The result
// is best effort and only used to explain why an image is busy.
func loopHolders(loops []string) []string {
	devs := map[string]bool{}
	for _, loop := range loops {
		b, err := os.ReadFile(filepath.Join(sysBlock, filepath.Base(loop), "dev"))
		if err == nil {
			devs[strings.TrimSpace(string(b))] = true
		}
	}
	if len(devs) == 0 {
		return nil
	}
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil
	}
	// Every process of a mount namespace sees the same mounts, so each
	// namespace is only inspected once.
	seen := map[string]bool{}
	var holders []string
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(procDir, e.Name())
		ns, err := os.Readlink(filepath.Join(dir, "ns", "mnt"))
		if err != nil {
			continue
		}
		mounted, ok := seen[ns]
		if !ok {
			mounted = mountsAny(filepath.Join(dir, "mountinfo"), devs)
			seen[ns] = mounted
		}
		if !mounted {
			continue
		}
		comm, _ := os.ReadFile(filepath.Join(dir, "comm"))
		holders = append(holders, fmt.Sprintf("pid %d (%s)", pid, strings.TrimSpace(string(comm))))
	}
	return holders
}

// mountsAny reports whether a mountinfo file lists a mount of one of the
// devices, given as "major:minor".
func mountsAny(mountinfo string, devs map[string]bool) bool {
	f, err := os.Open(mountinfo)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// mount ID, parent ID, major:minor, root, mount point, ...
		fields := strings.Fields(sc.Text())
		if len(fields) > 2 && devs[fields[2]] {
			return true
		}
	}
	return false
}
