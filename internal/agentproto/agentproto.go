// Package agentproto is the agents' protocol (OPENCE_FLEET_PLAN.md 4.4 and
// 12.1, docs/agent_protocol.md): one WebSocket, GET /agent with the node's
// token and the subprotocol opence-fleet-agent-1, text frames of one JSON
// object each, {"type":…}. The agent speaks first (hello); the control plane
// answers with the node's spec, and sends it whole again on every change;
// requests carry an id and get exactly one reply.
package agentproto

import (
	"encoding/json"
	"errors"

	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

// Subprotocol names the protocol's version; an agent of another is refused
// with a close naming the version to install.
const Subprotocol = "opence-fleet-agent-1"

// Protocol is the version in hello.
const Protocol = 1

// The message types.
const (
	THello       = "hello"
	TSpec        = "spec"
	THeartbeat   = "heartbeat"
	TInstance    = "instance"
	TStatus      = "status"
	TEvent       = "event"
	TLog         = "log"
	TSync        = "sync"
	TApplied     = "applied"
	TAPI         = "api"
	TConsole     = "console"
	TControl     = "control"
	TUpdateAgent = "update_agent"
	TForget      = "forget"
	TReply       = "reply"
	TPing        = "ping"
	TPong        = "pong"
	TError       = "error"
	TOverflow    = "overflow"
	TClose       = "close"
)

// The instance states an agent reports.
const (
	Starting = "starting"
	Up       = "up"
	Stopping = "stopping"
	Stopped  = "stopped"
	Crashed  = "crashed"
	// stopped by its configuration or game data (exit 2 or 3): not restarted
	Blocked = "blocked"
	// waiting for its image, release or library items
	Pending = "pending"
)

// Envelope is what every message has.
type Envelope struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

// Kind reads a message's type and id.
func Kind(data []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return e, err
	}
	if e.Type == "" {
		return e, errors.New("a message without a type")
	}
	return e, nil
}

// Marshal writes a message with its type.
func Marshal(kind string, message any) ([]byte, error) {
	data, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	fields["type"], _ = json.Marshal(kind)
	return json.Marshal(fields)
}

// InstanceReport is an instance as the agent has it, in hello and heartbeat.
type InstanceReport struct {
	Slug  string `json:"slug"`
	ID    string `json:"id,omitempty"` // the container's id, or the pid
	State string `json:"state"`
	// when the state began (milliseconds since the epoch)
	Since int64  `json:"since,omitempty"`
	Build string `json:"build,omitempty"`
	// the generation the running server was made from
	Generation int64   `json:"generation,omitempty"`
	RSS        int64   `json:"rss,omitempty"`
	CPU        float64 `json:"cpu,omitempty"`
	// a restart is needed for the spec to be whole (ports, environment)
	RestartNeeded bool `json:"restart_needed,omitempty"`
}

// Hello is the agent's first message.
type Hello struct {
	Protocol  int              `json:"protocol"`
	Node      string           `json:"node"`
	Version   string           `json:"version"`
	OS        string           `json:"os"`
	Arch      string           `json:"arch"`
	Hostname  string           `json:"hostname"`
	Kernel    string           `json:"kernel"`
	CPUs      int              `json:"cpus"`
	Memory    int64            `json:"memory"`
	Disk      int64            `json:"disk"`
	Runtime   string           `json:"runtime"`
	Docker    string           `json:"docker,omitempty"`
	DataRoot  string           `json:"data_root"` // "synced" or the external folder
	Instances []InstanceReport `json:"instances"`
	LastSeq   map[string]int64 `json:"last_seq"`
}

// SpecMessage carries a node's spec.
type SpecMessage struct {
	spec.Spec
}

// Heartbeat comes every 15 seconds.
type Heartbeat struct {
	Load        float64 `json:"load"`
	MemoryUsed  int64   `json:"memory_used"`
	MemoryTotal int64   `json:"memory_total"`
	DiskUsed    int64   `json:"disk_used"`
	DiskTotal   int64   `json:"disk_total"`
	// the share of the CPUs busy since the last heartbeat (0 to 100), and
	// the bytes the machine's interfaces received and sent since its start
	// (counters): none from an agent before them, or off Linux
	CPU           *float64         `json:"cpu,omitempty"`
	NetReceived   *uint64          `json:"net_received,omitempty"`
	NetSent       *uint64          `json:"net_sent,omitempty"`
	Version       string           `json:"version"`
	Images        []string         `json:"images"`
	LibrarySynced bool             `json:"library_synced"`
	Missing       []string         `json:"missing"`
	Instances     []InstanceReport `json:"instances"`
}

