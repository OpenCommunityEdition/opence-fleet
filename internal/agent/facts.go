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

// cpuTimes is the machine's CPU time from /proc/stat's first line, in
// ticks: busy and in all (iowait counts as idle; guest time is in user's
// already).
type cpuTimes struct{ busy, total uint64 }

// parseCPU reads /proc/stat's text.
func parseCPU(text string) (cpuTimes, bool) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var t cpuTimes
		// user nice system idle iowait irq softirq steal
		for k, field := range fields[1:min(len(fields), 9)] {
			n, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return cpuTimes{}, false
			}
			t.total += n
			if k != 3 && k != 4 {
				t.busy += n
			}
		}
		return t, true
	}
	return cpuTimes{}, false
}

// readCPU is the machine's CPU time (false: not Linux, or unreadable).
func readCPU() (cpuTimes, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, false
	}
	return parseCPU(string(data))
}

// cpuPercent is the share of all the CPUs that was busy between two
// readings, 0 to 100 (false: no time passed, or the counters went back).
func cpuPercent(before, after cpuTimes) (float64, bool) {
	if after.total <= before.total || after.busy < before.busy {
		return 0, false
	}
	p := float64(after.busy-before.busy) * 100 / float64(after.total-before.total)
	return min(100, p), true
}

// parseNetDev reads /proc/net/dev's text: the bytes received and sent by
// the machine's interfaces, summed. The loopback and the virtual ones of
// containers (veth*, docker*, br-*, virbr*) are left out: what crosses them
// crosses a real interface too, and would count twice.
func parseNetDev(text string) (received, sent uint64, ok bool) {
	for _, line := range strings.Split(text, "\n") {
		name, counters, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		fields := strings.Fields(counters)
		if len(fields) < 9 || virtualInterface(name) {
			continue
		}
		in, err1 := strconv.ParseUint(fields[0], 10, 64)
		out, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		received, sent, ok = received+in, sent+out, true
	}
	return received, sent, ok
}

func virtualInterface(name string) bool {
	if name == "lo" {
		return true
	}
	for _, prefix := range []string{"veth", "docker", "br-", "virbr"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// network is the machine's bytes received and sent (false: not Linux, or
// unreadable).
func network() (received, sent uint64, ok bool) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0, false
	}
	return parseNetDev(string(data))
}

// kernel is the kernel's release.
func kernel() string {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
