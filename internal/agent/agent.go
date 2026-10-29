// Package agent is `opence-fleet agent`: the program on each machine that
// hosts servers. It connects out to the control plane over one WebSocket
// (package agentproto), keeps the node's desired state (the spec) on disk,
// and makes it so: it fetches the builds (the server image, or the release
// for the process runtime) and the library's maps, writes each instance's
// files, runs each server through a runtime, holds its standard input (the
// owner's console) and one RCON connection, forwards its status, events and
// log, relays the control plane's calls to its API, and restarts it by its
// exit code. A control plane that is down changes nothing: the agent goes
// on from its state file.
package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agent/runtime"
	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/config"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

// Agent is a node's agent.
type Agent struct {
	Config  *config.AgentConfig
	Version string
	Log     *slog.Logger
	Runtime runtime.Runtime
	// the control plane's client (its CA, the node token)
	HTTP *http.Client
	// the release key, which signs the agent's own updates
	ReleaseKey ed25519.PublicKey
	// Restart runs an installed update (exec): set by the command
	Restart func(binary string)

	mu          sync.Mutex
	state       State
	supervisors map[string]*supervisor
	// the servers run on without this agent (a self-update): its end stops
	// none of them
	detached bool
	builds   map[string]string // build → "" ready, or why not
	missing  []string
	synced   bool

	out      chan []byte
	statusMu sync.Mutex
	statuses map[string]json.RawMessage
	online   bool
	wakeSync chan struct{}
	ctx      context.Context
}

// New makes an agent.
func New(c *config.AgentConfig, version string, log *slog.Logger, rt runtime.Runtime, client *http.Client) *Agent {
	return &Agent{Config: c, Version: version, Log: log, Runtime: rt, HTTP: client,
		supervisors: map[string]*supervisor{}, builds: map[string]string{}, out: make(chan []byte, 4096),
		statuses: map[string]json.RawMessage{}, wakeSync: make(chan struct{}, 1)}
}

// HTTPClient makes the control plane's client, with extra certificate
// authorities.
func HTTPClient(caFile string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no PEM certificate", caFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: transport, Timeout: 0}, nil
}

// The node folder's parts.
func (a *Agent) dataDir() string {
	if a.Config.DataRoot != "" {
		return a.Config.DataRoot
	}
	return filepath.Join(a.Config.Dir, "data")
}
func (a *Agent) instancesDir() string { return filepath.Join(a.Config.Dir, "instances") }
func (a *Agent) releasesDir() string  { return filepath.Join(a.Config.Dir, "releases") }

func (a *Agent) paths(slug string) runtime.Paths {
	return runtime.Paths{Instance: filepath.Join(a.instancesDir(), slug), Data: a.dataDir(), Releases: a.releasesDir()}
}

// Run runs the agent until ctx ends: the servers go on from the state file,
// and the control plane is connected (again and again).
func (a *Agent) Run(ctx context.Context) error {
	a.ctx = ctx
	if a.Config.URL == "" || a.Config.Token == "" {
		return errors.New("the agent needs FLEET_AGENT_URL and FLEET_AGENT_TOKEN (enrol with `opence-fleet agent --join <url> --token <join token>`)")
	}
	for _, dir := range []string{a.Config.Dir, a.instancesDir(), filepath.Join(a.instancesDir(), ".removed")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	if a.Config.DataRoot == "" {
		if err := os.MkdirAll(a.dataDir(), 0o755); err != nil {
			return err
		}
	}
	a.state = a.loadState()
	handles, stopped, err := a.Runtime.Adopt(ctx)
	if err != nil {
		a.Log.Warn("the running servers were not adopted", "err", err)
	}
	for slug, h := range handles {
		a.Log.Info("adopted a running server", "slug", slug, "id", h.ID(), "build", h.Build())
		s := a.supervisor(slug)
		s.adopt(h, a.state.Applied[slug])
	}
	for _, slug := range stopped {
		a.Log.Info("a stopped server of a previous run", "slug", slug)
	}
	a.reconcile(a.state.Spec)
	go a.syncLoop(ctx)
	go a.janitor(ctx)
	go a.statusLoop(ctx)
	wait := time.Second
	for {
		started := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			a.shutdown()
			return nil
		}
		if time.Since(started) > time.Minute {
			wait = time.Second
		}
		a.Log.Warn("the control plane's connection ended; again in a while", "err", err, "in", wait)
		select {
		case <-ctx.Done():
			a.shutdown()
			return nil
		case <-time.After(wait):
		}
		wait = min(wait*2, 30*time.Second)
	}
}

