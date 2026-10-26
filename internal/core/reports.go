package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/live"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

func liveChange(kind, key string) live.Change { return live.Change{Kind: kind, Key: key} }

// owners caches which node each instance is on: a report about a slug not
// on the reporting node is refused (a node reports about its own alone).
type owners struct {
	mu    sync.Mutex
	nodes map[string]int64
	at    time.Time
}

func (f *Fleet) owns(node int64, slug string) bool {
	f.owners.mu.Lock()
	defer f.owners.mu.Unlock()
	if n, ok := f.owners.nodes[slug]; ok && n == node && time.Since(f.owners.at) < time.Minute {
		return true
	}
	rows, err := f.Store.ListInstances(context.Background())
	if err != nil {
		return false
	}
	f.owners.nodes = map[string]int64{}
	for _, r := range rows {
		f.owners.nodes[r.Slug] = r.NodeID
	}
	f.owners.at = time.Now()
	return f.owners.nodes[slug] == node
}

// forgetOwners drops the cache (an instance made, moved or removed).
func (f *Fleet) forgetOwners() {
	f.owners.mu.Lock()
	f.owners.at = time.Time{}
	f.owners.mu.Unlock()
}

// Report takes an agent's report (the hub's Sink).
func (f *Fleet) Report(node int64, kind string, data []byte) {
	refuse := func(slug string) bool {
		if f.owns(node, slug) {
			return false
		}
		f.Log.Warn("an agent reported about an instance not on its node: refused", "node", node, "slug", slug, "type", kind)
		return true
	}
	switch kind {
	case agentproto.THeartbeat:
		var h agentproto.Heartbeat
		if json.Unmarshal(data, &h) != nil {
			return
		}
		var mine []agentproto.InstanceReport
		for _, r := range h.Instances {
			if f.owns(node, r.Slug) {
				mine = append(mine, r)
			}
		}
		h.Instances = mine
		f.Live.Heartbeat(node, h)
		f.Store.TouchNode(context.Background(), db.TouchNodeParams{LastSeen: ptrInt(f.Now().Unix()), ID: node})
		f.checkDisk(node, h)
	case agentproto.TInstance:
		var r agentproto.Instance
		if json.Unmarshal(data, &r) != nil || refuse(r.Slug) {
			return
		}
		before := f.Live.Instance(r.Slug)
		f.Live.SetInstance(node, r)
		if before == nil || before.State != r.State {
			f.onState(node, r, before)
		}
	case agentproto.TStatus:
		var s agentproto.Status
		if json.Unmarshal(data, &s) != nil || refuse(s.Slug) {
			return
		}
		f.Live.SetStatus(node, s.Slug, s.Status)
	case agentproto.TEvent:
		var e agentproto.Event
		if json.Unmarshal(data, &e) != nil || refuse(e.Slug) {
			return
		}
		if !f.Live.NewEvent(e.Slug, e.Run, e.Seq) {
			return
		}
		f.onEvent(node, e)
	case agentproto.TLog:
		var l agentproto.Log
		if json.Unmarshal(data, &l) != nil || refuse(l.Slug) {
			return
		}
		f.Live.AddLog(l)
	case agentproto.TSync:
		var s agentproto.Sync
		if json.Unmarshal(data, &s) == nil {
			f.Live.Sync(node, s)
		}
	case agentproto.TApplied:
		var a agentproto.Applied
		if json.Unmarshal(data, &a) != nil || refuse(a.Slug) {
			return
		}
		f.Live.SetApplied(node, a)
		f.jobs.applied(a)
		if len(a.Errors) > 0 {
			var parts []string
			for _, e := range a.Errors {
				parts = append(parts, e.Name+": "+e.Message)
			}
			f.Live.AddFeed(live.FeedItem{Slug: a.Slug, Kind: "warn", Event: "applied",
				Text: "refused " + strings.Join(parts, "; ")})
		}
	}
}

// onState is an instance's change of state: the feed, the alerts, an event
// kept.
func (f *Fleet) onState(node int64, r agentproto.Instance, before *live.Instance) {
	ctx := context.Background()
	data, _ := json.Marshal(r)
	f.storeEvent(node, r.Slug, 0, "instance."+r.State, data)
	switch r.State {
	case agentproto.Up:
		f.Live.AddFeed(live.FeedItem{Slug: r.Slug, Kind: "ok", Event: "up", Text: "is up (" + r.Build + ")"})
		f.resolveAlert("instance_crashed", r.Slug)
		f.resolveAlert("instance_blocked", r.Slug)
	case agentproto.Crashed:
		code := 0
		if r.Exit != nil {
			code = *r.Exit
		}
		f.Live.AddFeed(live.FeedItem{Slug: r.Slug, Kind: "bad", Event: "crashed", Text: fmt.Sprintf("crashed (exit %d): %s", code, r.Reason)})
		f.crashed(ctx, r)
	case agentproto.Blocked:
		f.Live.AddFeed(live.FeedItem{Slug: r.Slug, Kind: "bad", Event: "blocked", Text: "stopped by its configuration: " + r.Reason})
		f.raiseAlert(ctx, "instance_blocked", r.Slug, r.Reason)
	case agentproto.Stopped:
		if before != nil && before.State != "unknown" && before.State != "" {
			f.Live.AddFeed(live.FeedItem{Slug: r.Slug, Kind: "info", Event: "stopped", Text: "stopped: " + r.Reason})
		}
		f.resolveAlert("instance_crashed", r.Slug)
	case agentproto.Pending:
		f.Live.AddFeed(live.FeedItem{Slug: r.Slug, Kind: "info", Event: "pending", Text: r.Reason})
	}
}

