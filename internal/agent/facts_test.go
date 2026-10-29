package agent

import (
	"math"
	"os"
	"testing"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestParseCPU(t *testing.T) {
	got, ok := parseCPU(fixture(t, "proc_stat.txt"))
	// (user nice system idle iowait irq softirq steal; guest is in user)
	busy := uint64(10132153 + 290696 + 3084719 + 0 + 25195 + 0)
	total := busy + 46828483 + 16683
	if !ok || got.busy != busy || got.total != total {
		t.Fatalf("%+v %v, want busy %d of %d", got, ok, busy, total)
	}
	if _, ok := parseCPU("intr 1 2 3\n"); ok {
		t.Fatal("a stat without a cpu line was read")
	}
	if _, ok := parseCPU("cpu  1 2 x 4 5\n"); ok {
		t.Fatal("a broken cpu line was read")
	}
	// an old kernel's four columns
	if got, ok := parseCPU("cpu 10 0 10 80\n"); !ok || got.busy != 20 || got.total != 100 {
		t.Fatalf("four columns: %+v %v", got, ok)
	}
}

func TestCPUPercent(t *testing.T) {
	p, ok := cpuPercent(cpuTimes{busy: 100, total: 1000}, cpuTimes{busy: 350, total: 2000})
	if !ok || math.Abs(p-25) > 1e-9 {
		t.Fatalf("%v %v", p, ok)
	}
	if _, ok := cpuPercent(cpuTimes{busy: 100, total: 1000}, cpuTimes{busy: 100, total: 1000}); ok {
		t.Fatal("no time passed, yet a share")
	}
	// (the counters went back: a reboot under a kept agent state)
	if _, ok := cpuPercent(cpuTimes{busy: 500, total: 1000}, cpuTimes{busy: 10, total: 2000}); ok {
		t.Fatal("counters that went back gave a share")
	}
}

func TestParseNetDev(t *testing.T) {
	received, sent, ok := parseNetDev(fixture(t, "proc_net_dev.txt"))
	// eth0 and wlan0: not lo, docker0, the veth or the bridge
	if !ok || received != 1234567890+1000 || sent != 987654321+2000 {
		t.Fatalf("%d %d %v", received, sent, ok)
	}
	if _, _, ok := parseNetDev("Inter-|   Receive\n    lo: 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16\n"); ok {
		t.Fatal("the loopback alone was counted")
	}
	if _, _, ok := parseNetDev(""); ok {
		t.Fatal("nothing was read as something")
	}
}