// shutdown stops every server (the unit's stop: SIGTERM, the postgame, the
// listings withdrawn, in parallel).
func (a *Agent) shutdown() {
	a.mu.Lock()
	if a.detached {
		a.mu.Unlock()
		a.saveState()
		return
	}
	list := make([]*supervisor, 0, len(a.supervisors))
	for _, s := range a.supervisors {
		list = append(list, s)
	}
	a.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.shutdown()
		}()
	}
	wg.Wait()
	a.saveState()
}

// Detach lets the servers run on (a self-update): the containers are
// Docker's, the children are handed on.
func (a *Agent) Detach() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.detached = true
	for _, s := range a.supervisors {
		s.detach()
	}
}

func (a *Agent) websocketURL() string {
	u := a.Config.URL
	switch {
	case strings.HasPrefix(u, "https://"):
		u = "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		u = "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u + "/agent"
}

// dialer is the WebSocket's dialer, trusting what the HTTP client trusts
// but offering HTTP/1.1 alone: the upgrade is HTTP/1.1's, and the
// transport's TLS configuration (which it shares no more) offers h2 too
// once it has made a request, which a proxy that speaks HTTP/2 (Caddy by
// default) then takes, failing the upgrade.
func (a *Agent) dialer() *websocket.Dialer {
	dialer := &websocket.Dialer{HandshakeTimeout: 15 * time.Second, Subprotocols: []string{agentproto.Subprotocol},
		Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if t, ok := a.HTTP.Transport.(*http.Transport); ok && t.TLSClientConfig != nil {
		dialer.TLSClientConfig = t.TLSClientConfig.Clone()
	}
	dialer.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return dialer
}

// session is one connection to the control plane.
func (a *Agent) session(ctx context.Context) error {
	header := http.Header{"Authorization": {"Bearer " + a.Config.Token}}
	conn, response, err := a.dialer().DialContext(ctx, a.websocketURL(), header)
	if err != nil {
		if response != nil {
			return fmt.Errorf("the control plane answered %s (a forgotten node, or a token another enrolment replaced?)", response.Status)
		}
		return err
	}
	defer conn.Close()
	conn.SetReadLimit(64 << 20)
	if err := a.writeDirect(conn, agentproto.THello, a.hello(ctx)); err != nil {
		return err
	}
	a.Log.Info("connected to the control plane", "url", a.Config.URL)
	session, cancel := context.WithCancel(ctx)
	defer cancel()
	// (what waited while the connection was down: the latest of each, and
	// the events again, which the control plane drops by their seq)
	a.drainQueue()
	a.mu.Lock()
	a.online = true
	list := make([]*supervisor, 0, len(a.supervisors))
	for _, s := range a.supervisors {
		list = append(list, s)
	}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.online = false
		a.mu.Unlock()
	}()
	writeErr := make(chan error, 1)
	go func() { writeErr <- a.writer(session, conn) }()
	for _, s := range list {
		a.send(agentproto.TInstance, s.report())
		for _, e := range s.events.all() {
			e.Replay = true
			a.send(agentproto.TEvent, e)
		}
	}
	readErr := make(chan error, 1)
	go func() { readErr <- a.reader(session, conn) }()
	select {
	case err := <-readErr:
		return err
	case err := <-writeErr:
		return err
	case <-ctx.Done():
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "the agent stops"),
			time.Now().Add(time.Second))
		return ctx.Err()
	}
}

func (a *Agent) hello(ctx context.Context) agentproto.Hello {
	used, total := memory()
	_, diskTotal := disk(a.Config.Dir)
	hostname, _ := os.Hostname()
	h := agentproto.Hello{Protocol: agentproto.Protocol, Node: a.Config.Name, Version: a.Version, OS: goruntime.GOOS,
		Arch: goruntime.GOARCH, Hostname: hostname, Kernel: kernel(), CPUs: goruntime.NumCPU(), Memory: total,
		Disk: diskTotal, Runtime: a.Runtime.Name(), Docker: a.Runtime.Describe(ctx), DataRoot: "synced",
		LastSeq: map[string]int64{}}
	_ = used
	if a.Config.DataRoot != "" {
		h.DataRoot = a.Config.DataRoot
	}
	a.mu.Lock()
	for slug, s := range a.supervisors {
		h.Instances = append(h.Instances, s.instanceReport(false))
		h.LastSeq[slug] = s.lastSeq()
	}
	a.mu.Unlock()
	return h
}

