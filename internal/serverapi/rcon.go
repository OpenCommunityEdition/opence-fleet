package serverapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// RCONSubprotocol is the RCON's WebSocket subprotocol.
const RCONSubprotocol = "opence-rcon-1"

// Groups is every event group.
var Groups = []string{"player", "chat", "game", "moderation", "status", "log", "master", "server"}

// Hello is the RCON's first message.
type Hello struct {
	Protocol       int      `json:"protocol"`
	Server         string   `json:"server"`
	Build          string   `json:"build"`
	NetworkVersion int      `json:"network_version"`
	Capabilities   []string `json:"capabilities"`
}

// Has tells whether the server has a capability.
func (h Hello) Has(capability string) bool {
	for _, c := range h.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// RCONReply is a command's reply.
type RCONReply struct {
	OK    bool     `json:"ok"`
	Lines []string `json:"lines"`
	Error *Error   `json:"error"`
}

// Message is an event as the RCON sent it: the whole object, and the
// fields every event has.
type Message struct {
	Raw    json.RawMessage
	Seq    int64
	Time   int64
	Event  string
	Replay bool
}

// RCON is a signed-in RCON connection, subscribed or not.
type RCON struct {
	Hello Hello
	Role  string

	conn    *websocket.Conn
	write   sync.Mutex
	events  chan Message
	pending sync.Map // id → chan RCONReply
	ids     atomic.Int64
	done    chan struct{}
	errMu   sync.Mutex
	err     error
	closed  sync.Once
}

// DialRCON opens the RCON on a server's web port and signs in with the
// client's session token.
func DialRCON(ctx context.Context, c *Client) (*RCON, error) {
	token, err := c.Token(ctx)
	if err != nil {
		return nil, err
	}
	address := "ws" + strings.TrimPrefix(c.Base, "http") + "/ws"
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, Subprotocols: []string{RCONSubprotocol}}
	conn, response, err := dialer.DialContext(ctx, address, http.Header{})
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("the RCON answered %s", response.Status)
		}
		return nil, err
	}
	conn.SetReadLimit(4 << 20)
	r := &RCON{conn: conn, events: make(chan Message, 1024), done: make(chan struct{})}
	deadline := time.Now().Add(10 * time.Second)
	conn.SetReadDeadline(deadline)
	var hello struct {
		Type string `json:"type"`
		Hello
	}
	if err := conn.ReadJSON(&hello); err != nil || hello.Type != "hello" {
		conn.Close()
		return nil, fmt.Errorf("the RCON said no hello: %v", err)
	}
	r.Hello = hello.Hello
	if err := conn.WriteJSON(map[string]string{"type": "auth", "token": token}); err != nil {
		conn.Close()
		return nil, err
	}
	var auth struct {
		Type  string `json:"type"`
		OK    bool   `json:"ok"`
		Role  string `json:"role"`
		Error *Error `json:"error"`
	}
	if err := conn.ReadJSON(&auth); err != nil || !auth.OK {
		conn.Close()
		if auth.Error != nil {
			c.Forget()
			return nil, auth.Error
		}
		return nil, fmt.Errorf("the RCON refused the session: %v", err)
	}
	r.Role = auth.Role
	conn.SetReadDeadline(time.Time{})
	go r.read()
	return r, nil
}

// read dispatches what the server sends: replies to their command, events
// to the channel. A reader too slow for the channel is closed, as the
// server closes one (overflow).
func (r *RCON) read() {
	defer close(r.events)
	defer r.Close()
	for {
		_, data, err := r.conn.ReadMessage()
		if err != nil {
			r.fail(err)
			return
		}
		var head struct {
			Type   string          `json:"type"`
			ID     json.RawMessage `json:"id"`
			Seq    int64           `json:"seq"`
			Time   int64           `json:"time"`
			Event  string          `json:"event"`
			Replay bool            `json:"replay"`
		}
		if json.Unmarshal(data, &head) != nil {
			continue
		}
		switch head.Type {
		case "reply", "error":
			var reply RCONReply
			if head.Type == "error" {
				var e struct {
					Error *Error `json:"error"`
				}
				json.Unmarshal(data, &e)
				reply.Error = e.Error
			} else {
				json.Unmarshal(data, &reply)
			}
			id := strings.Trim(string(head.ID), `"`)
			if waiter, ok := r.pending.LoadAndDelete(id); ok {
				waiter.(chan RCONReply) <- reply
			}
		case "event":
			select {
			case r.events <- Message{Raw: data, Seq: head.Seq, Time: head.Time, Event: head.Event, Replay: head.Replay}:
			default:
				r.fail(errors.New("the events were read too slowly"))
				return
			}
		case "overflow":
			r.fail(errors.New("the server closed the RCON: read too slowly (overflow)"))
			return
		}
	}
}

func (r *RCON) send(message any) error {
	r.write.Lock()
	defer r.write.Unlock()
	r.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return r.conn.WriteJSON(message)
}

// Subscribe asks for every event of the groups after a sequence number (0:
// the ring's events are not replayed).
func (r *RCON) Subscribe(groups []string, after int64) error {
	message := map[string]any{"type": "subscribe", "events": groups}
	if after > 0 {
		message["after"] = after
	}
	return r.send(message)
}

// Events is the events, until the connection closes.
func (r *RCON) Events() <-chan Message { return r.events }

// Command runs a command and waits for its reply (one at a time: the
// server refuses a command sent before the last one's reply).
func (r *RCON) Command(ctx context.Context, line string) (RCONReply, error) {
	id := strconv.FormatInt(r.ids.Add(1), 10)
	waiter := make(chan RCONReply, 1)
	r.pending.Store(id, waiter)
	defer r.pending.Delete(id)
	if err := r.send(map[string]any{"type": "command", "id": id, "line": line}); err != nil {
		return RCONReply{}, err
	}
	select {
	case reply := <-waiter:
		return reply, nil
	case <-ctx.Done():
		return RCONReply{}, ctx.Err()
	case <-r.done:
		return RCONReply{}, errors.New("the RCON closed")
	}
}

// Ping keeps the connection alive.
func (r *RCON) Ping() error { return r.send(map[string]string{"type": "ping"}) }

// Done is closed when the connection is.
func (r *RCON) Done() <-chan struct{} { return r.done }

// Err is why the connection closed.
func (r *RCON) Err() error {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	return r.err
}

func (r *RCON) fail(err error) {
	r.errMu.Lock()
	r.err = err
	r.errMu.Unlock()
}

// Close closes the connection.
func (r *RCON) Close() {
	r.closed.Do(func() {
		close(r.done)
		r.conn.Close()
	})
}
