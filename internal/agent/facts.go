package agent

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// load is the one-minute load average.
func load() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	f, _ := strconv.ParseFloat(fields[0], 64)
	return f
}

// memory is the machine's memory in use and in all, in bytes.
func memory() (used, total int64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	values := map[string]int64{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			n, _ := strconv.ParseInt(fields[1], 10, 64)
			values[strings.TrimSuffix(fields[0], ":")] = n * 1024
		}
	}
	total = values["MemTotal"]
	return total - values["MemAvailable"], total
}

// disk is the node folder's file system's space in use and in all.
func disk(path string) (used, total int64) {
	var s syscall.Statfs_t
	if syscall.Statfs(path, &s) != nil {
		return 0, 0
	}
	total = int64(s.Blocks) * int64(s.Bsize)
	return total - int64(s.Bavail)*int64(s.Bsize), total
}

// kernel is the kernel's release.
func kernel() string {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
