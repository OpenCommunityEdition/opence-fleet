package hub_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/hub"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

// sink is a fleet of two nodes, "alpha" (token-alpha) and "beta"
// (token-beta), that keeps what the hub tells it.
type sink struct {
	mu           sync.Mutex
	nodes        map[string]int64
	names        map[int64]string
	address      string
	generation   int64
	hellos       []agentproto.Hello
	reports      []string
	disconnected chan int64
}

func newSink() *sink {
	s := &sink{nodes: map[string]int64{}, names: map[int64]string{1: "alpha", 2: "beta"}, disconnected: make(chan int64, 8)}
	s.nodes[hex.EncodeToString(hub.TokenHash("token-alpha"))] = 1
	s.nodes[hex.EncodeToString(hub.TokenHash("token-beta"))] = 2
	return s
}

func (s *sink) Authenticate(_ context.Context, hash []byte) (int64, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node, ok := s.nodes[hex.EncodeToString(hash)]
	return node, s.names[node], ok
}

// Spec is a new generation every time (as the core's is), the rest the same
// until the address changes.
func (s *sink) Spec(_ context.Context, node int64) (spec.Spec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generation++
	n := spec.DefaultNode()
	n.PublicAddress = s.address
	return spec.Spec{Generation: s.generation, Node: n, Images: []spec.Image{}, Releases: []spec.Release{},
		Library: []spec.LibraryItem{}, Instances: []spec.Instance{}}, nil
}

func (s *sink) setAddress(a string) {
	s.mu.Lock()
	s.address = a
	s.mu.Unlock()
}

func (s *sink) Hello(_ int64, _ string, hello agentproto.Hello) {
	s.mu.Lock()
	s.hellos = append(s.hellos, hello)
	s.mu.Unlock()
}

func (s *sink) Report(_ int64, kind string, _ []byte) {
	s.mu.Lock()
	s.reports = append(s.reports, kind)
	s.mu.Unlock()
}

func (s *sink) Disconnected(node int64) { s.disconnected <- node }

func start(t *testing.T) (*hub.Hub, *sink, string) {
	t.Helper()
	s := newSink()
	h := hub.New(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	return h, s, "ws" + strings.TrimPrefix(server.URL, "http")
}

// dial connects as an agent: its token ("": none) and subprotocols.
func dial(t *testing.T, url, token string, protocols ...string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	d := websocket.Dialer{Subprotocols: protocols, HandshakeTimeout: 5 * time.Second}
	conn, response, err := d.Dial(url, header)
	if conn != nil {
		t.Cleanup(func() { conn.Close() })
	}
	return conn, response, err
}

func send(t *testing.T, conn *websocket.Conn, kind string, message any) {
	t.Helper()
	data, err := agentproto.Marshal(kind, message)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatal(err)
	}
}

// read is the next message but pings.
func read(t *testing.T, conn *websocket.Conn) (string, []byte) {
	t.Helper()
	for {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("no message: %v", err)
		}
		e, err := agentproto.Kind(data)
		if err != nil {
			t.Fatalf("%s: %v", data, err)
		}
		if e.Type != agentproto.TPing {
			return e.Type, data
		}
	}
}

// closed reads the close message and its code.
func closed(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	kind, data := read(t, conn)
	if kind != agentproto.TClose {
		t.Fatalf("a %s rather than a close: %s", kind, data)
	}
	var c agentproto.Close
	json.Unmarshal(data, &c)
	return c.Code
}

// connect is an agent of node alpha (or the token's) through its hello and
// first spec.
func connect(t *testing.T, url, token string) (*websocket.Conn, spec.Spec) {
	t.Helper()
	conn, _, err := dial(t, url, token, agentproto.Subprotocol)
	if err != nil {
		t.Fatal(err)
	}
	send(t, conn, agentproto.THello, agentproto.Hello{Protocol: agentproto.Protocol, Node: "alpha", Version: "test",
		Runtime: "process"})
	kind, data := read(t, conn)
	if kind != agentproto.TSpec {
		t.Fatalf("a %s first rather than the spec: %s", kind, data)
	}
	var sp agentproto.SpecMessage
	if err := json.Unmarshal(data, &sp); err != nil {
		t.Fatal(err)
	}
	return conn, sp.Spec
}