// Instance is an instance's change of state.
type Instance struct {
	Slug   string   `json:"slug"`
	State  string   `json:"state"`
	Exit   *int     `json:"exit,omitempty"`
	Signal string   `json:"signal,omitempty"`
	Reason string   `json:"reason,omitempty"`
	Tail   []string `json:"tail,omitempty"`
	// the generation and build the server runs
	Generation int64  `json:"generation,omitempty"`
	Build      string `json:"build,omitempty"`
	Since      int64  `json:"since,omitempty"`
	// the server's hello: its build and capabilities (the game's changes
	// the fleet uses when present)
	ServerBuild  string   `json:"server_build,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// Status is an instance's /api/status object, as the RCON's status event
// gave it (every 2 seconds).
type Status struct {
	Slug   string          `json:"slug"`
	Status json.RawMessage `json:"status"`
}

// Event is one of the server's events (the RCON's), whole.
type Event struct {
	Slug string `json:"slug"`
	// the server's run (its start, in nanoseconds): a seq is the run's
	Run    int64           `json:"run"`
	Seq    int64           `json:"seq"`
	Time   int64           `json:"time"`
	Event  string          `json:"event"`
	Data   json.RawMessage `json:"data"`
	Replay bool            `json:"replay,omitempty"`
}

// Log is a line of the server's log.
type Log struct {
	Slug  string `json:"slug"`
	N     int64  `json:"n"`
	Time  int64  `json:"time"`
	Level string `json:"level"`
	Cat   string `json:"cat"`
	Text  string `json:"text"`
	// stdout (the server's), stderr, or agent (the agent's own notes)
	Source string `json:"source,omitempty"`
	// a line for admins alone: the owner's console, a private invite
	Private bool `json:"private,omitempty"`
}

// Sync is the progress of a release's or a library item's download.
type Sync struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
	Error string `json:"error,omitempty"`
}

// SettingError is the server's refusal of one setting.
type SettingError struct {
	Name    string `json:"name"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Applied is the outcome of a spec's live changes to a running instance.
type Applied struct {
	Slug          string         `json:"slug"`
	Generation    int64          `json:"generation"`
	Applied       []string       `json:"applied"`
	Errors        []SettingError `json:"errors"`
	RestartNeeded bool           `json:"restart_needed"`
	Drift         []string       `json:"drift,omitempty"`
}

// API is a request to an instance's HTTP API.
type API struct {
	ID     string          `json:"id"`
	Slug   string          `json:"slug"`
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// Console is a line typed on an instance's standard input (the owner's).
type Console struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Line string `json:"line"`
}

// The control actions.
const (
	Start          = "start"
	Stop           = "stop"
	Restart        = "restart"
	Drain          = "drain"
	Signal         = "signal"
	Logs           = "logs"
	RotatePassword = "rotate_password"
	BanFile        = "ban_file"
	FileTail       = "file"
	Prune          = "prune"
)

// Control is a lifecycle request.
type Control struct {
	ID     string `json:"id"`
	Slug   string `json:"slug"`
	Action string `json:"action"`
	// drain: game, empty or now; the notice said; the deadline in seconds
	After    string `json:"after,omitempty"`
	Notice   string `json:"notice,omitempty"`
	Deadline int    `json:"deadline,omitempty"`
	// drain, then a restart rather than a stop
	Then string `json:"then,omitempty"`
	// signal: hup or usr1
	Signal string `json:"signal,omitempty"`
	// logs: the ring's lines after n, limit at most
	AfterN int64 `json:"after_n,omitempty"`
	Limit  int   `json:"limit,omitempty"`
	// rotate_password: the new password; ban_file: the line; file: the
	// file's name (debug.txt, stderr.txt, bans.txt) and its last lines
	Password string `json:"password,omitempty"`
	Line     string `json:"line,omitempty"`
	File     string `json:"file,omitempty"`
}

// UpdateAgent offers the agent a release of itself.
type UpdateAgent struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	// the release key's signature of the SHA256SUMS that names the binary,
	// and the file itself
	Sums string `json:"sums"`
	Sig  string `json:"sig"`
}

// Forget stops an instance and keeps its folder aside.
type Forget struct {
	Slug string `json:"slug"`
}

// Error is the protocol's error.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Reply answers a request.
type Reply struct {
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Status int             `json:"status,omitempty"`
	Body   json.RawMessage `json:"body,omitempty"`
	Lines  []string        `json:"lines,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Fail makes a refusal's reply.
func Fail(id, code, message string) Reply {
	return Reply{ID: id, Error: &Error{Code: code, Message: message}}
}

// Close is the last message before a close.
type Close struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
