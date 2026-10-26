// Package hub is the control plane's end of the agents' protocol (package
// agentproto): the /agent WebSocket, one session per connected node, the
// node's spec sent whole on connect and on every change, the requests to an
// agent and their replies, and the agent's reports passed to the Sink (the
// fleet's core). A node token authenticates one node; a node is marked
// offline after 45 seconds without a word from it.
package hub

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

// Sink is what the hub tells of the agents, and asks of the fleet.
type Sink interface {
	// Authenticate finds the node of a token's SHA-256.
	Authenticate(ctx context.Context, tokenHash []byte) (int64, string, bool)
	// Spec is a node's desired state.
	Spec(ctx context.Context, node int64) (spec.Spec, error)
	// Hello, Report and Disconnected tell what a node's agent said and did.
	Hello(node int64, name string, hello agentproto.Hello)
	Report(node int64, kind string, data []byte)
	Disconnected(node int64)
}

// ErrOffline is a request to a node that is not connected.
var ErrOffline = errors.New("the node is offline")

// Hub holds the agents' sessions.
type Hub struct {
	Sink Sink
	Log  *slog.Logger
	// Allowed tells whether a request may connect an agent (TLS or
	// loopback, as the control plane's forms are)
	Allowed func(r *http.Request) bool

	mu       sync.Mutex
	sessions map[int64]*session
	ids      atomic.Int64
}

// New makes a hub.
func New(sink Sink, log *slog.Logger) *Hub {
	return &Hub{Sink: sink, Log: log, sessions: map[int64]*session{}}
}

type session struct {
	h       *Hub
	node    int64
	name    string
	conn    *websocket.Conn
	send    chan []byte
	pending sync.Map
	done    chan struct{}
	once    sync.Once
	// (one writer at a time: the writer's frames, a close's)
	writeMu sync.Mutex
	// the generation of the last spec sent, so that a push of the same is
	// not sent twice
	specMu   sync.Mutex
	lastSpec []byte
}

var upgrader = websocket.Upgrader{
	Subprotocols: []string{agentproto.Subprotocol},
	// (an agent is no browser: it sends no Origin, and a browser's page
	// cannot add the node's bearer token)
	CheckOrigin:     func(r *http.Request) bool { return r.Header.Get("Origin") == "" },
	ReadBufferSize:  64 << 10,
	WriteBufferSize: 64 << 10,
}

// TokenHash is the SHA-256 of a node token, as the database keeps it.
func TokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// ServeHTTP is GET /agent.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Allowed != nil && !h.Allowed(r) {
		http.Error(w, `{"ok":false,"error":{"code":"tls_required","message":"agents connect over HTTPS"}}`, http.StatusForbidden)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		http.Error(w, `{"ok":false,"error":{"code":"denied","message":"a node token is needed"}}`, http.StatusUnauthorized)
		return
	}
	node, name, ok := h.Sink.Authenticate(r.Context(), TokenHash(strings.TrimSpace(token)))
	if !ok {
		h.Log.Warn("an agent's token was refused", "address", r.RemoteAddr)
		http.Error(w, `{"ok":false,"error":{"code":"denied","message":"the node token is not one of this fleet's (forgotten, or enrolled again)"}}`,
			http.StatusUnauthorized)
		return
	}
	protocols := websocket.Subprotocols(r)
	speaks := false
	for _, p := range protocols {
		if p == agentproto.Subprotocol {
			speaks = true
		}
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	if !speaks {
		closeWith(conn, "version", fmt.Sprintf("this control plane speaks %s: update the agent (%v)", agentproto.Subprotocol, protocols))
		return
	}
	conn.SetReadLimit(16 << 20)
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return
	}
	var hello struct {
		Type string `json:"type"`
		agentproto.Hello
	}
	if json.Unmarshal(data, &hello) != nil || hello.Type != agentproto.THello {
		closeWith(conn, "bad_request", "the agent speaks first, with hello")
		return
	}
	if hello.Protocol != agentproto.Protocol {
		closeWith(conn, "version", fmt.Sprintf("protocol %d; this control plane speaks %d", hello.Protocol, agentproto.Protocol))
		return
	}
	s := &session{h: h, node: node, name: name, conn: conn, send: make(chan []byte, 4096), done: make(chan struct{})}
	h.mu.Lock()
	if old := h.sessions[node]; old != nil {
		old.close("replaced", "another connection of this node's")
	}
	h.sessions[node] = s
	h.mu.Unlock()
	h.Log.Info("an agent connected", "node", name, "version", hello.Version, "runtime", hello.Runtime,
		"instances", len(hello.Instances))
	h.Sink.Hello(node, name, hello.Hello)
	go s.writer()
	if err := h.Push(r.Context(), node); err != nil {
		h.Log.Error("the spec was not made", "node", name, "err", err)
	}
	s.reader()
	h.mu.Lock()
	if h.sessions[node] == s {
		delete(h.sessions, node)
		h.mu.Unlock()
		h.Sink.Disconnected(node)
	} else {
		h.mu.Unlock()
	}
	h.Log.Info("an agent disconnected", "node", name)
}

func closeWith(conn *websocket.Conn, code, message string) {
	data, _ := agentproto.Marshal(agentproto.TClose, agentproto.Close{Code: code, Message: message})
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	conn.WriteMessage(websocket.TextMessage, data)
	conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, code),
		time.Now().Add(time.Second))
	conn.Close()
}