func waitOnline(t *testing.T, h *hub.Hub, node int64, online bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.Online(node) != online {
		if time.Now().After(deadline) {
			t.Fatalf("node %d online is not %v", node, online)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRefusedBeforeTheUpgrade(t *testing.T) {
	_, _, url := start(t)
	for _, c := range []struct{ name, token string }{
		{"no token", ""},
		{"an unknown token", "token-gamma"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, response, err := dial(t, url, c.token, agentproto.Subprotocol)
			if !errors.Is(err, websocket.ErrBadHandshake) || response == nil || response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%v %v", err, response)
			}
			body, _ := io.ReadAll(response.Body)
			if !strings.Contains(string(body), `"denied"`) {
				t.Fatalf("%s", body)
			}
		})
	}
}

func TestAllowedRefuses(t *testing.T) {
	h, _, url := start(t)
	h.Allowed = func(r *http.Request) bool { return false }
	_, response, err := dial(t, url, "token-alpha", agentproto.Subprotocol)
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("%v %v", err, response)
	}
	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), "tls_required") {
		t.Fatalf("%s", body)
	}
}

func TestClosedBeforeTheSession(t *testing.T) {
	h, s, url := start(t)
	// another subprotocol: told to update
	conn, _, err := dial(t, url, "token-alpha", "opence-fleet-agent-0")
	if err != nil {
		t.Fatal(err)
	}
	if code := closed(t, conn); code != "version" {
		t.Fatalf("closed with %q", code)
	}
	// hello first
	conn, _, err = dial(t, url, "token-alpha", agentproto.Subprotocol)
	if err != nil {
		t.Fatal(err)
	}
	send(t, conn, agentproto.THeartbeat, agentproto.Heartbeat{})
	if code := closed(t, conn); code != "bad_request" {
		t.Fatalf("closed with %q", code)
	}
	// a hello of another protocol
	conn, _, err = dial(t, url, "token-alpha", agentproto.Subprotocol)
	if err != nil {
		t.Fatal(err)
	}
	send(t, conn, agentproto.THello, agentproto.Hello{Protocol: agentproto.Protocol + 1})
	if code := closed(t, conn); code != "version" {
		t.Fatalf("closed with %q", code)
	}
	if h.Online(1) {
		t.Fatal("a refused agent is online")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.hellos) != 0 {
		t.Fatalf("the sink heard %d hellos", len(s.hellos))
	}
}

func TestSpecOnConnectAndOnChange(t *testing.T) {
	h, s, url := start(t)
	conn, first := connect(t, url, "token-alpha")
	if first.Node.TunnelPorts != spec.DefaultNode().TunnelPorts || first.Generation == 0 {
		t.Fatalf("%+v", first)
	}
	s.mu.Lock()
	if len(s.hellos) != 1 || s.hellos[0].Runtime != "process" {
		t.Fatalf("%+v", s.hellos)
	}
	s.mu.Unlock()
	ctx := context.Background()
	// the same spec but its generation: not sent again
	for range 3 {
		if err := h.Push(ctx, 1); err != nil {
			t.Fatal(err)
		}
	}
	s.setAddress("203.0.113.9")
	if err := h.Push(ctx, 1); err != nil {
		t.Fatal(err)
	}
	kind, data := read(t, conn)
	var next agentproto.SpecMessage
	json.Unmarshal(data, &next)
	if kind != agentproto.TSpec || next.Node.PublicAddress != "203.0.113.9" {
		t.Fatalf("the next message is %s: %s", kind, data)
	}
	// (Push to a node that is not connected is nothing: it gets its spec at
	// its connect)
	if err := h.Push(ctx, 2); err != nil {
		t.Fatal(err)
	}
	// the agent's reports reach the sink; its pings are answered
	send(t, conn, agentproto.THeartbeat, agentproto.Heartbeat{Load: 1})
	send(t, conn, agentproto.TPing, struct{}{})
	if kind, data := read(t, conn); kind != agentproto.TPong {
		t.Fatalf("%s: %s", kind, data)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reports) != 1 || s.reports[0] != agentproto.THeartbeat {
		t.Fatalf("%v", s.reports)
	}
}

