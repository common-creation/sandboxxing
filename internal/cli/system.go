package cli

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// memoryInfo returns the total and available memory of the host.
func memoryInfo() (total, available int64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			available = v * 1024
		}
	}
	return total, available
}

// diskFree returns the free space in bytes of the file system holding path.
func diskFree(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

func numCPU() (int, error) {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "processor") {
			count++
		}
	}
	if count == 0 {
		return 0, os.ErrNotExist
	}
	return count, nil
}
