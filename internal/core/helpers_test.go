package core

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/config"
	"github.com/OpenCommunityEdition/opence-fleet/internal/crypt"
	"github.com/OpenCommunityEdition/opence-fleet/internal/live"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// logBuffer is the fleet's log, read while it is written.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// clock is a fleet's time, moved by the test (read by the fleet's
// goroutines meanwhile).
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// stop has a fleet's time stand at t (before anything runs).
func stop(f *Fleet, t time.Time) *clock {
	c := &clock{t: t}
	f.Now = c.now
	return c
}

// fleet is a core on a database of its own, its log kept (at debug).
func fleet(t *testing.T) (*Fleet, *logBuffer) {
	t.Helper()
	work := t.TempDir()
	c := &config.ServeConfig{Name: "Test fleet", Data: filepath.Join(work, "data"), DB: filepath.Join(work, "fleet.db"),
		EventsDays: 30, SamplesDays: 7, LogAddresses: true, Updates: "off", Timezone: time.UTC, SessionHours: 12}
	s, err := store.Open(c.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	logs := &logBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return New(s, crypt.New(), c, log, live.New()), logs
}

// node enrols a node: its id and its token.
func node(t *testing.T, f *Fleet, name string, settings spec.Node) (int64, string) {
	t.Helper()
	ctx := context.Background()
	join, err := f.JoinToken(ctx, "user:paul", name, settings)
	if err != nil {
		t.Fatal(err)
	}
	got, token, err := f.Join(ctx, JoinRequest{Token: join, Name: name, Runtime: "process"})
	if err != nil || got != name {
		t.Fatal(got, err)
	}
	if _, _, err := f.Join(ctx, JoinRequest{Token: join}); err == nil {
		t.Fatal("a join token worked twice")
	}
	n, err := f.NodeByName(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	return n.ID, token
}

func profile(t *testing.T, f *Fleet, name string) int64 {
	t.Helper()
	id, err := f.CreateProfile(context.Background(), "user:paul", ProfileInput{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// servers makes n servers of a profile on the nodes.
func servers(t *testing.T, f *Fleet, profileID int64, n int, running bool, nodes ...string) []string {
	t.Helper()
	slugs, _, err := f.CreateInstances(context.Background(), "user:paul", CreateRequest{ProfileID: profileID, Nodes: nodes,
		Count: n, Running: running})
	if err != nil {
		t.Fatal(err)
	}
	return slugs
}

func instance(t *testing.T, f *Fleet, slug string) db.Instance {
	t.Helper()
	row, err := f.Store.GetInstance(context.Background(), slug)
	if err != nil {
		t.Fatal(slug, err)
	}
	return row
}

func audit(t *testing.T, f *Fleet) []db.Audit {
	t.Helper()
	rows, err := f.Store.ListAudit(context.Background(), db.ListAuditParams{Lim: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// audited finds an audit row of an action.
func audited(t *testing.T, f *Fleet, action string) (db.Audit, bool) {
	t.Helper()
	for _, a := range audit(t, f) {
		if a.Action == action {
			return a, true
		}
	}
	return db.Audit{}, false
}

func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("waited for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// request is a request an agent got.
type request struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	Slug   string          `json:"slug"`
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body"`
	Action string          `json:"action"`
	Line   string          `json:"line"`
}

// agent is a node's agent that answers the hub's requests with answer
// (nil: no answer), and keeps every request.
type agent struct {
	conn     *websocket.Conn
	mu       sync.Mutex
	requests []request
	answer   func(request) *agentproto.Reply
}

// connect connects an agent for a node.
func connect(t *testing.T, f *Fleet, token string, answer func(request) *agentproto.Reply) *agent {
	t.Helper()
	server := httptest.NewServer(f.Hub)
	t.Cleanup(server.Close)
	header := map[string][]string{"Authorization": {"Bearer " + token}}
	d := websocket.Dialer{Subprotocols: []string{agentproto.Subprotocol}, HandshakeTimeout: 5 * time.Second}
	conn, _, err := d.Dial("ws"+strings.TrimPrefix(server.URL, "http"), header)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := agentproto.Marshal(agentproto.THello, agentproto.Hello{Protocol: agentproto.Protocol, Version: "test",
		Runtime: "process", Arch: "amd64"})
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatal(err)
	}
	a := &agent{conn: conn, answer: answer}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var writeMu sync.Mutex
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var r request
			if json.Unmarshal(data, &r) != nil || r.ID == "" {
				continue
			}
			a.mu.Lock()
			a.requests = append(a.requests, r)
			a.mu.Unlock()
			go func() {
				reply := a.answer(r)
				if reply == nil {
					return
				}
				reply.ID = r.ID
				data, _ := agentproto.Marshal(agentproto.TReply, reply)
				writeMu.Lock()
				conn.WriteMessage(websocket.TextMessage, data)
				writeMu.Unlock()
			}()
		}
	}()
	t.Cleanup(func() {
		conn.Close()
		<-done
	})
	return a
}

// close ends the agent's connection.
func (a *agent) close() { a.conn.Close() }

// got is the requests the agent got of a type.
func (a *agent) got(kind string) []request {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []request
	for _, r := range a.requests {
		if r.Type == kind {
			out = append(out, r)
		}
	}
	return out
}

// ok is a 200 of the server's API with a body.
func ok(body string) *agentproto.Reply {
	return &agentproto.Reply{OK: true, Status: 200, Body: json.RawMessage(body)}
}