// crashTimes are each instance's crash times, for the crash-loop alert.
type crashTimes struct {
	sync.Mutex
	at map[string][]time.Time
}

func (f *Fleet) crashed(ctx context.Context, r agentproto.Instance) {
	f.crashes.Lock()
	now := f.Now()
	list := append(f.crashes.at[r.Slug], now)
	var recent []time.Time
	for _, t := range list {
		if now.Sub(t) < 10*time.Minute {
			recent = append(recent, t)
		}
	}
	f.crashes.at[r.Slug] = recent
	n := len(recent)
	f.crashes.Unlock()
	detail := r.Reason
	if len(r.Tail) > 0 {
		detail += "\n" + strings.Join(r.Tail, "\n")
	}
	f.raiseAlert(ctx, "instance_crashed", r.Slug, detail)
	if n >= 3 {
		f.raiseAlert(ctx, "instance_crash_loop", r.Slug, fmt.Sprintf("%d crashes in 10 minutes: %s", n, r.Reason))
	}
}

// eventWriter batches the events kept, one transaction a moment.
type eventWriter struct {
	once sync.Once
	ch   chan db.InsertEventParams
}

func (f *Fleet) storeEvent(node int64, slug string, seq int64, kind string, data []byte) {
	f.events.once.Do(func() { go f.writeEvents() })
	params := db.InsertEventParams{Time: f.Now().UnixMilli(), NodeID: &node, Slug: slug, Seq: seq, Type: kind, Data: string(data)}
	select {
	case f.events.ch <- params:
	default:
		f.Log.Warn("an event was not kept: the database is behind")
	}
}

