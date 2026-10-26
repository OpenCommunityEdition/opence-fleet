package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view/pages"
)

// fleetData is what most pages read of the fleet at once.
type fleetData struct {
	instances []core.Instance
	nodes     map[int64]core.Node
	profiles  map[int64]string
	latest    string
	names     map[string]string
}

func (s *Server) data(ctx context.Context) fleetData {
	d := fleetData{nodes: map[int64]core.Node{}, profiles: map[int64]string{}, names: map[string]string{}}
	d.instances, _ = s.Fleet.Instances(ctx)
	nodes, _ := s.Fleet.Nodes(ctx)
	for _, n := range nodes {
		d.nodes[n.ID] = n
	}
	if rows, err := s.Fleet.Store.ListProfiles(ctx); err == nil {
		for _, p := range rows {
			d.profiles[p.ID] = p.Name
		}
	}
	if latest, err := s.Fleet.Store.GetLatestRelease(ctx); err == nil {
		d.latest = latest.Build
	}
	for _, i := range d.instances {
		d.names[i.Slug] = i.Name
	}
	return d
}

func (d fleetData) row(s *Server, i core.Instance) view.ServerRow {
	profile := ""
	if i.ProfileID != nil {
		profile = d.profiles[*i.ProfileID]
	}
	return view.RowOf(i, s.Fleet.Live.Instance(i.Slug), d.nodes[i.NodeID].Name, profile, d.latest, s.Fleet.Now())
}

// rowOf is one instance's row (the live stream's).
func (s *Server) rowOf(ctx context.Context, slug string) (view.ServerRow, bool) {
	i, err := s.Fleet.Instance(ctx, slug)
	if err != nil {
		return view.ServerRow{}, false
	}
	d := fleetData{nodes: map[int64]core.Node{}, profiles: map[int64]string{}}
	if n, err := s.Fleet.NodeByID(ctx, i.NodeID); err == nil {
		d.nodes[n.ID] = n
	}
	if i.ProfileID != nil {
		if p, err := s.Fleet.Store.GetProfile(ctx, *i.ProfileID); err == nil {
			d.profiles[p.ID] = p.Name
		}
	}
	if latest, err := s.Fleet.Store.GetLatestRelease(ctx); err == nil {
		d.latest = latest.Build
	}
	return d.row(s, i), true
}

func (s *Server) tiles(ctx context.Context, d fleetData) pages.Tiles {
	t := pages.Tiles{Total: len(d.instances), Players: s.Fleet.Live.Players(), Nodes: len(d.nodes)}
	for _, i := range d.instances {
		r := d.row(s, i)
		if r.Up() {
			t.Up++
		}
		if r.State == "blocked" {
			t.Blocked = append(t.Blocked, i.Slug)
		}
	}
	var names []string
	for _, n := range d.nodes {
		if s.Fleet.NodeOnline(n.ID) {
			t.NodesOnline++
		}
		names = append(names, n.Name)
	}
	sort.Strings(names)
	t.NodeNames = strings.Join(names, " · ")
	if len(names) == 0 {
		t.NodeNames = "none yet"
	}
	// (the players of the last 24 hours, by half hour)
	since := s.Fleet.Now().Add(-24 * time.Hour)
	samples, _ := s.Fleet.Store.ListSamplesSince(ctx, since.Truncate(time.Minute).Unix())
	buckets := make([]float64, 48)
	counts := map[int64]map[string]int64{}
	for _, sm := range samples {
		minute := sm.Time
		if counts[minute] == nil {
			counts[minute] = map[string]int64{}
		}
		counts[minute][sm.Slug] = sm.Players
	}
	peak, peakAt := int64(0), int64(0)
	for minute, bySlug := range counts {
		total := int64(0)
		for _, n := range bySlug {
			total += n
		}
		if total > peak {
			peak, peakAt = total, minute
		}
		b := int((minute - since.Unix()) / 1800)
		if b >= 0 && b < 48 && float64(total) > buckets[b] {
			buckets[b] = float64(total)
		}
	}
	if len(samples) > 0 {
		t.Spark = view.SparkPoints(buckets)
	}
	if peak > 0 {
		t.Peak = fmt.Sprintf("peak %d at %s, last 24 h", peak, view.Clock(time.Unix(peakAt, 0), s.Fleet.Location()))
	} else {
		t.Peak = "last 24 h"
	}
	alerts, _ := s.Fleet.Store.ListOpenAlerts(ctx)
	t.Alerts = len(alerts)
	if len(alerts) > 0 {
		t.FirstAlert = alerts[0].Target + ": " + core.RuleName(alerts[0].Rule)
	}
	return t
}

func (s *Server) alertLines(ctx context.Context, limit int) []pages.AlertLine {
	alerts, _ := s.Fleet.Store.ListOpenAlerts(ctx)
	var out []pages.AlertLine
	for i, a := range alerts {
		if limit > 0 && i == limit {
			break
		}
		out = append(out, pages.AlertLine{ID: a.ID, Rule: core.RuleName(a.Rule), Target: a.Target, Detail: a.Detail,
			Since: view.Unix(a.FirstSeen, s.Fleet.Location()), Told: a.Delivered, Tone: view.Bad})
	}
	return out
}

func (s *Server) groups(d fleetData) []pages.NodeGroup {
	byNode := map[int64][]core.Instance{}
	for _, i := range d.instances {
		byNode[i.NodeID] = append(byNode[i.NodeID], i)
	}
	var out []pages.NodeGroup
	ids := make([]int64, 0, len(d.nodes))
	for id := range d.nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return d.nodes[ids[a]].Name < d.nodes[ids[b]].Name })
	for _, id := range ids {
		n := d.nodes[id]
		list := byNode[id]
		if len(list) == 0 {
			continue
		}
		g := pages.NodeGroup{Name: n.Name, Online: s.Fleet.NodeOnline(id)}
		line := []string{view.Count(len(list), "server", "servers")}
		if l := s.Fleet.Live.Node(id); l != nil && l.Heartbeat.MemoryTotal > 0 {
			line = append(line, fmt.Sprintf("load %.2f", l.Heartbeat.Load),
				fmt.Sprintf("%s of %s", view.Bytes(l.Heartbeat.MemoryUsed), view.Bytes(l.Heartbeat.MemoryTotal)))
		}
		builds := map[string]bool{}
		for _, i := range list {
			r := d.row(s, i)
			g.Rows = append(g.Rows, r)
			if r.Build != "" {
				builds[r.Build] = true
			}
		}
		line = append(line, strings.Join(view.Sorted(builds), ", "))
		g.Line = strings.Join(line, " · ")
		out = append(out, g)
	}
	return out
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := s.data(ctx)
	p := principal(r)
	page := s.page(r, "Dashboard", "dashboard")
	page.Live = true
	feed := view.FeedOf(s.Fleet.Live.Feed(50, ""), d.names, p.Can("admin"), s.Fleet.Location())
	banner := ""
	if d.latest != "" {
		behind := 0
		for _, i := range d.instances {
			if l := s.Fleet.Live.Instance(i.Slug); l != nil && l.Build != "" && l.Build != d.latest && i.Channel == "" {
				behind++
			}
		}
		if behind > 0 {
			banner = fmt.Sprintf("%s is out · %s behind", d.latest, view.Count(behind, "server", "servers"))
		}
	}
	s.render(w, r, http.StatusOK, pages.Dashboard(page, s.tiles(ctx, d), s.groups(d), s.alertLines(ctx, 4), feed, banner))
}
