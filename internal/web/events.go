package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/OpenCommunityEdition/opence-fleet/internal/auth"
	"github.com/OpenCommunityEdition/opence-fleet/internal/live"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view/pages"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view/parts"
)

// events is the live stream: server-sent events of re-rendered fragments,
// named by what they replace (instance:<slug>, server:<slug>, tiles,
// alerts, feed, node:<name>, job:<id>), rendered for the viewer's role, and
// coalesced: a change that changes nothing visible sends nothing.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", http.StatusInternalServerError)
		return
	}
	p := principal(r)
	slug := r.URL.Query().Get("server")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	changes, cancel := s.Fleet.Live.Subscribe()
	defer cancel()
	ctx := r.Context()
	id := time.Now().UnixMilli()
	sent := map[string]string{}
	send := func(event, data string) {
		// (a fragment that replaces is sent when it changed; a feed's item,
		// which is added, each time)
		if !strings.HasPrefix(event, "feed") {
			if sent[event] == data {
				return
			}
			sent[event] = data
		}
		id++
		fmt.Fprintf(w, "id: %d\nevent: %s\n", id, event)
		for _, line := range strings.Split(data, "\n") {
			fmt.Fprintf(w, "data: %s\n", line)
		}
		fmt.Fprint(w, "\n")
	}
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()
	pending := map[string]live.Change{}
	var feeds []*live.FeedItem
	flush := time.NewTicker(300 * time.Millisecond)
	defer flush.Stop()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	if r.Header.Get("Last-Event-ID") != "" {
		// (a reconnect: every row again, which the page may have missed)
		instances, _ := s.Fleet.Instances(ctx)
		for _, i := range instances {
			pending["instance:"+i.Slug] = live.Change{Kind: "instance", Key: i.Slug}
		}
		pending["tiles"] = live.Change{Kind: "tiles"}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case c, ok := <-changes:
			if !ok {
				return
			}
			switch c.Kind {
			case "feed":
				if c.Feed != nil && (!c.Feed.Private || p.Can(auth.Admin)) {
					feeds = append(feeds, c.Feed)
				}
			case "log", "players":
			case "resync":
				send("resync", "reload")
				flusher.Flush()
			default:
				pending[c.Kind+":"+c.Key] = c
			}
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-flush.C:
			if len(pending) == 0 && len(feeds) == 0 {
				continue
			}
			s.renderChanges(ctx, p, slug, pending, feeds, send)
			pending = map[string]live.Change{}
			feeds = nil
			flusher.Flush()
		}
	}
}

func (s *Server) renderChanges(ctx context.Context, p *auth.Principal, slug string, pending map[string]live.Change,
	feeds []*live.FeedItem, send func(event, data string)) {
	render := func(c templ.Component) string { return view.String(ctx, c) }
	var d *fleetData
	data := func() fleetData {
		if d == nil {
			v := s.data(ctx)
			d = &v
		}
		return *d
	}
	for _, c := range pending {
		switch c.Kind {
		case "instance":
			if row, ok := s.rowOf(ctx, c.Key); ok {
				send("instance:"+c.Key, render(parts.RowCells(row)))
			} else {
				send("instance:"+c.Key, `<div id="row-`+templ.EscapeString(c.Key)+`" class="text-sm text-faint">removed</div>`)
			}
			if c.Key == slug {
				if live, ok := s.serverLive(ctx, slug, p); ok {
					send("server:"+slug, render(live))
				}
			}
		case "tiles":
			send("tiles", render(pages.TilesView(s.tiles(ctx, data()))))
		case "alert":
			send("alerts", render(pages.AlertsCard(s.alertLines(ctx, 4), view.Page{User: p})))
		case "node":
			if card, ok := s.nodeCard(ctx, c.Key); ok {
				send("node:"+c.Key, render(pages.NodeCardView(card)))
			}
		case "job":
			if id, err := strconv.ParseInt(c.Key, 10, 64); err == nil {
				if job, items, err := s.Fleet.Job(ctx, id); err == nil {
					send("job:"+c.Key, render(pages.JobTable(job, items, s.Fleet.Location())))
				}
			}
		}
	}
	for _, f := range feeds {
		lines := view.FeedOf([]live.FeedItem{*f}, data().names, p.Can(auth.Admin), s.Fleet.Location())
		if len(lines) == 0 {
			continue
		}
		html := render(parts.FeedItem(lines[0]))
		send("feed", html)
		if f.Slug != "" {
			send("feed:"+f.Slug, html)
		}
	}
}