func (s *session) close(code, message string) {
	s.once.Do(func() {
		close(s.done)
		s.writeMu.Lock()
		closeWith(s.conn, code, message)
		s.writeMu.Unlock()
	})
}

func (s *session) write(data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return s.conn.WriteMessage(websocket.TextMessage, data)
}

func (s *session) writer() {
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-s.done:
			return
		case data := <-s.send:
			if err := s.write(data); err != nil {
				s.close("write", err.Error())
				return
			}
		case <-ping.C:
			data, _ := agentproto.Marshal(agentproto.TPing, struct{}{})
			if err := s.write(data); err != nil {
				s.close("write", err.Error())
				return
			}
		}
	}
}

func (s *session) reader() {
	defer s.close("closed", "the connection ended")
	for {
		// (an agent's heartbeat comes every 15 s: 45 s without a word is a
		// node offline)
		s.conn.SetReadDeadline(time.Now().Add(45 * time.Second))
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		envelope, err := agentproto.Kind(data)
		if err != nil {
			continue
		}
		switch envelope.Type {
		case agentproto.TReply:
			var reply agentproto.Reply
			if json.Unmarshal(data, &reply) == nil {
				if waiter, ok := s.pending.LoadAndDelete(reply.ID); ok {
					waiter.(chan agentproto.Reply) <- reply
				}
			}
		case agentproto.TPong, agentproto.TPing:
			if envelope.Type == agentproto.TPing {
				s.queue(agentproto.TPong, struct{}{})
			}
		case agentproto.TOverflow:
			return
		case agentproto.TError:
			s.h.Log.Warn("an agent's error", "node", s.name, "message", string(data))
		default:
			s.h.Sink.Report(s.node, envelope.Type, data)
		}
	}
}

func (s *session) queue(kind string, message any) error {
	data, err := agentproto.Marshal(kind, message)
	if err != nil {
		return err
	}
	select {
	case s.send <- data:
		return nil
	case <-s.done:
		return ErrOffline
	default:
		s.close("overflow", "the agent read too slowly")
		return ErrOffline
	}
}

func (h *Hub) session(node int64) *session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[node]
}

// Online tells whether a node is connected.
func (h *Hub) Online(node int64) bool { return h.session(node) != nil }

// Push sends a node its spec again (a change concerns it). Nothing is sent
// to a node that is not connected: it gets its spec when it connects.
func (h *Hub) Push(ctx context.Context, node int64) error {
	s := h.session(node)
	if s == nil {
		return nil
	}
	sp, err := h.Sink.Spec(ctx, node)
	if err != nil {
		return err
	}
	data, err := agentproto.Marshal(agentproto.TSpec, agentproto.SpecMessage{Spec: sp})
	if err != nil {
		return err
	}
	s.specMu.Lock()
	defer s.specMu.Unlock()
	// (the same spec but its generation is not sent again)
	same := s.lastSpec != nil && sameSpec(s.lastSpec, data)
	if same {
		return nil
	}
	s.lastSpec = data
	select {
	case s.send <- data:
		return nil
	case <-s.done:
		return ErrOffline
	}
}

func sameSpec(a, b []byte) bool {
	strip := func(data []byte) string {
		var m map[string]json.RawMessage
		json.Unmarshal(data, &m)
		delete(m, "generation")
		out, _ := json.Marshal(m)
		return string(out)
	}
	return strip(a) == strip(b)
}

// PushAll sends every connected node its spec.
func (h *Hub) PushAll(ctx context.Context) {
	h.mu.Lock()
	nodes := make([]int64, 0, len(h.sessions))
	for node := range h.sessions {
		nodes = append(nodes, node)
	}
	h.mu.Unlock()
	for _, node := range nodes {
		if err := h.Push(ctx, node); err != nil {
			h.Log.Warn("a spec was not sent", "node", node, "err", err)
		}
	}
}

// Send sends a message that has no reply (forget).
func (h *Hub) Send(node int64, kind string, message any) error {
	s := h.session(node)
	if s == nil {
		return ErrOffline
	}
	return s.queue(kind, message)
}

// Request sends a request (api, console, control, update_agent) and waits
// for its reply. The message's ID is set here.
func (h *Hub) Request(ctx context.Context, node int64, kind string, message any) (agentproto.Reply, error) {
	s := h.session(node)
	if s == nil {
		return agentproto.Reply{}, ErrOffline
	}
	id := strconv.FormatInt(h.ids.Add(1), 10)
	data, err := json.Marshal(message)
	if err != nil {
		return agentproto.Reply{}, err
	}
	var fields map[string]json.RawMessage
	json.Unmarshal(data, &fields)
	fields["id"], _ = json.Marshal(id)
	waiter := make(chan agentproto.Reply, 1)
	s.pending.Store(id, waiter)
	defer s.pending.Delete(id)
	if err := s.queue(kind, fields); err != nil {
		return agentproto.Reply{}, err
	}
	select {
	case reply := <-waiter:
		return reply, nil
	case <-ctx.Done():
		return agentproto.Reply{}, fmt.Errorf("the agent did not answer: %w", ctx.Err())
	case <-s.done:
		return agentproto.Reply{}, ErrOffline
	}
}

// Disconnect closes a node's connection (forgotten, or its token revoked).
func (h *Hub) Disconnect(node int64, message string) {
	if s := h.session(node); s != nil {
		s.close("revoked", message)
	}
}
