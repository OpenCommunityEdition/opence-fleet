package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type session struct {
	token, csrf, role, by string
	expires, last         time.Time
}

func random(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func answer(w http.ResponseWriter, status int, body any) {
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(append(data, '\n'))
}

func fail(w http.ResponseWriter, status int, code, message string, extra map[string]any) {
	e := map[string]any{"code": code, "message": message}
	body := map[string]any{"ok": false, "error": e}
	for k, v := range extra {
		if k == "lines" {
			body[k] = v
		} else {
			e[k] = v
		}
	}
	answer(w, status, body)
}

// loopback tells whether the socket's peer is on loopback (the owner's
// password is taken from there alone).
func loopback(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (g *game) passwordRole(password string, r *http.Request) (string, bool) {
	checks := []struct{ setting, role string }{{"server.web_owner_password", "owner"}, {"server.web_password", "admin"},
		{"server.web_moderator_password", "moderator"}, {"server.web_viewer_password", "viewer"}}
	for _, c := range checks {
		if c.role == "owner" && !options.capable {
			continue
		}
		if p := g.st.text(c.setting); p != "" && p == password {
			if c.role == "owner" && !loopback(r) {
				return "", false
			}
			return c.role, true
		}
	}
	return "", true
}

func (g *game) session(r *http.Request) *session {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		if c, err := r.Cookie("opence_session"); err == nil {
			token = c.Value
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[token]
	if s == nil || time.Now().After(s.expires) {
		return nil
	}
	s.last = time.Now()
	return s
}

var playerAction = regexp.MustCompile(`^/api/players/(\d{1,3})/(kick|ban|mute|unmute|tell)$`)

func (g *game) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch path {
		case "/healthz":
			g.mu.Lock()
			state := g.state
			g.mu.Unlock()
			if g.stalled() {
				answer(w, 503, map[string]any{"state": "stalled"})
				return
			}
			status := 200
			if state == "waiting" || state == "stopping" {
				status = 503
			}
			answer(w, status, map[string]any{"state": state})
			return
		case "/metrics":
			g.mu.Lock()
			n := len(g.players)
			g.mu.Unlock()
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			fmt.Fprintf(w, "opence_players %d\nopence_build_info{build=%q} 1\n", n, Build)
			return
		case "/api/login":
			if r.Method != http.MethodPost {
				fail(w, 405, "method", "/api/login takes POST", nil)
				return
			}
			var body struct{ Password, By string }
			json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&body)
			role, allowed := g.passwordRole(body.Password, r)
			if !allowed {
				fail(w, 403, "forbidden", "the owner's password is taken from loopback alone", nil)
				return
			}
			if role == "" {
				fail(w, 401, "denied", "a wrong password", nil)
				return
			}
			s := &session{token: random(32), csrf: random(16), role: role, by: body.By,
				expires: time.Now().Add(12 * time.Hour), last: time.Now()}
			g.mu.Lock()
			g.sessions[s.token] = s
			g.mu.Unlock()
			logLine("info", "server", fmt.Sprintf("web: %s signed in (%s)", body.By, role))
			answer(w, 200, map[string]any{"ok": true, "token": s.token, "role": role, "csrf": s.csrf, "expires": s.expires.Unix()})
			return
		case "/ws":
			g.rconUpgrade(w, r)
			return
		}
		if !strings.HasPrefix(path, "/api/") {
			if !g.st.bool("server.web_ui") {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<!doctype html><title>fakehalo</title><p>fakehalo's page")
			return
		}
		s := g.session(r)
		if s == nil {
			fail(w, 401, "denied", "no session", nil)
			return
		}
		var body map[string]any
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			decoder := json.NewDecoder(io.LimitReader(r.Body, 65536))
			decoder.UseNumber()
			if err := decoder.Decode(&body); err != nil && err != io.EOF {
				fail(w, 400, "bad_request", "the body is not JSON", nil)
				return
			}
		}
		g.api(w, r, s, path, body)
	})
}

