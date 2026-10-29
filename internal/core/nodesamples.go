package core

import (
	"context"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/live"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// netReading is a node's network counters as a heartbeat said them, and
// when it came.
type netReading struct {
	at             time.Time
	received, sent uint64
}

// The longest a heartbeat may be old and still be sampled (they come every
// 15 s): a node quiet longer is left out, a gap in its graphs.
const sampleFresh = time.Minute

// The longest between two readings a rate is taken over: beyond, the node
// was gone, and an average over its absence would hide it.
const rateSpan = 10 * time.Minute

// netRates is the bytes a second in and out between two readings; each is
// nil when it is not known: no reading before, no time between them, too
// long, or its counter went back (the machine restarted, an interface
// went away).
func netRates(before netReading, had bool, now netReading) (in, out *float64) {
	if !had {
		return nil, nil
	}
	span := now.at.Sub(before.at)
	if span <= 0 || span > rateSpan {
		return nil, nil
	}
	rate := func(a, b uint64) *float64 {
		if b < a {
			return nil
		}
		r := float64(b-a) / span.Seconds()
		return &r
	}
	return rate(before.received, now.received), rate(before.sent, now.sent)
}

// nodeSample is a node's sample at a minute from its last heartbeat, and
// its network reading (if the heartbeat had one) for the next.
func nodeSample(n live.Node, minute int64, before netReading, had bool) (db.InsertNodeSampleParams, netReading, bool) {
	h := n.Heartbeat
	sample := db.InsertNodeSampleParams{NodeID: n.ID, Time: minute, Cpu: h.CPU, Load: h.Load, MemoryUsed: h.MemoryUsed,
		MemoryTotal: h.MemoryTotal, DiskUsed: h.DiskUsed, DiskTotal: h.DiskTotal}
	if h.NetReceived == nil || h.NetSent == nil {
		return sample, netReading{}, false
	}
	now := netReading{at: n.HeartbeatAt, received: *h.NetReceived, sent: *h.NetSent}
	if had && now.at.Equal(before.at) {
		// (no heartbeat since the last sample: no rate, the reading kept)
		return sample, before, true
	}
	sample.NetIn, sample.NetOut = netRates(before, had, now)
	return sample, now, true
}

// sampleNodes keeps each online node's health at a minute.
func (f *Fleet) sampleNodes(ctx context.Context, minute int64) {
	f.netMu.Lock()
	defer f.netMu.Unlock()
	online := map[int64]bool{}
	for _, n := range f.Live.Nodes() {
		online[n.ID] = true
		if n.HeartbeatAt.IsZero() || time.Since(n.HeartbeatAt) > sampleFresh {
			// (no heartbeat yet, or none lately)
			continue
		}
		before, had := f.netReadings[n.ID]
		sample, reading, ok := nodeSample(n, minute, before, had)
		if ok {
			f.netReadings[n.ID] = reading
		} else {
			delete(f.netReadings, n.ID)
		}
		if err := f.Store.InsertNodeSample(ctx, sample); err != nil {
			f.Log.Warn("a node's sample was not kept", "node", n.Name, "err", err)
		}
	}
	for id := range f.netReadings {
		if !online[id] {
			delete(f.netReadings, id)
		}
	}
}