func (f *Fleet) writeEvents() {
	for first := range f.events.ch {
		batch := []db.InsertEventParams{first}
		timeout := time.After(300 * time.Millisecond)
	collect:
		for len(batch) < 500 {
			select {
			case p := <-f.events.ch:
				batch = append(batch, p)
			case <-timeout:
				break collect
			}
		}
		err := f.Store.Tx(context.Background(), func(q *db.Queries) error {
			for _, p := range batch {
				if err := q.InsertEvent(context.Background(), p); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			f.Log.Error("events were not kept", "n", len(batch), "err", err)
		}
	}
}

// eventFields is an event's fields the fleet reads.
type eventFields struct {
	Event      string           `json:"event"`
	Time       int64            `json:"time"`
	Slot       int              `json:"slot"`
	Name       string           `json:"name"`
	Reason     string           `json:"reason"`
	By         string           `json:"by"`
	Map        string           `json:"map"`
	Gametype   string           `json:"gametype"`
	Variant    string           `json:"variant"`
	Duration   int64            `json:"duration"`
	TeamScores []int            `json:"team_scores"`
	Players    []map[string]any `json:"players"`
	Address    string           `json:"address"`
	HWID       string           `json:"hwid"`
	Channel    string           `json:"channel"`
	Text       string           `json:"text"`
	Machine    int              `json:"machine"`
	Action     string           `json:"action"`
	URL        string           `json:"url"`
	Listed     bool             `json:"listed"`
	Probe      string           `json:"probe"`
	Code       any              `json:"code"`
	Message    string           `json:"message"`
	Until      *int64           `json:"until"`
	After      string           `json:"after"`
	Restart    bool             `json:"restart"`
}

// onEvent is a server's event: kept, made a game's, a sentence of the feed,
// and acted on (bans, alerts).
func (f *Fleet) onEvent(node int64, e agentproto.Event) {
	var ev eventFields
	json.Unmarshal(e.Data, &ev)
	if ev.Event == "" {
		ev.Event = e.Event
	}
	// (an event's addresses are kept for the retention, as the README says)
	f.storeEvent(node, e.Slug, e.Seq, ev.Event, e.Data)
	ctx := context.Background()
	slug := e.Slug
	feed := func(kind, text string) {
		f.Live.AddFeed(live.FeedItem{Slug: slug, Kind: kind, Event: ev.Event, Text: text})
	}
	switch ev.Event {
	case "player_joined":
		feed("info", ev.Name+" joined")
		go f.checkJoin(context.Background(), slug, ev)
	case "player_left":
		if ev.Reason == "kicked" || ev.Reason == "banned" {
			feed("info", ev.Name+" left ("+ev.Reason+")")
		} else {
			feed("info", ev.Name+" left")
		}
	case "game_started":
		feed("info", fmt.Sprintf("started %s on %s, %d players", label(ev.Variant, ev.Gametype), ev.Map, len(ev.Players)))
		f.startGame(ctx, slug, ev)
	case "game_ended":
		text := fmt.Sprintf("ended %s on %s", label(ev.Variant, ev.Gametype), ev.Map)
		if len(ev.TeamScores) == 2 {
			text += fmt.Sprintf(", %d–%d", ev.TeamScores[0], ev.TeamScores[1])
		}
		text += fmt.Sprintf(" after %s (%s)", minutes(ev.Duration), ev.Reason)
		feed("info", text)
		f.endGame(ctx, slug, ev)
	case "kick", "ban", "unban", "mute", "unmute":
		text := fmt.Sprintf("%s: %s %s", ev.By, pastTense(ev.Event), ev.Name)
		if ev.Reason != "" {
			text += " (" + ev.Reason + ")"
		}
		feed("mod", text)
		if ev.Event == "ban" {
			go f.importServerBan(context.Background(), slug, ev)
		}
	case "speed_hack":
		feed("bad", fmt.Sprintf("speed hack: machine %d %s", ev.Machine, ev.Action))
		f.raiseAlert(ctx, "speed_hack", slug, fmt.Sprintf("machine %d: %s", ev.Machine, ev.Action))
	case "chat":
		f.Live.AddFeed(live.FeedItem{Slug: slug, Kind: "chat", Event: "chat", Text: ev.Name + ": " + ev.Text, Private: true})
	case "master":
		if !ev.Listed && ev.Message != "" {
			feed("warn", fmt.Sprintf("%s: not listed (%s)", ev.URL, ev.Message))
		}
	case "server_started":
		feed("info", "the server started")
	case "shutdown":
		feed("info", "the server stops ("+ev.Reason+")")
	case "drain":
		if ev.After == "cancel" {
			feed("info", "the drain is cancelled")
		} else {
			feed("info", "draining: it stops once "+map[string]string{"game": "its game ends", "empty": "it is empty"}[ev.After])
		}
	case "warning":
		feed("warn", ev.Text)
	}
}

func label(variant, gametype string) string {
	if variant != "" {
		return variant
	}
	return gametype
}

func minutes(seconds int64) string {
	if seconds < 60 {
		return fmt.Sprintf("%d s", seconds)
	}
	return fmt.Sprintf("%d min", seconds/60)
}

func pastTense(verb string) string {
	switch verb {
	case "ban":
		return "banned"
	case "unban":
		return "unbanned"
	case "mute":
		return "muted"
	case "unmute":
		return "unmuted"
	}
	return verb + "ed"
}

func (f *Fleet) startGame(ctx context.Context, slug string, ev eventFields) {
	f.gamesMu.Lock()
	defer f.gamesMu.Unlock()
	f.Store.CloseOpenGames(ctx, db.CloseOpenGamesParams{Ended: ptrInt(f.Now().Unix()), Slug: slug})
	f.Live.ResetPeak(slug)
	id, err := f.Store.StartGame(ctx, db.StartGameParams{Slug: slug, Started: f.Now().Unix(), Map: ev.Map, Gametype: ev.Gametype,
		Variant: ev.Variant, PeakPlayers: int64(len(ev.Players))})
	if err == nil {
		f.games[slug] = id
	}
}

func (f *Fleet) endGame(ctx context.Context, slug string, ev eventFields) {
	f.gamesMu.Lock()
	defer f.gamesMu.Unlock()
	id, ok := f.games[slug]
	if !ok {
		open, err := f.Store.OpenGame(ctx, slug)
		if err != nil {
			id, _ = f.Store.StartGame(ctx, db.StartGameParams{Slug: slug, Started: f.Now().Unix() - ev.Duration, Map: ev.Map,
				Gametype: ev.Gametype, Variant: ev.Variant})
		} else {
			id = open.ID
		}
	}
	delete(f.games, slug)
	scores, _ := json.Marshal(ev.TeamScores)
	if ev.TeamScores == nil {
		scores = nil
	}
	players, _ := json.Marshal(ev.Players)
	peak := f.Live.ResetPeak(slug)
	f.Store.EndGame(ctx, db.EndGameParams{Ended: ptrInt(f.Now().Unix()), Reason: ev.Reason, Duration: ev.Duration,
		TeamScores: string(scores), Players: string(players), Peak: int64(max(peak, len(ev.Players))), ID: id})
}

// sampleLoop keeps each instance's players once a minute (the sparklines).
func (f *Fleet) sampleLoop(ctx context.Context) {
	for {
		now := f.Now()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(now.Truncate(time.Minute).Add(time.Minute))):
		}
		minute := f.Now().Truncate(time.Minute).Unix()
		for slug, i := range f.Live.Instances() {
			if i.Status == nil {
				continue
			}
			f.Store.InsertSample(ctx, db.InsertSampleParams{Slug: slug, Time: minute, Players: int64(i.Status.Players),
				FrameMsMax: i.Status.FrameMsMax})
		}
		f.Live.Notify(liveChange("tiles", ""))
	}
}
