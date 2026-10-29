// Package live is the control plane's live state, in memory: each
// instance's latest status and state as its agent reported them, each
// node's hello and heartbeat, the log ring of each instance, the fleet's
// feed, and the notifications of what changed, which the pages' stream
// (SSE) and the console's WebSocket follow. Nothing here is in the
// database: it is rebuilt from the agents' reports after a restart.
package live

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/serverapi"
)

// LogSize is how many log lines an instance keeps in memory.
const LogSize = 2048

// FeedSize is how many of the fleet's events the feed keeps.
const FeedSize = 200

// Instance is an instance's live state.
type Instance struct {
	Slug   string
	NodeID int64
	// the agent's: starting, up, stopping, stopped, crashed, blocked,
	// pending; unknown while its node is offline
	State   string
	Reason  string
	Exit    *int
	Tail    []string
	Since   time.Time
	Updated time.Time
	// the server's own status (every 2 s while up)
	Status   *serverapi.Status
	StatusAt time.Time
	// what the running server was made from
	Generation int64
	Build      string
	// the server's hello
	ServerBuild  string
	Capabilities []string
	// its use, from the heartbeat
	RSS int64
	CPU float64
	// the last live application of a spec
	Applied       *agentproto.Applied
	RestartNeeded bool
	// the players seen at the last sample, the peak since the game began
	Peak int
}

// Has tells whether the server has a capability.
func (i *Instance) Has(capability string) bool {
	for _, c := range i.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// Node is a node's live state.
type Node struct {
	ID          int64
	Name        string
	Online      bool
	ConnectedAt time.Time
	LastSeen    time.Time
	Hello       agentproto.Hello
	Heartbeat   agentproto.Heartbeat
	// when the heartbeat came (LastSeen is also the connect's)
	HeartbeatAt time.Time
	Syncs       map[string]agentproto.Sync
}

// FeedItem is an event of the fleet, as a sentence's parts.
type FeedItem struct {
	ID    int64
	Time  time.Time
	Slug  string
	Kind  string
	Event string
	Text  string
	// for admins alone (a chat line)
	Private bool
}

// Change is what changed, for the subscribers.
type Change struct {
	// instance, node, feed, tiles, alert, job, log, players
	Kind string
	Key  string
	Log  *agentproto.Log
	Feed *FeedItem
}

// Live is the live state.
type Live struct {
	mu        sync.RWMutex
	instances map[string]*Instance
	nodes     map[int64]*Node
	logs      map[string][]agentproto.Log
	feed      []FeedItem
	feedID    int64
	// the last seq seen of each instance's run (events are dropped by it)
	seqs map[string]runSeq

	subMu       sync.Mutex
	subscribers map[chan Change]struct{}
}

type runSeq struct {
	run int64
	seq int64
}

// New makes the live state.
func New() *Live {
	return &Live{instances: map[string]*Instance{}, nodes: map[int64]*Node{}, logs: map[string][]agentproto.Log{},
		seqs: map[string]runSeq{}, subscribers: map[chan Change]struct{}{}}
}

// Subscribe returns the changes' channel (buffered; a subscriber too slow
// for it misses changes, and gets a "resync") and its end.
func (l *Live) Subscribe() (<-chan Change, func()) {
	ch := make(chan Change, 512)
	l.subMu.Lock()
	l.subscribers[ch] = struct{}{}
	l.subMu.Unlock()
	return ch, func() {
		l.subMu.Lock()
		if _, ok := l.subscribers[ch]; ok {
			delete(l.subscribers, ch)
			close(ch)
		}
		l.subMu.Unlock()
	}
}

// Notify tells the subscribers of a change.
func (l *Live) Notify(c Change) {
	l.subMu.Lock()
	defer l.subMu.Unlock()
	for ch := range l.subscribers {
		select {
		case ch <- c:
		default:
			// (too slow: it is told to read everything again)
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- Change{Kind: "resync"}:
			default:
			}
		}
	}
}

// Instance returns a copy of an instance's live state (nil: none yet).
func (l *Live) Instance(slug string) *Instance {
	l.mu.RLock()
	defer l.mu.RUnlock()
	i := l.instances[slug]
	if i == nil {
		return nil
	}
	copy := *i
	return &copy
}

// Instances returns copies of every instance's live state, by slug.
func (l *Live) Instances() map[string]*Instance {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]*Instance, len(l.instances))
	for slug, i := range l.instances {
		copy := *i
		out[slug] = &copy
	}
	return out
}

func (l *Live) instance(slug string, node int64) *Instance {
	i := l.instances[slug]
	if i == nil {
		i = &Instance{Slug: slug, NodeID: node, State: "unknown", Since: time.Now()}
		l.instances[slug] = i
	}
	if node != 0 {
		i.NodeID = node
	}
	return i
}