func need(w http.ResponseWriter, s *session, role string) bool {
	if roles[s.role] < roles[role] {
		fail(w, 403, "forbidden", "needs the "+role+" role", nil)
		return false
	}
	return true
}

func text(body map[string]any, key string) string {
	if v, ok := body[key]; ok {
		return fmt.Sprint(v)
	}
	return ""
}

func (g *game) api(w http.ResponseWriter, r *http.Request, s *session, path string, body map[string]any) {
	by := s.by
	if by == "" {
		by = "web " + s.role
	}
	switch {
	case path == "/api/session":
		answer(w, 200, map[string]any{"ok": true, "role": s.role, "csrf": s.csrf, "expires": s.expires.Unix()})
	case path == "/api/logout":
		g.mu.Lock()
		delete(g.sessions, s.token)
		g.mu.Unlock()
		answer(w, 200, map[string]any{"ok": true})
	case path == "/api/status":
		g.mu.Lock()
		st := g.status(s.role)
		g.mu.Unlock()
		answer(w, 200, st)
	case path == "/api/players":
		g.mu.Lock()
		list := g.playersJSON(s.role)
		g.mu.Unlock()
		answer(w, 200, map[string]any{"ok": true, "players": list})
	case playerAction.MatchString(path):
		if !need(w, s, "moderator") {
			return
		}
		m := playerAction.FindStringSubmatch(path)
		g.mu.Lock()
		p := g.findPlayer(m[1])
		if p == nil {
			g.mu.Unlock()
			fail(w, 404, "not_found", "no player in slot "+m[1], nil)
			return
		}
		if name := text(body, "name"); name != "" && name != p.Name {
			g.mu.Unlock()
			fail(w, 409, "changed", "the slot holds another player now", map[string]any{"name": p.Name})
			return
		}
		g.mu.Unlock()
		line := m[2] + " " + m[1]
		switch m[2] {
		case "ban":
			if d := text(body, "duration"); d != "" {
				line += " " + d
			}
			if reason := text(body, "reason"); reason != "" {
				line += " " + strconv.Quote(reason)
			}
		case "tell":
			line += " " + strconv.Quote(text(body, "text"))
		}
		g.runAPI(w, line, s.role, by)
	case path == "/api/say":
		if !need(w, s, "moderator") {
			return
		}
		g.runAPI(w, "say "+strconv.Quote(text(body, "text")), s.role, by)
	case path == "/api/command":
		g.runAPI(w, text(body, "line"), s.role, by)
	case path == "/api/rotation" && r.Method == http.MethodGet:
		g.mu.Lock()
		var entries []map[string]any
		for _, e := range g.rotation {
			entries = append(entries, g.entryJSON(e))
		}
		next, _ := g.nextEntry()
		index := g.index
		g.mu.Unlock()
		answer(w, 200, map[string]any{"ok": true, "index": index, "entries": entries, "next": g.entryJSON(next)})
	case path == "/api/rotation" && r.Method == http.MethodPut:
		if !need(w, s, "admin") {
			return
		}
		var entries []string
		if list, ok := body["entries"].([]any); ok {
			for _, e := range list {
				entries = append(entries, fmt.Sprint(e))
			}
		}
		g.mu.Lock()
		parsed := g.parseRotation(strings.Join(entries, ","))
		g.mu.Unlock()
		var rejected []map[string]any
		for i, e := range parsed {
			if e.Rejected != "" {
				rejected = append(rejected, map[string]any{"index": i, "entry": e.Text, "reason": e.Rejected})
			}
		}
		if len(rejected) > 0 || len(parsed) == 0 {
			fail(w, 400, "rejected", "an entry cannot be played: nothing changed", map[string]any{"rejected": rejected})
			return
		}
		g.setRotation(entries)
		answer(w, 200, map[string]any{"ok": true})
	case path == "/api/rotation/next":
		g.runAPI(w, "sv_map_next", s.role, by)
	case path == "/api/rotation/play":
		g.runAPI(w, "sv_map "+text(body, "map")+" "+text(body, "gametype"), s.role, by)
	case path == "/api/maps":
		entries, _ := os.ReadDir(filepath.Join(g.dataRoot, "maps"))
		maps := []map[string]any{}
		for _, e := range entries {
			if name, ok := strings.CutSuffix(e.Name(), ".map"); ok && name != "ui" {
				maps = append(maps, map[string]any{"name": name, "level": `levels\test\` + name + `\` + name, "kind": "xbox", "title": name})
			}
		}
		answer(w, 200, map[string]any{"ok": true, "maps": maps})
	case path == "/api/gametypes":
		entries, _ := os.ReadDir(g.gametypesDir())
		files := []map[string]any{}
		for _, e := range entries {
			if name, ok := strings.CutSuffix(e.Name(), ".toml"); ok {
				files = append(files, map[string]any{"name": name, "base": "slayer", "description": "", "error": nil})
			}
		}
		answer(w, 200, map[string]any{"ok": true, "builtin": builtins, "schema": map[string]any{"fields": []map[string]any{
			{"key": "score_to_win", "alias": "score", "table": "universal", "engine": "all", "type": "integer", "min": 0, "max": 999,
				"choices": nil, "default": nil, "description": "The score that wins"}}}, "files": files})
	case strings.HasPrefix(path, "/api/gametypes/"):
		name := strings.TrimPrefix(path, "/api/gametypes/")
		if !regexp.MustCompile(`^[a-z0-9_-]{1,32}$`).MatchString(name) {
			fail(w, 400, "bad_request", "a gametype's name is a-z, 0-9, _ and -", nil)
			return
		}
		file := filepath.Join(g.gametypesDir(), name+".toml")
		switch r.Method {
		case http.MethodGet:
			data, err := os.ReadFile(file)
			if err != nil {
				fail(w, 404, "not_found", "no gametype "+name, nil)
				return
			}
			answer(w, 200, map[string]any{"ok": true, "name": name, "text": string(data)})
		case http.MethodPut:
			if !need(w, s, "admin") {
				return
			}
			t := text(body, "text")
			if !strings.Contains(t, "base") {
				fail(w, 400, "invalid", "a gametype file needs base", map[string]any{"line": 1})
				return
			}
			os.MkdirAll(g.gametypesDir(), 0o750)
			os.WriteFile(file, []byte(t), 0o640)
			answer(w, 200, map[string]any{"ok": true})
		case http.MethodDelete:
			if !need(w, s, "admin") {
				return
			}
			if os.Remove(file) != nil {
				fail(w, 404, "not_found", "no gametype "+name, nil)
				return
			}
			answer(w, 200, map[string]any{"ok": true})
		}
	case path == "/api/bans" && r.Method == http.MethodGet:
		if !need(w, s, "admin") {
			return
		}
		g.mu.Lock()
		list := g.bansJSON()
		g.mu.Unlock()
		answer(w, 200, map[string]any{"ok": true, "bans": list})
	case path == "/api/bans" && r.Method == http.MethodPost && options.capable:
		if !need(w, s, "admin") {
			return
		}
		b := ban{When: time.Now().UTC().Format("2006-01-02 15:04:05"), IP: text(body, "ip"), HWID: text(body, "hwid"),
			Players: text(body, "names"), Reason: text(body, "reason")}
		if id := text(body, "discord_id"); id != "" && linkCapable() {
			if !discordDigits(id) {
				fail(w, 400, "schema", "discord_id is a Discord account's id: up to 20 digits", nil)
				return
			}
			b.DiscordID, b.DiscordVerified = id, true
		}
		if b.IP == "" && b.HWID == "" && b.DiscordID == "" {
			fail(w, 400, "schema", "ip, hwid or discord_id is needed", nil)
			return
		}
		if d, ok := duration(text(body, "duration")); ok {
			until := time.Now().Add(d).Unix()
			b.Until = &until
		}
		n := g.addBan(b, by)
		answer(w, 200, map[string]any{"ok": true, "n": n})
	case strings.HasPrefix(path, "/api/bans/") && r.Method == http.MethodDelete:
		if !need(w, s, "admin") {
			return
		}
		g.runAPI(w, "unban "+strings.TrimPrefix(path, "/api/bans/"), s.role, by)
	case path == "/api/settings" && r.Method == http.MethodGet:
		if !need(w, s, "admin") {
			return
		}
		answer(w, 200, map[string]any{"ok": true, "settings": g.st.json()})
	case path == "/api/settings" && r.Method == http.MethodPut:
		if !need(w, s, "admin") {
			return
		}
		applied := []string{}
		errs := []map[string]any{}
		for _, name := range sortedKeys(body) {
			value := body[name]
			if n, ok := value.(json.Number); ok {
				value = n.String()
			}
			if code, message := g.st.set(name, value, s.role); code != "" {
				errs = append(errs, map[string]any{"name": name, "code": code, "message": message})
				continue
			}
			applied = append(applied, name)
		}
		answer(w, 200, map[string]any{"ok": true, "applied": applied, "errors": errs})
	case path == "/api/log":
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		g.ringMu.Lock()
		lines := []logEntry{}
		next := after
		for _, l := range g.logs {
			if l.N > after && (l.Cat != "chat" || roles[s.role] >= roles["admin"]) {
				lines = append(lines, l)
				next = l.N
			}
		}
		g.ringMu.Unlock()
		answer(w, 200, map[string]any{"ok": true, "lines": lines, "next": next, "missed": 0})
	case path == "/api/events":
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		g.mu.Lock()
		events := []map[string]any{}
		next := after
		for _, e := range g.events {
			if e.Seq > after {
				events = append(events, g.view(e, s.role))
				next = e.Seq
			}
		}
		g.mu.Unlock()
		answer(w, 200, map[string]any{"ok": true, "events": events, "next": next, "missed": 0})
	case path == "/api/stop" && options.capable:
		if !need(w, s, "owner") {
			return
		}
		line := "sv_stop"
		if body["restart"] == true {
			line = "sv_restart"
		}
		if body["cancel"] == true {
			line = "sv_stop cancel"
		} else {
			after := text(body, "after")
			if after == "" {
				after = "now"
			}
			line += " " + after
			if notice := text(body, "notice"); notice != "" {
				line += " " + strconv.Quote(notice)
			}
		}
		g.runAPI(w, line, s.role, by)
	default:
		fail(w, 404, "not_found", "no such endpoint", nil)
	}
}

func (g *game) runAPI(w http.ResponseWriter, line, role, by string) {
	lines, ok, refusal := g.commandCode(line, role, by)
	if refusal != nil {
		status := map[string]int{"forbidden": 403, "not_found": 404, "pinned": 409, "bad_request": 400}[refusal.code]
		fail(w, status, refusal.code, refusal.message, map[string]any{"lines": lines})
		return
	}
	if !ok {
		fail(w, 400, "failed", strings.Join(lines, "; "), map[string]any{"lines": lines})
		return
	}
	answer(w, 200, map[string]any{"ok": true, "lines": lines})
}

// rcon is an RCON connection.
type rcon struct {
	g      *game
	conn   *websocket.Conn
	role   string
	by     string
	groups map[string]bool
	send   chan any
	mu     sync.Mutex
	closed bool
}

var upgrader = websocket.Upgrader{Subprotocols: []string{"opence-rcon-1"}, CheckOrigin: func(*http.Request) bool { return true }}

func (g *game) rconUpgrade(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &rcon{g: g, conn: conn, send: make(chan any, 1024), groups: map[string]bool{}}
	go c.writer()
	c.write(map[string]any{"type": "hello", "protocol": 1, "server": g.st.text("server.name"), "build": Build,
		"network_version": NetworkVersion, "capabilities": capabilities()})
	defer c.close()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			c.write(map[string]any{"type": "error", "error": map[string]string{"code": "bad_request", "message": "not JSON"}})
			continue
		}
		switch m["type"] {
		case "auth":
			if token, ok := m["token"].(string); ok {
				g.mu.Lock()
				s := g.sessions[token]
				g.mu.Unlock()
				if s == nil {
					c.write(map[string]any{"type": "auth", "ok": false, "error": map[string]string{"code": "denied"}})
					return
				}
				c.role, c.by = s.role, s.by
			} else {
				role, allowed := g.passwordRole(fmt.Sprint(m["password"]), r)
				if !allowed || role == "" {
					c.write(map[string]any{"type": "auth", "ok": false, "error": map[string]string{"code": "denied"}})
					return
				}
				c.role, c.by = role, fmt.Sprint(m["by"])
			}
			c.write(map[string]any{"type": "auth", "ok": true, "role": c.role})
		case "command":
			if c.role == "" {
				c.write(map[string]any{"type": "reply", "id": m["id"], "ok": false, "lines": []string{},
					"error": map[string]string{"code": "denied"}})
				continue
			}
			lines, ok := g.command(fmt.Sprint(m["line"]), c.role, c.by)
			c.write(map[string]any{"type": "reply", "id": m["id"], "ok": ok, "lines": lines})
		case "subscribe":
			if c.role == "" {
				continue
			}
			c.mu.Lock()
			if list, ok := m["events"].([]any); ok {
				for _, e := range list {
					c.groups[fmt.Sprint(e)] = true
				}
			}
			c.groups["server"] = true
			c.mu.Unlock()
			after, _ := m["after"].(float64)
			g.mu.Lock()
			var replay []map[string]any
			for _, e := range g.events {
				if after > 0 && e.Seq > int64(after) && c.wants(e.Group) {
					copy := map[string]any{"replay": true}
					for k, v := range g.view(e, c.role) {
						copy[k] = v
					}
					replay = append(replay, copy)
				}
			}
			g.mu.Unlock()
			for _, e := range replay {
				c.write(e)
			}
			g.subMu.Lock()
			g.subscribers[c] = true
			g.subMu.Unlock()
			c.status()
		case "ping":
			c.write(map[string]any{"type": "pong"})
		default:
			c.write(map[string]any{"type": "error", "error": map[string]string{"code": "bad_request", "message": "unknown type"}})
		}
	}
}

func (c *rcon) wants(group string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.groups[group]
}

func (c *rcon) event(group string, data map[string]any) {
	if c.wants(group) {
		c.write(data)
	}
}

func (c *rcon) status() {
	if !c.wants("status") {
		return
	}
	c.g.mu.Lock()
	status := c.g.status(c.role)
	seq := c.g.seq
	c.g.mu.Unlock()
	c.write(map[string]any{"type": "event", "seq": seq, "time": time.Now().UnixMilli(), "event": "status", "status": status})
}

func (c *rcon) write(message any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.send <- message:
	default:
		c.closed = true
		close(c.send)
	}
}

func (c *rcon) writer() {
	for m := range c.send {
		if options.slowRCON {
			time.Sleep(50 * time.Millisecond)
		}
		c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := c.conn.WriteJSON(m); err != nil {
			break
		}
	}
	c.conn.WriteJSON(map[string]string{"type": "overflow"})
	c.conn.Close()
}

func (c *rcon) close() {
	c.g.subMu.Lock()
	delete(c.g.subscribers, c)
	c.g.subMu.Unlock()
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.send)
	}
	c.mu.Unlock()
}