func TestRequestAndReply(t *testing.T) {
	h, _, url := start(t)
	ctx := context.Background()
	if _, err := h.Request(ctx, 1, agentproto.TConsole, agentproto.Console{Slug: "a", Line: "sv_status"}); !errors.Is(err, hub.ErrOffline) {
		t.Fatalf("a request to an offline node: %v", err)
	}
	if err := h.Send(1, agentproto.TForget, agentproto.Forget{Slug: "a"}); !errors.Is(err, hub.ErrOffline) {
		t.Fatalf("a message to an offline node: %v", err)
	}
	conn, _ := connect(t, url, "token-alpha")
	// the agent answers each request by its id, the second first
	go func() {
		var requests []agentproto.Console
		for len(requests) < 2 {
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var c agentproto.Console
			if e, _ := agentproto.Kind(data); e.Type == agentproto.TConsole && json.Unmarshal(data, &c) == nil {
				requests = append(requests, c)
			}
		}
		for i := len(requests) - 1; i >= 0; i-- {
			r := requests[i]
			data, _ := agentproto.Marshal(agentproto.TReply, agentproto.Reply{ID: r.ID, OK: true, Lines: []string{"ran " + r.Line}})
			conn.WriteMessage(websocket.TextMessage, data)
		}
	}()
	var wg sync.WaitGroup
	answers := make([]agentproto.Reply, 2)
	errs := make([]error, 2)
	for i, line := range []string{"sv_status", "sv_players"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			call, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			answers[i], errs[i] = h.Request(call, 1, agentproto.TConsole, agentproto.Console{Slug: "a", Line: line})
		}()
		// (the first one sent first)
		time.Sleep(50 * time.Millisecond)
	}
	wg.Wait()
	for i, line := range []string{"sv_status", "sv_players"} {
		if errs[i] != nil || !answers[i].OK || len(answers[i].Lines) != 1 || answers[i].Lines[0] != "ran "+line {
			t.Fatalf("%s: %+v %v", line, answers[i], errs[i])
		}
	}
	// a request not answered ends with its context
	call, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := h.Request(call, 1, agentproto.TConsole, agentproto.Console{Slug: "a", Line: "x"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

func TestReplacedAndDisconnected(t *testing.T) {
	h, s, url := start(t)
	first, _ := connect(t, url, "token-alpha")
	waitOnline(t, h, 1, true)
	// a request pending on the first connection ends with it
	pending := make(chan error, 1)
	go func() {
		call, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := h.Request(call, 1, agentproto.TConsole, agentproto.Console{Slug: "a", Line: "x"})
		pending <- err
	}()
	if kind, _ := read(t, first); kind != agentproto.TConsole {
		t.Fatalf("a %s", kind)
	}
	second, _ := connect(t, url, "token-alpha")
	if code := closed(t, first); code != "replaced" {
		t.Fatalf("the first connection closed with %q", code)
	}
	if err := <-pending; !errors.Is(err, hub.ErrOffline) {
		t.Fatalf("the pending request: %v", err)
	}
	// (the replaced connection's end is not the node's)
	select {
	case node := <-s.disconnected:
		t.Fatalf("node %d disconnected by its replacement", node)
	case <-time.After(200 * time.Millisecond):
	}
	if !h.Online(1) {
		t.Fatal("the node is offline with its second connection")
	}
	// the second connection ends: the node is offline
	second.Close()
	select {
	case node := <-s.disconnected:
		if node != 1 {
			t.Fatalf("node %d disconnected", node)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Disconnected was not called")
	}
	waitOnline(t, h, 1, false)
	// Disconnect closes a node's connection with "revoked"
	third, _ := connect(t, url, "token-alpha")
	waitOnline(t, h, 1, true)
	h.Disconnect(1, "the node was forgotten")
	if code := closed(t, third); code != "revoked" {
		t.Fatalf("closed with %q", code)
	}
	<-s.disconnected
	waitOnline(t, h, 1, false)
}

func TestTwoNodes(t *testing.T) {
	h, s, url := start(t)
	connect(t, url, "token-alpha")
	beta, _ := connect(t, url, "token-beta")
	waitOnline(t, h, 1, true)
	waitOnline(t, h, 2, true)
	// a message to one node reaches that node alone
	if err := h.Send(2, agentproto.TForget, agentproto.Forget{Slug: "b"}); err != nil {
		t.Fatal(err)
	}
	if kind, data := read(t, beta); kind != agentproto.TForget || !strings.Contains(string(data), `"b"`) {
		t.Fatalf("%s: %s", kind, data)
	}
	// PushAll sends each node its changed spec
	s.setAddress("198.51.100.1")
	h.PushAll(context.Background())
	if kind, data := read(t, beta); kind != agentproto.TSpec || !strings.Contains(string(data), "198.51.100.1") {
		t.Fatalf("%s: %s", kind, data)
	}
}