func (a *Agent) writeDirect(conn *websocket.Conn, kind string, message any) error {
	data, err := agentproto.Marshal(kind, message)
	if err != nil {
		return err
	}
	conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, data)
}

// send queues a message for the control plane (dropped, but for the latest
// state, while it is not connected: the rings keep the rest).
func (a *Agent) send(kind string, message any) {
	data, err := agentproto.Marshal(kind, message)
	if err != nil {
		a.Log.Error("a message did not encode", "type", kind, "err", err)
		return
	}
	select {
	case a.out <- data:
	default:
		// (a slow connection: the log's lines go first; the rest waits)
		if kind != agentproto.TLog {
			select {
			case a.out <- data:
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (a *Agent) drainQueue() {
	for {
		select {
		case <-a.out:
		default:
			return
		}
	}
}

func (a *Agent) isOnline() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.online
}

func (a *Agent) writer(ctx context.Context, conn *websocket.Conn) error {
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	a.sendHeartbeat(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case data := <-a.out:
			conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return err
			}
		case <-heartbeat.C:
			go a.sendHeartbeat(ctx)
		}
	}
}

// statusLoop sends each instance's latest status once a second at most
// (coalesced: a slow connection gets the newest).
func (a *Agent) statusLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		a.statusMu.Lock()
		pending := a.statuses
		a.statuses = map[string]json.RawMessage{}
		a.statusMu.Unlock()
		if !a.isOnline() {
			continue
		}
		for slug, status := range pending {
			a.send(agentproto.TStatus, agentproto.Status{Slug: slug, Status: status})
		}
	}
}

func (a *Agent) queueStatus(slug string, status json.RawMessage) {
	a.statusMu.Lock()
	a.statuses[slug] = status
	a.statusMu.Unlock()
}

func (a *Agent) sendHeartbeat(ctx context.Context) {
	used, total := memory()
	diskUsed, diskTotal := disk(a.Config.Dir)
	h := agentproto.Heartbeat{Load: load(), MemoryUsed: used, MemoryTotal: total, DiskUsed: diskUsed, DiskTotal: diskTotal,
		Version: a.Version}
	a.mu.Lock()
	for build, why := range a.builds {
		if why == "" {
			h.Images = append(h.Images, build)
		}
	}
	h.LibrarySynced, h.Missing = a.synced, append([]string(nil), a.missing...)
	list := make([]*supervisor, 0, len(a.supervisors))
	for _, s := range a.supervisors {
		list = append(list, s)
	}
	a.mu.Unlock()
	sort.Strings(h.Images)
	for _, s := range list {
		h.Instances = append(h.Instances, s.instanceReport(true))
	}
	a.send(agentproto.THeartbeat, h)
}

func (a *Agent) reader(ctx context.Context, conn *websocket.Conn) error {
	for {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		envelope, err := agentproto.Kind(data)
		if err != nil {
			a.send(agentproto.TError, agentproto.Error{Code: "bad_request", Message: err.Error()})
			continue
		}
		switch envelope.Type {
		case agentproto.TSpec:
			var s spec.Spec
			if err := json.Unmarshal(data, &s); err != nil {
				a.Log.Error("a spec did not parse", "err", err)
				continue
			}
			a.Log.Info("a spec", "generation", s.Generation, "instances", len(s.Instances))
			a.reconcile(s)
		case agentproto.TAPI:
			var r agentproto.API
			if json.Unmarshal(data, &r) == nil {
				go a.handleAPI(ctx, r)
			}
		case agentproto.TConsole:
			var r agentproto.Console
			if json.Unmarshal(data, &r) == nil {
				go a.handleConsole(ctx, r)
			}
		case agentproto.TControl:
			var r agentproto.Control
			if json.Unmarshal(data, &r) == nil {
				go a.handleControl(ctx, r)
			}
		case agentproto.TUpdateAgent:
			var r agentproto.UpdateAgent
			if json.Unmarshal(data, &r) == nil {
				go a.handleUpdate(ctx, r)
			}
		case agentproto.TForget:
			var r agentproto.Forget
			if json.Unmarshal(data, &r) == nil {
				go a.forget(r.Slug)
			}
		case agentproto.TPing:
			a.send(agentproto.TPong, struct{}{})
		case agentproto.TPong, agentproto.TReply:
		case agentproto.TClose:
			var c agentproto.Close
			json.Unmarshal(data, &c)
			return fmt.Errorf("the control plane closed the connection: %s (%s)", c.Message, c.Code)
		default:
			a.send(agentproto.TError, agentproto.Error{Code: "unknown", Message: "unknown type " + envelope.Type})
		}
	}
}