// SetInstance applies an agent's instance report.
func (l *Live) SetInstance(node int64, r agentproto.Instance) {
	l.mu.Lock()
	i := l.instance(r.Slug, node)
	i.State, i.Reason, i.Exit, i.Tail = r.State, r.Reason, r.Exit, r.Tail
	if r.Since > 0 {
		i.Since = time.UnixMilli(r.Since)
	}
	i.Updated = time.Now()
	if r.Generation != 0 {
		i.Generation, i.Build = r.Generation, r.Build
	}
	if r.ServerBuild != "" || len(r.Capabilities) > 0 {
		i.ServerBuild, i.Capabilities = r.ServerBuild, r.Capabilities
	}
	if r.State != agentproto.Up {
		i.Status = nil
	}
	l.mu.Unlock()
	l.Notify(Change{Kind: "instance", Key: r.Slug})
	l.Notify(Change{Kind: "tiles"})
}

// SetReport applies an instance's part of a heartbeat or a hello.
func (l *Live) SetReport(node int64, r agentproto.InstanceReport) {
	l.mu.Lock()
	i := l.instance(r.Slug, node)
	changed := i.State != r.State || i.RestartNeeded != r.RestartNeeded
	if i.State == "unknown" || i.State == "" {
		i.State = r.State
		if r.Since > 0 {
			i.Since = time.UnixMilli(r.Since)
		}
	} else if i.State != r.State {
		i.State = r.State
	}
	if r.Generation != 0 {
		i.Generation, i.Build = r.Generation, r.Build
	}
	i.RSS, i.CPU, i.RestartNeeded = r.RSS, r.CPU, r.RestartNeeded
	l.mu.Unlock()
	if changed {
		l.Notify(Change{Kind: "instance", Key: r.Slug})
	}
}

// SetStatus applies a server's status; it returns the status before.
func (l *Live) SetStatus(node int64, slug string, raw json.RawMessage) (*serverapi.Status, *serverapi.Status) {
	status, err := serverapi.ParseStatus(raw)
	if err != nil {
		return nil, nil
	}
	l.mu.Lock()
	i := l.instance(slug, node)
	before := i.Status
	i.Status, i.StatusAt = status, time.Now()
	if i.State == "unknown" {
		// (an external instance, which no agent reports: its status says it)
		i.State = "up"
	}
	if status.Players > i.Peak {
		i.Peak = status.Players
	}
	l.mu.Unlock()
	if before == nil || visible(before) != visible(status) {
		l.Notify(Change{Kind: "instance", Key: slug})
	}
	if before == nil || before.Players != status.Players {
		l.Notify(Change{Kind: "tiles"})
		l.Notify(Change{Kind: "players", Key: slug})
	}
	return before, status
}

// visible is what a row shows of a status: a status that changes nothing of
// it sends nothing to the pages.
func visible(s *serverapi.Status) string {
	listed := ""
	for _, m := range s.Masters {
		listed += m.Probe
		if m.Listed {
			listed += "+"
		}
	}
	countdown := s.Countdown
	if countdown > 0 {
		// (a countdown shown to 5 seconds)
		countdown = (countdown + 4) / 5
	}
	drain := ""
	if s.Drain != nil {
		drain = s.Drain.After
	}
	return fmt.Sprint(s.State, "|", s.Map, "|", s.Gametype, "|", s.Name, "|", s.Players, "/", s.MaxPlayers, "|",
		countdown, "|", s.TeamScores, "|", s.GameSeconds/60, "|", listed, "|", drain, "|", s.Build)
}

// SetApplied keeps the outcome of a spec's live application.
func (l *Live) SetApplied(node int64, a agentproto.Applied) {
	l.mu.Lock()
	i := l.instance(a.Slug, node)
	i.Applied, i.RestartNeeded = &a, a.RestartNeeded
	l.mu.Unlock()
	l.Notify(Change{Kind: "instance", Key: a.Slug})
}

// Forget drops an instance's live state (removed).
func (l *Live) Forget(slug string) {
	l.mu.Lock()
	delete(l.instances, slug)
	delete(l.logs, slug)
	delete(l.seqs, slug)
	l.mu.Unlock()
	l.Notify(Change{Kind: "instance", Key: slug})
	l.Notify(Change{Kind: "tiles"})
}

// NewEvent tells whether an event is new (not a replay of one seen): the
// seq of its server's run.
func (l *Live) NewEvent(slug string, run, seq int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	last := l.seqs[slug]
	if seq == 0 {
		return true
	}
	if run != last.run {
		if run < last.run {
			return false
		}
		l.seqs[slug] = runSeq{run, seq}
		return true
	}
	if seq <= last.seq {
		return false
	}
	l.seqs[slug] = runSeq{run, seq}
	return true
}

// AddLog keeps a log line and tells the console's followers.
func (l *Live) AddLog(line agentproto.Log) {
	l.mu.Lock()
	lines := append(l.logs[line.Slug], line)
	if len(lines) > LogSize {
		lines = lines[len(lines)-LogSize:]
	}
	l.logs[line.Slug] = lines
	l.mu.Unlock()
	l.Notify(Change{Kind: "log", Key: line.Slug, Log: &line})
}

// Logs returns an instance's last lines (n at most).
func (l *Live) Logs(slug string, n int) []agentproto.Log {
	l.mu.RLock()
	defer l.mu.RUnlock()
	lines := l.logs[slug]
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return append([]agentproto.Log(nil), lines...)
}

