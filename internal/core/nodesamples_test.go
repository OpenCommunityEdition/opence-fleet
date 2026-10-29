package core

import (
	"context"
	"testing"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/live"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

func TestNetRates(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	before := netReading{at: at, received: 1000, sent: 5000}
	in, out := netRates(before, true, netReading{at: at.Add(time.Minute), received: 61000, sent: 5000})
	if in == nil || out == nil || *in != 1000 || *out != 0 {
		t.Fatalf("%v %v", in, out)
	}
	// no reading before: unknown
	if in, out := netRates(netReading{}, false, before); in != nil || out != nil {
		t.Fatal("a rate without a reading before")
	}
	// a counter that went back (a restart): that one unknown
	in, out = netRates(before, true, netReading{at: at.Add(time.Minute), received: 10, sent: 11000})
	if in != nil || out == nil || *out != 100 {
		t.Fatalf("a reset: %v %v", in, out)
	}
	// no time between, or too long: unknown
	if in, _ := netRates(before, true, netReading{at: at, received: 2000}); in != nil {
		t.Fatal("a rate over no time")
	}
	if in, _ := netRates(before, true, netReading{at: at.Add(time.Hour), received: 2000}); in != nil {
		t.Fatal("a rate over an hour's absence")
	}
}

func TestNodeSample(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	cpu, received, sent := 42.5, uint64(1_000_000), uint64(2_000_000)
	n := live.Node{ID: 7, HeartbeatAt: at, Heartbeat: agentproto.Heartbeat{Load: 0.5, MemoryUsed: 1, MemoryTotal: 2,
		DiskUsed: 3, DiskTotal: 4, CPU: &cpu, NetReceived: &received, NetSent: &sent}}
	sample, reading, ok := nodeSample(n, 60, netReading{}, false)
	if !ok || sample.NetIn != nil || *sample.Cpu != 42.5 || sample.MemoryTotal != 2 || reading.received != received {
		t.Fatalf("the first: %+v %+v %v", sample, reading, ok)
	}
	// a minute later, the rates
	received2, sent2 := received+60_000, sent+120_000
	n.HeartbeatAt, n.Heartbeat.NetReceived, n.Heartbeat.NetSent = at.Add(time.Minute), &received2, &sent2
	sample, reading, _ = nodeSample(n, 120, reading, true)
	if sample.NetIn == nil || *sample.NetIn != 1000 || *sample.NetOut != 2000 {
		t.Fatalf("the rates: %+v", sample)
	}
	// the same heartbeat again: no rate, the reading kept
	again, kept, _ := nodeSample(n, 180, reading, true)
	if again.NetIn != nil || kept != reading {
		t.Fatalf("the same heartbeat: %+v %+v", again, kept)
	}
	// an old agent's heartbeat: no CPU, no network, the rest kept
	old := live.Node{ID: 7, HeartbeatAt: at, Heartbeat: agentproto.Heartbeat{Load: 1, MemoryUsed: 5, MemoryTotal: 10}}
	sample, _, ok = nodeSample(old, 240, reading, true)
	if ok || sample.Cpu != nil || sample.NetIn != nil || sample.MemoryUsed != 5 {
		t.Fatalf("an old agent: %+v %v", sample, ok)
	}
}

// The minute's sampling keeps each online node's health with its rates,
// and the pruning drops them past FLEET_SAMPLES_DAYS.
func TestSampleNodes(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	id, _ := node(t, f, "home", spec.DefaultNode())
	f.Live.Connected(id, "home", agentproto.Hello{})
	beat := func(received, sent uint64) {
		cpu := 10.0
		f.Live.Heartbeat(id, agentproto.Heartbeat{Version: "v1", MemoryUsed: 1 << 30, MemoryTotal: 4 << 30, CPU: &cpu,
			NetReceived: &received, NetSent: &sent})
	}
	beat(1000, 1000)
	f.sampleNodes(ctx, 60)
	time.Sleep(20 * time.Millisecond)
	beat(1000+1_000_000, 1000)
	f.sampleNodes(ctx, 120)
	samples, err := f.Store.ListNodeSamples(ctx, db.ListNodeSamplesParams{NodeID: id, Time: 0})
	if err != nil || len(samples) != 2 {
		t.Fatalf("%v %v", samples, err)
	}
	if samples[0].NetIn != nil || samples[1].NetIn == nil || *samples[1].NetIn < 1_000_000 || *samples[1].NetOut != 0 ||
		*samples[1].Cpu != 10 || samples[1].MemoryTotal != 4<<30 {
		t.Fatalf("the samples: %+v %+v", samples[0], samples[1])
	}
	// an offline node is not sampled
	f.Live.Disconnected(id)
	f.sampleNodes(ctx, 180)
	if samples, _ := f.Store.ListNodeSamples(ctx, db.ListNodeSamplesParams{NodeID: id, Time: 0}); len(samples) != 2 {
		t.Fatalf("an offline node was sampled: %d", len(samples))
	}
	// the pruning keeps FLEET_SAMPLES_DAYS
	if err := f.Store.Prune(ctx, time.Unix(120, 0).Add(7*24*time.Hour), 30, 7); err != nil {
		t.Fatal(err)
	}
	if samples, _ := f.Store.ListNodeSamples(ctx, db.ListNodeSamplesParams{NodeID: id, Time: 0}); len(samples) != 1 || samples[0].Time != 120 {
		t.Fatalf("after the pruning: %+v", samples)
	}
}
