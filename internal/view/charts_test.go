package view

import (
	"strings"
	"testing"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

func TestNodeCharts(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).Truncate(time.Minute)
	f := func(v float64) *float64 { return &v }
	var samples []db.NodeSample
	for m := 60; m >= 1; m-- {
		at := now.Add(-time.Duration(m) * time.Minute).Unix()
		if m > 20 && m <= 30 {
			continue // (the node offline)
		}
		s := db.NodeSample{Time: at, Cpu: f(float64(m)), MemoryUsed: 1 << 30, MemoryTotal: 4 << 30, DiskUsed: 1, DiskTotal: 2,
			NetIn: f(1500 * 1024), NetOut: f(100)}
		if m > 50 {
			// (an older agent: no CPU, no network)
			s.Cpu, s.NetIn, s.NetOut = nil, nil, nil
		}
		samples = append(samples, s)
	}
	charts := NodeCharts(samples, now, WindowOf("1h"), time.UTC)
	if len(charts) != 4 {
		t.Fatalf("%d charts", len(charts))
	}
	cpu, memory, network := charts[0], charts[1], charts[3]
	// a gap is a new subpath: before, during and after the offline minutes
	if n := strings.Count(cpu.Series[0].Path, "M"); n != 2 {
		t.Fatalf("the CPU's line has %d runs: %s", n, cpu.Series[0].Path)
	}
	if n := strings.Count(memory.Series[0].Path, "M"); n != 2 {
		t.Fatalf("the memory's line has %d runs", n)
	}
	if cpu.Latest != "1 %" || cpu.Top != "100 %" || cpu.Empty {
		t.Fatalf("the CPU: %q %q", cpu.Latest, cpu.Top)
	}
	if memory.Latest != "25 % · 1.0 GB of 4.0 GB" {
		t.Fatalf("the memory: %q", memory.Latest)
	}
	if len(network.Series) != 2 || network.Top != "2.0 MB/s" || network.Latest != "in 1.5 MB/s · out 100 B/s" {
		t.Fatalf("the network: %q %q", network.Top, network.Latest)
	}
	// a hover a column with a value, none for the offline ones
	if len(cpu.Columns) != 40 || !strings.Contains(cpu.Columns[0].Title, "%") {
		t.Fatalf("%d columns: %+v", len(cpu.Columns), cpu.Columns[0])
	}
	// no samples: empty, no line
	empty := NodeCharts(nil, now, WindowOf("7d"), time.UTC)
	if !empty[0].Empty || empty[0].Series[0].Path != "" || empty[3].Top != "1 KB/s" {
		t.Fatalf("%+v", empty[0])
	}
	if WindowOf("nonsense").Name != "24h" || WindowOf("7d").Span != 7*24*time.Hour {
		t.Fatal("the windows")
	}
}

func TestNiceTop(t *testing.T) {
	for v, want := range map[float64]float64{0: 1024, 1000: 1024, 1500: 2048, 3 << 20: 5 << 20, 900 * 1024: 1000 * 1024} {
		if got := niceTop(v); got != want {
			t.Errorf("niceTop(%v) = %v, want %v", v, got, want)
		}
	}
}
