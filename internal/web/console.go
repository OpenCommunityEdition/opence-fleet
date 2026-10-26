package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/auth"
)

// console is a server's console over a WebSocket: its log (the control
// plane's ring, then live), the commands of the role (the owner's on the
// server's standard input), and the role's commands for completion.
func (s *Server) console(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if _, err := s.Fleet.Instance(r.Context(), slug); err != nil {
		http.NotFound(w, r)
		return
	}
	p := principal(r)
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		return origin == "" || s.sameOrigin(r, origin)
	}}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	out := make(chan any, 512)
	visible := func(l agentproto.Log) bool { return !l.Private || p.Can(auth.Admin) }
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-out:
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := conn.WriteJSON(m); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	send := func(m any) {
		select {
		case out <- m:
		default:
		}
	}
	changes, unsubscribe := s.Fleet.Live.Subscribe()
	defer unsubscribe()
	var lines []agentproto.Log
	for _, l := range s.Fleet.Live.Logs(slug, 400) {
		if visible(l) {
			lines = append(lines, l)
		}
	}
	send(map[string]any{"type": "lines", "lines": lines})
	go func() {
		// (through the API, at most the admin's: the owner's console would
		// echo it in the log)
		role := roleFor(p)
		if role == auth.Owner {
			role = auth.Admin
		}
		help, err := s.Fleet.Command(context.Background(), "auto", slug, "help", role)
		if err == nil {
			var words []string
			for _, line := range help {
				words = append(words, strings.Fields(line)...)
			}
			send(map[string]any{"type": "help", "commands": words})
		}
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case c, ok := <-changes:
				if !ok {
					return
				}
				if c.Kind == "log" && c.Key == slug && c.Log != nil && visible(*c.Log) {
					send(map[string]any{"type": "line", "line": c.Log})
				}
			}
		}
	}()
	conn.SetReadLimit(4096)
	for {
		var in struct {
			Line string `json:"line"`
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if json.Unmarshal(data, &in) != nil || strings.TrimSpace(in.Line) == "" {
			continue
		}
		if !p.Can(auth.Moderator) {
			send(map[string]any{"type": "reply", "line": in.Line, "ok": false, "lines": []string{"a viewer reads the log alone"}})
			continue
		}
		lines, err := s.Fleet.Command(ctx, p.Actor(), slug, in.Line, roleFor(p))
		reply := map[string]any{"type": "reply", "line": in.Line, "ok": err == nil, "lines": lines}
		if err != nil && len(lines) == 0 {
			reply["lines"] = []string{err.Error()}
		}
		send(reply)
	}
}

// roleFor is the server role a fleet user's commands run with.
func roleFor(p *auth.Principal) string {
	if p == nil {
		return auth.Viewer
	}
	return p.Role
}
