package view

import (
	"fmt"
	"strings"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
	"github.com/OpenCommunityEdition/opence-fleet/internal/live"
	"github.com/OpenCommunityEdition/opence-fleet/internal/serverapi"
)

// MasterDot is a master's listing of a server: a dot.
type MasterDot struct {
	Tone  Tone
	Title string
}

// ServerRow is a server as the lists and the dashboard show it (one live
// fragment: the stream re-renders it when what it shows changes).
type ServerRow struct {
	Slug, Name, Node, Profile string
	// the state's word (up: lobby, playing, stopped, crashed, blocked,
	// unknown...) and tone
	State string
	Tone  Tone
	// the game, or why there is none
	Game       string
	Players    int
	MaxPlayers int
	Masters    []MasterDot
	Build      string
	Uptime     string
	TunnelPort int64
	Groups     []string
	Running    bool
	// the spec waits for a restart; a newer release than the one it runs
	RestartNeeded   bool
	UpdateAvailable bool
	External        bool
	Status          *serverapi.Status
}

// Up tells whether the server's game runs.
func (r ServerRow) Up() bool { return strings.HasPrefix(r.State, "up") || r.State == "draining" }

// RowOf makes a server's row.
func RowOf(i core.Instance, l *live.Instance, node, profile, latest string, now time.Time) ServerRow {
	r := ServerRow{Slug: i.Slug, Name: i.Name, Node: node, Profile: profile, TunnelPort: i.TunnelPort, Groups: i.Groups,
		Running: i.Running != 0, External: i.Kind == "external", Uptime: "—"}
	if l == nil {
		r.State, r.Tone = "unknown", Neutral
		r.Game = "its node has not reported it yet"
		if !r.Running {
			r.State, r.Game = "stopped", "stopped"
		}
		return r
	}
	r.Build, r.RestartNeeded = l.Build, l.RestartNeeded
	r.UpdateAvailable = latest != "" && l.Build != "" && l.Build != latest && i.Channel == ""
	r.Status = l.Status
	switch l.State {
	case "up":
		r.Uptime = Uptime(l.Since, now)
		s := l.Status
		if s == nil {
			r.State, r.Tone, r.Game = "up", OK, "waiting for its status"
			break
		}
		r.Players, r.MaxPlayers = s.Players, s.MaxPlayers
		r.State = "up: " + s.State
		switch s.State {
		case "playing":
			r.Tone = Accent
		case "loading", "hosting", "waiting":
			r.Tone = Warn
		case "stopping":
			r.Tone = Warn
		default:
			r.Tone = OK
		}
		if s.Drain != nil {
			r.State, r.Tone = "draining", Warn
		}
		r.Game = game(s)
		for _, m := range s.Masters {
			dot := MasterDot{Tone: Neutral, Title: m.URL}
			switch {
			case m.Listed && (m.Probe == "ok" || m.Probe == ""):
				dot.Tone, dot.Title = OK, m.URL+": listed"
			case m.Probe != "" && m.Probe != "ok" && m.Probe != "pending":
				dot.Tone, dot.Title = Bad, fmt.Sprintf("%s: probe %s %s", m.URL, m.Probe, m.Message)
			case m.Code != "":
				dot.Tone, dot.Title = Warn, m.URL+": "+m.Code+" "+m.Message
			default:
				dot.Tone, dot.Title = Warn, m.URL+": not listed yet"
			}
			r.Masters = append(r.Masters, dot)
		}
	case "starting":
		r.State, r.Tone, r.Game = "starting", Warn, firstNonEmpty(l.Reason, "starting")
	case "pending":
		r.State, r.Tone, r.Game = "pending", Warn, l.Reason
	case "stopping":
		r.State, r.Tone, r.Game = "stopping", Warn, firstNonEmpty(l.Reason, "stopping")
	case "stopped":
		r.State, r.Tone, r.Game = "stopped", Neutral, firstNonEmpty(l.Reason, "stopped")
	case "crashed":
		r.State, r.Tone = "crashed", Bad
		r.Game = l.Reason
	case "blocked":
		r.State, r.Tone = "blocked", Bad
		r.Game = l.Reason
	default:
		r.State, r.Tone = "unknown", Neutral
		r.Game = "its node is offline"
	}
	return r
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// game is the game's line: the map, the gametype, the scores and the time,
// or the lobby's countdown.
func game(s *serverapi.Status) string {
	parts := []string{MapTitle(s.Map)}
	gametype := s.Variant
	if gametype == "" {
		gametype = s.Gametype
	}
	if gametype != "" {
		parts = append(parts, gametype)
	}
	switch s.State {
	case "playing":
		if len(s.TeamScores) == 2 {
			parts = append(parts, fmt.Sprintf("%d–%d", s.TeamScores[0], s.TeamScores[1]))
		}
		parts = append(parts, Duration(time.Duration(s.GameSeconds)*time.Second))
	case "lobby":
		if s.Countdown >= 0 {
			parts = append(parts, fmt.Sprintf("starts in %d s", s.Countdown))
		} else if s.Players == 0 {
			parts = append(parts, "waiting for players")
		}
	case "postgame":
		parts = append(parts, "scores")
	}
	return strings.Join(parts, " · ")
}

// FeedLine is a feed item, as the feed shows it.
type FeedLine struct {
	Time   string
	Slug   string
	Server string
	Text   string
	Tone   Tone
}

// FeedOf makes the feed's lines (a viewer's without the private ones).
func FeedOf(items []live.FeedItem, names map[string]string, admin bool, zone *time.Location) []FeedLine {
	var out []FeedLine
	for _, it := range items {
		if it.Private && !admin {
			continue
		}
		tone := Neutral
		switch it.Kind {
		case "ok":
			tone = OK
		case "bad":
			tone = Bad
		case "warn":
			tone = Warn
		case "mod":
			tone = Info
		case "chat":
			tone = Neutral
		}
		name := names[it.Slug]
		if name == "" {
			name = it.Slug
		}
		out = append(out, FeedLine{Time: Clock(it.Time, zone), Slug: it.Slug, Server: name, Text: it.Text, Tone: tone})
	}
	return out
}

// NodeCard is a node as the nodes page shows it.
type NodeCard struct {
	core.Node
	Online    bool
	LastSeen  string
	Load      string
	Memory    string
	Disk      string
	Version   string
	Runtime   string
	Instances int
	Images    []string
	Missing   []string
	Syncs     []string
	Arch      string
	Docker    string
}

// NodeCardOf makes a node's card.
func NodeCardOf(n core.Node, l *live.Node, instances int, now time.Time) NodeCard {
	c := NodeCard{Node: n, Instances: instances, Version: n.AgentVersion, Runtime: n.Settings.Runtime, Arch: n.Facts.Arch,
		Docker: n.Facts.Docker}
	if c.Runtime == "" {
		c.Runtime = n.Facts.Runtime
	}
	if n.LastSeen != nil {
		c.LastSeen = Since(time.Unix(*n.LastSeen, 0), now)
	} else {
		c.LastSeen = "never"
	}
	if l != nil {
		c.Online = l.Online
		h := l.Heartbeat
		if h.MemoryTotal > 0 {
			c.Load = fmt.Sprintf("%.2f", h.Load)
			c.Memory = fmt.Sprintf("%s of %s", Bytes(h.MemoryUsed), Bytes(h.MemoryTotal))
			c.Disk = fmt.Sprintf("%s of %s", Bytes(h.DiskUsed), Bytes(h.DiskTotal))
		}
		if h.Version != "" {
			c.Version = h.Version
		}
		c.Images, c.Missing = h.Images, h.Missing
		for _, k := range Sorted(l.Syncs) {
			s := l.Syncs[k]
			if s.Error != "" {
				c.Syncs = append(c.Syncs, fmt.Sprintf("%s: %s", s.Name, s.Error))
			} else if s.Total > 0 {
				c.Syncs = append(c.Syncs, fmt.Sprintf("%s: %s of %s", s.Name, Bytes(s.Done), Bytes(s.Total)))
			}
		}
		if l.Hello.Arch != "" {
			c.Arch, c.Docker = l.Hello.Arch, l.Hello.Docker
			if c.Runtime == "" {
				c.Runtime = l.Hello.Runtime
			}
		}
	}
	return c
}