// AddFeed adds an item to the feed.
func (l *Live) AddFeed(item FeedItem) FeedItem {
	l.mu.Lock()
	l.feedID++
	item.ID = l.feedID
	if item.Time.IsZero() {
		item.Time = time.Now()
	}
	l.feed = append(l.feed, item)
	if len(l.feed) > FeedSize {
		l.feed = l.feed[len(l.feed)-FeedSize:]
	}
	l.mu.Unlock()
	l.Notify(Change{Kind: "feed", Key: item.Slug, Feed: &item})
	return item
}

// Feed returns the feed's newest items first (n at most; slug: one
// instance's).
func (l *Live) Feed(n int, slug string) []FeedItem {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []FeedItem
	for i := len(l.feed) - 1; i >= 0 && (n <= 0 || len(out) < n); i-- {
		if slug == "" || l.feed[i].Slug == slug {
			out = append(out, l.feed[i])
		}
	}
	return out
}

// Node returns a copy of a node's live state (nil: never connected).
func (l *Live) Node(id int64) *Node {
	l.mu.RLock()
	defer l.mu.RUnlock()
	n := l.nodes[id]
	if n == nil {
		return nil
	}
	copy := *n
	copy.Syncs = map[string]agentproto.Sync{}
	for k, v := range n.Syncs {
		copy.Syncs[k] = v
	}
	return &copy
}

// Nodes is every node online, copied.
func (l *Live) Nodes() []Node {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []Node
	for _, n := range l.nodes {
		if n.Online {
			copy := *n
			copy.Syncs = nil
			out = append(out, copy)
		}
	}
	return out
}

// Connected marks a node online with its hello.
func (l *Live) Connected(id int64, name string, hello agentproto.Hello) {
	l.mu.Lock()
	n := l.nodes[id]
	if n == nil {
		n = &Node{ID: id, Syncs: map[string]agentproto.Sync{}}
		l.nodes[id] = n
	}
	n.Name, n.Online, n.ConnectedAt, n.LastSeen, n.Hello = name, true, time.Now(), time.Now(), hello
	for _, r := range hello.Instances {
		i := l.instance(r.Slug, id)
		i.State = r.State
		if r.Since > 0 {
			i.Since = time.UnixMilli(r.Since)
		}
		if r.Generation != 0 {
			i.Generation, i.Build = r.Generation, r.Build
		}
		i.RestartNeeded = r.RestartNeeded
	}
	l.mu.Unlock()
	l.Notify(Change{Kind: "node", Key: name})
	l.Notify(Change{Kind: "tiles"})
}

// Heartbeat applies a node's heartbeat.
func (l *Live) Heartbeat(id int64, h agentproto.Heartbeat) {
	l.mu.Lock()
	n := l.nodes[id]
	if n == nil {
		l.mu.Unlock()
		return
	}
	n.Heartbeat, n.LastSeen, n.HeartbeatAt = h, time.Now(), time.Now()
	name := n.Name
	l.mu.Unlock()
	for _, r := range h.Instances {
		l.SetReport(id, r)
	}
	l.Notify(Change{Kind: "node", Key: name})
}

// Sync keeps a node's download progress.
func (l *Live) Sync(id int64, s agentproto.Sync) {
	l.mu.Lock()
	n := l.nodes[id]
	if n != nil {
		if s.Error == "" && s.Total > 0 && s.Done >= s.Total {
			delete(n.Syncs, s.Kind+":"+s.Name)
		} else {
			n.Syncs[s.Kind+":"+s.Name] = s
		}
	}
	name := ""
	if n != nil {
		name = n.Name
	}
	l.mu.Unlock()
	if name != "" {
		l.Notify(Change{Kind: "node", Key: name})
	}
}

// Disconnected marks a node offline, and its instances unknown (their
// last state kept, greyed).
func (l *Live) Disconnected(id int64) {
	l.mu.Lock()
	n := l.nodes[id]
	name := ""
	if n != nil {
		n.Online = false
		name = n.Name
	}
	var slugs []string
	for _, i := range l.instances {
		if i.NodeID == id {
			i.State, i.Status = "unknown", nil
			slugs = append(slugs, i.Slug)
		}
	}
	l.mu.Unlock()
	sort.Strings(slugs)
	for _, slug := range slugs {
		l.Notify(Change{Kind: "instance", Key: slug})
	}
	l.Notify(Change{Kind: "node", Key: name})
	l.Notify(Change{Kind: "tiles"})
}

// Players is how many players the fleet has now.
func (l *Live) Players() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	n := 0
	for _, i := range l.instances {
		if i.Status != nil {
			n += i.Status.Players
		}
	}
	return n
}

// ResetPeak starts an instance's peak again (a game began).
func (l *Live) ResetPeak(slug string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	i := l.instances[slug]
	if i == nil {
		return 0
	}
	peak := i.Peak
	i.Peak = 0
	if i.Status != nil {
		i.Peak = i.Status.Players
	}
	return peak
}
