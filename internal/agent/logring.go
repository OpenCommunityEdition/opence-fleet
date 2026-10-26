package agent

import (
	"sync"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
)

// ringSize is how many log lines an instance keeps on the node, for a
// control plane that reconnects and asks what it missed.
const ringSize = 2048

type logRing struct {
	mu    sync.Mutex
	lines []agentproto.Log
	next  int64
}

func (r *logRing) add(line agentproto.Log) agentproto.Log {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	line.N = r.next
	r.lines = append(r.lines, line)
	if len(r.lines) > ringSize {
		r.lines = r.lines[len(r.lines)-ringSize:]
	}
	return line
}

func (r *logRing) after(n int64, limit int) []agentproto.Log {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || limit > ringSize {
		limit = ringSize
	}
	var out []agentproto.Log
	for _, l := range r.lines {
		if l.N > n {
			out = append(out, l)
			if len(out) == limit {
				break
			}
		}
	}
	return out
}

func (r *logRing) tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	start := len(r.lines) - n
	if start < 0 {
		start = 0
	}
	var out []string
	for _, l := range r.lines[start:] {
		if !l.Private {
			out = append(out, l.Cat+": "+l.Text)
		}
	}
	return out
}

// eventRing keeps an instance's last events, replayed to a control plane
// that reconnects (it drops those it has by their seq).
type eventRing struct {
	mu     sync.Mutex
	events []agentproto.Event
}

func (r *eventRing) add(e agentproto.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	if len(r.events) > 512 {
		r.events = r.events[len(r.events)-512:]
	}
}

func (r *eventRing) all() []agentproto.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agentproto.Event(nil), r.events...)
}