func (a *Agent) reply(r agentproto.Reply) { a.send(agentproto.TReply, r) }

// supervisor returns an instance's supervisor, made (and run) if new.
func (a *Agent) supervisor(slug string) *supervisor {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.supervisors[slug]
	if s == nil {
		s = newSupervisor(a, slug)
		a.supervisors[slug] = s
		go s.loop(a.ctx)
	}
	return s
}

func (a *Agent) find(slug string) *supervisor {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.supervisors[slug]
}

// reconcile takes a new spec: each instance's supervisor gets its desired
// state, the instances no longer in it are forgotten, and the builds and
// the library are fetched.
func (a *Agent) reconcile(s spec.Spec) {
	a.mu.Lock()
	a.state.Spec = s
	wanted := map[string]bool{}
	for _, i := range s.Instances {
		wanted[i.Slug] = true
	}
	var gone []string
	for slug := range a.supervisors {
		if !wanted[slug] {
			gone = append(gone, slug)
		}
	}
	a.mu.Unlock()
	a.saveState()
	for _, i := range s.Instances {
		a.supervisor(i.Slug).desire(i, s.Node)
	}
	for _, slug := range gone {
		go a.forget(slug)
	}
	select {
	case a.wakeSync <- struct{}{}:
	default:
	}
}

// forget stops an instance, removes its server and keeps its folder aside
// for 7 days (instances/.removed/<slug>-<date>).
func (a *Agent) forget(slug string) {
	s := a.find(slug)
	if s != nil {
		s.forget()
		a.mu.Lock()
		delete(a.supervisors, slug)
		delete(a.state.Applied, slug)
		a.mu.Unlock()
	}
	_ = a.Runtime.Remove(context.Background(), slug)
	dir := filepath.Join(a.instancesDir(), slug)
	if _, err := os.Stat(dir); err == nil {
		os.Remove(filepath.Join(dir, "env"))
		target := filepath.Join(a.instancesDir(), ".removed", slug+"-"+time.Now().UTC().Format("20060102-150405"))
		if err := os.Rename(dir, target); err != nil {
			a.Log.Warn("an instance's folder was not moved aside", "slug", slug, "err", err)
		}
	}
	a.saveState()
	a.Log.Info("an instance was removed", "slug", slug)
}

// janitor deletes the removed instances' folders after 7 days, and prunes
// the builds no instance names once a day.
func (a *Agent) janitor(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
		removed := filepath.Join(a.instancesDir(), ".removed")
		entries, _ := os.ReadDir(removed)
		for _, e := range entries {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 7*24*time.Hour {
				os.RemoveAll(filepath.Join(removed, e.Name()))
			}
		}
		if time.Now().Hour() == 4 {
			a.mu.Lock()
			var keep []string
			for _, i := range a.state.Spec.Instances {
				keep = append(keep, i.Build)
			}
			for _, s := range a.supervisors {
				if b := s.runningBuild(); b != "" {
					keep = append(keep, b)
				}
			}
			a.mu.Unlock()
			if err := a.Runtime.Prune(ctx, keep); err != nil {
				a.Log.Warn("the builds were not pruned", "err", err)
			}
		}
	}
}

func (a *Agent) handleAPI(ctx context.Context, r agentproto.API) {
	s := a.find(r.Slug)
	if s == nil {
		a.reply(agentproto.Fail(r.ID, "not_found", "no instance "+r.Slug+" on this node"))
		return
	}
	a.reply(s.api(ctx, r))
}

func (a *Agent) handleConsole(ctx context.Context, r agentproto.Console) {
	s := a.find(r.Slug)
	if s == nil {
		a.reply(agentproto.Fail(r.ID, "not_found", "no instance "+r.Slug+" on this node"))
		return
	}
	lines, err := s.console(ctx, r.Line)
	if err != nil {
		reply := agentproto.Fail(r.ID, "console", err.Error())
		reply.Lines = lines
		a.reply(reply)
		return
	}
	a.reply(agentproto.Reply{ID: r.ID, OK: true, Lines: lines})
}

func (a *Agent) handleControl(ctx context.Context, r agentproto.Control) {
	s := a.find(r.Slug)
	if s == nil {
		if r.Action == agentproto.Prune {
			a.reply(agentproto.Reply{ID: r.ID, OK: true})
			return
		}
		a.reply(agentproto.Fail(r.ID, "not_found", "no instance "+r.Slug+" on this node"))
		return
	}
	a.reply(s.control(ctx, r))
}
