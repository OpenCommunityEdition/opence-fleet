package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agent/runtime"
	"github.com/OpenCommunityEdition/opence-fleet/internal/agent/runtime/container"
	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/serverapi"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

// The game's capabilities (its RCON hello) that the fleet uses when present
// (OPENCE_FLEET_PLAN.md 7.).
const (
	capabilityOwnerWeb     = "owner_web"
	capabilityBanAddress   = "ban_address"
	capabilityStop         = "stop"
	capabilityEventAddress = "event_addresses"
)

// backoff is the wait before a crashed server is started again, by its
// failures in a row (OPENCE_FLEET_PLAN.md 12.3).
var backoff = []time.Duration{10 * time.Second, 30 * time.Second, time.Minute, 5 * time.Minute}

// stopGrace is how long a server has between SIGTERM and SIGKILL.
const stopGrace = 15 * time.Second

type exit struct {
	handle runtime.Handle
	code   int
	err    error
}

// supervisor runs one instance's server.
type supervisor struct {
	a    *Agent
	slug string
	log  *slog.Logger

	mu      sync.Mutex
	desired *spec.Instance
	node    spec.Node
	applied *spec.Instance
	handle  runtime.Handle
	state   string
	since   time.Time
	code    *int
	reason  string
	tailed  []string
	// failures in a row, and when the next start may be
	failures int
	retryAt  time.Time
	// blocked (exit 2 or 3) and quit (an operator's quit) hold until the
	// spec's next generation
	blockedGen, quitGen int64
	// the fleet asked the stop; a restart follows it
	stopping, restartAfter bool
	restartNeeded          bool
	draining               string
	forgotten              bool
	// the server's run: ready seen, its RCON's hello, the last event's seq
	run          int64
	ready        bool
	caps         []string
	serverBuild  string
	seq          int64
	client       *serverapi.Client
	health       string
	runCancel    context.CancelFunc
	consoleLines chan agentproto.Log

	ring      logRing
	events    eventRing
	consoleMu sync.Mutex
	stdout    *rotatingFile
	wake      chan struct{}
	exited    chan exit
	done      chan struct{}
}

func newSupervisor(a *Agent, slug string) *supervisor {
	return &supervisor{a: a, slug: slug, log: a.Log.With("slug", slug), state: agentproto.Stopped, since: time.Now(),
		wake: make(chan struct{}, 1), exited: make(chan exit, 4), done: make(chan struct{})}
}

func (s *supervisor) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// desire takes the instance's desired state from a spec.
func (s *supervisor) desire(i spec.Instance, node spec.Node) {
	s.mu.Lock()
	s.desired = &i
	s.node = node
	s.mu.Unlock()
	s.poke()
}

// adopt takes a server a previous run of the agent left running.
func (s *supervisor) adopt(h runtime.Handle, applied spec.Instance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handle = h
	if applied.Slug == s.slug && applied.Generation == h.Generation() {
		s.applied = &applied
	} else {
		// (made from a generation the state file does not hold: the next
		// spec is applied to it as a change)
		s.applied = &spec.Instance{Slug: s.slug, Generation: h.Generation(), Build: h.Build()}
	}
	s.state, s.since, s.run = agentproto.Starting, h.Started(), h.Started().UnixNano()
	s.client = s.newClient(h)
	ctx, cancel := context.WithCancel(s.a.ctx)
	s.runCancel = cancel
	go s.watch(ctx, h)
	go s.connect(ctx, h, true)
}

func (s *supervisor) newClient(h runtime.Handle) *serverapi.Client {
	password := ""
	if s.applied != nil {
		password = s.applied.WebPassword
	}
	if password == "" && s.desired != nil {
		password = s.desired.WebPassword
	}
	return &serverapi.Client{Base: h.Web(), Password: password, By: "opence-fleet",
		HTTP: &http.Client{Timeout: 25 * time.Second}}
}

// loop is the supervisor's life: it converges the server to the desired
// state on every change, exit and timer.
func (s *supervisor) loop(ctx context.Context) {
	defer close(s.done)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		s.converge(ctx)
		s.mu.Lock()
		wait := time.Hour
		if !s.retryAt.IsZero() {
			wait = max(time.Until(s.retryAt), 100*time.Millisecond)
		}
		forgotten := s.forgotten && s.handle == nil
		s.mu.Unlock()
		if forgotten {
			return
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-timer.C:
		case e := <-s.exited:
			s.onExit(e)
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

func (s *supervisor) setState(state, reason string) {
	if s.state != state || s.reason != reason {
		s.state, s.reason, s.since = state, reason, time.Now()
	}
}

// converge does the next step toward the desired state.
func (s *supervisor) converge(ctx context.Context) {
	s.mu.Lock()
	d := s.desired
	if s.handle != nil {
		h := s.handle
		switch {
		// (an adopted server before its spec is known is left alone: one the
		// spec does not name is forgotten by the reconcile)
		case s.forgotten || d != nil && !d.Running:
			if !s.stopping {
				s.stopping = true
				s.setState(agentproto.Stopping, "")
				s.mu.Unlock()
				s.report()
				go h.Stop(context.Background(), stopGrace)
				return
			}
		case d != nil && s.ready && s.applied != nil && d.Generation != s.applied.Generation && s.draining == "":
			s.mu.Unlock()
			s.applyLive(ctx)
			return
		}
		s.mu.Unlock()
		return
	}
	if d == nil || s.forgotten {
		s.mu.Unlock()
		return
	}
	if !d.Running {
		if s.state != agentproto.Blocked {
			s.setState(agentproto.Stopped, s.reason)
		}
		s.retryAt = time.Time{}
		s.mu.Unlock()
		return
	}
	if s.blockedGen == d.Generation || s.quitGen == d.Generation {
		s.mu.Unlock()
		return
	}
	if !s.retryAt.IsZero() && time.Now().Before(s.retryAt) {
		s.mu.Unlock()
		return
	}
	s.retryAt = time.Time{}
	if why := s.a.unready(*d); why != "" {
		changed := s.state != agentproto.Pending || s.reason != why
		s.setState(agentproto.Pending, why)
		s.mu.Unlock()
		if changed {
			s.report()
		}
		return
	}
	instance, node := *d, s.node
	s.mu.Unlock()
	s.start(ctx, instance, node)
}

// files writes the instance's folder before a start: config.toml, init.txt
// and the gametype files whole from the spec, the env file (0600) and
// spec.json (without the secrets).
func (s *supervisor) files(i spec.Instance) error {
	dir := s.a.paths(s.slug).Instance
	for _, sub := range []string{"", "logs", "saves", "gametypes"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return err
		}
	}
	if err := writeFile(filepath.Join(dir, "config.toml"), i.ConfigTOML, 0o640); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, "init.txt"), i.InitTxt, 0o640); err != nil {
		return err
	}
	if err := s.gametypeFiles(i); err != nil {
		return err
	}
	keys := make([]string, 0, len(i.Env))
	for k := range i.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var env strings.Builder
	env.WriteString("# The fleet's settings of this instance (the agent passes them to the server; this file is a record).\n")
	for _, k := range keys {
		env.WriteString(k + "=" + i.Env[k] + "\n")
	}
	if err := writeFile(filepath.Join(dir, "env"), env.String(), 0o600); err != nil {
		return err
	}
	public := i
	public.WebPassword, public.OwnerPassword, public.Env = "", "", nil
	data, _ := json.MarshalIndent(public, "", "  ")
	return writeFile(filepath.Join(dir, "spec.json"), string(data)+"\n", 0o640)
}

// gametypeFiles writes the spec's gametype files and removes the .toml
// files it no longer names (a saved .gametype stays).
func (s *supervisor) gametypeFiles(i spec.Instance) error {
	dir := filepath.Join(s.a.paths(s.slug).Instance, "gametypes")
	for name, text := range i.Gametypes {
		if err := writeFile(filepath.Join(dir, name+".toml"), text, 0o640); err != nil {
			return err
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".toml")
		if _, kept := i.Gametypes[name]; ok && !kept {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	return nil
}

func writeFile(path, text string, mode os.FileMode) error {
	part := path + ".part"
	if err := os.WriteFile(part, []byte(text), mode); err != nil {
		return err
	}
	return os.Rename(part, path)
}

// start starts the server from the spec.
func (s *supervisor) start(ctx context.Context, i spec.Instance, node spec.Node) {
	s.mu.Lock()
	s.setState(agentproto.Starting, "")
	s.code = nil
	s.mu.Unlock()
	s.report()
	err := s.files(i)
	var h runtime.Handle
	if err == nil {
		h, err = s.a.Runtime.Start(ctx, i, node, s.a.paths(s.slug))
	}
	if err != nil {
		s.log.Error("the server did not start", "err", err)
		code := 1
		var port *container.PortError
		if errors.As(err, &port) {
			code = 4
		}
		s.agentLine("warn", "the server did not start: "+err.Error())
		s.onExit(exit{code: code, err: err})
		return
	}
	applied := i
	s.a.setApplied(s.slug, applied)
	s.mu.Lock()
	s.applied = &applied
	s.handle, s.run, s.ready, s.caps, s.serverBuild, s.seq, s.health = h, time.Now().UnixNano(), false, nil, "", 0, ""
	s.client = s.newClient(h)
	s.restartNeeded = false
	runCtx, cancel := context.WithCancel(s.a.ctx)
	s.runCancel = cancel
	s.mu.Unlock()
	s.a.saveState()
	s.log.Info("started", "id", h.ID(), "build", i.Build, "generation", i.Generation)
	s.agentLine("info", fmt.Sprintf("started %s (%s, generation %d)", h.ID(), i.Build, i.Generation))
	go s.watch(runCtx, h)
	go s.connect(runCtx, h, false)
}

// watch reads the server's output and waits for its exit.
func (s *supervisor) watch(ctx context.Context, h runtime.Handle) {
	stdout := &lineWriter{line: s.onLine}
	stderr := s.stderrFile()
	output := make(chan struct{})
	go func() {
		defer close(output)
		if err := h.Output(stdout, stderr); err != nil {
			s.log.Debug("the output ended", "err", err)
		}
		stdout.flush()
	}()
	code, err := h.Wait(context.Background())
	// (the last lines: the output ends with the process)
	select {
	case <-output:
	case <-time.After(2 * time.Second):
	}
	if stderr != nil {
		stderr.Close()
	}
	s.exited <- exit{handle: h, code: code, err: err}
}

// onExit applies the exit codes' policy (OPENCE_FLEET_PLAN.md 12.3).
func (s *supervisor) onExit(e exit) {
	s.mu.Lock()
	if e.handle != nil && s.handle != e.handle {
		s.mu.Unlock()
		return
	}
	stopping, restart := s.stopping, s.restartAfter
	s.stopping, s.restartAfter, s.draining = false, false, ""
	ran := time.Duration(0)
	if s.handle != nil {
		ran = time.Since(s.handle.Started())
		if s.runCancel != nil {
			s.runCancel()
		}
		if s.client != nil {
			s.client.Forget()
		}
	}
	s.handle, s.ready = nil, false
	code := e.code
	s.code = &code
	s.tailed = s.ring.tail(10)
	d := s.desired
	generation := int64(0)
	if d != nil {
		generation = d.Generation
	}
	switch {
	case s.forgotten:
		s.setState(agentproto.Stopped, "removed")
	case stopping && restart:
		s.setState(agentproto.Starting, "restarted by the fleet")
		s.failures, s.retryAt = 0, time.Time{}
	case stopping:
		s.setState(agentproto.Stopped, "stopped by the fleet")
		s.failures = 0
		if d != nil && d.Running {
			// (a stop asked while the spec says running: held until its
			// next generation)
			s.quitGen = generation
		}
	case code == 5:
		s.setState(agentproto.Starting, "the server asked a restart (exit 5)")
		s.failures, s.retryAt = 0, time.Time{}
	case code == 0:
		reason := "the server stopped (exit 0)"
		if d != nil && d.Running {
			// (an operator's quit on the server's own console: honoured
			// until the next spec)
			s.quitGen = generation
			reason = "the server stopped itself (exit 0: a quit); started again by a start or the next change"
		}
		s.setState(agentproto.Stopped, reason)
	case code == 2 || code == 3:
		s.blockedGen = generation
		s.setState(agentproto.Blocked, blockedReason(code, s.ring.tail(30)))
	default:
		if ran > 10*time.Minute {
			s.failures = 0
		}
		s.failures++
		wait := backoff[min(s.failures, len(backoff))-1]
		s.retryAt = time.Now().Add(wait)
		reason := crashReason(code, e.err)
		s.setState(agentproto.Crashed, fmt.Sprintf("%s; started again in %s (failure %d in a row)", reason, wait, s.failures))
	}
	s.mu.Unlock()
	s.log.Info("the server ended", "code", code, "state", s.state, "reason", s.reason)
	if e.err == nil {
		s.agentLine("info", fmt.Sprintf("the server ended with exit code %d: %s", code, s.reason))
	}
	if s.a.Runtime.Name() == "process" {
		// (an ended child is forgotten; a stopped container stays until the
		// next start replaces it, for `docker logs`)
		_ = s.a.Runtime.Remove(context.Background(), s.slug)
	}
	s.report()
	s.poke()
}

func crashReason(code int, err error) string {
	switch {
	case err != nil:
		return err.Error()
	case code == 1:
		return "a fatal error in the game (exit 1)"
	case code == 4:
		return "a port in use (exit 4)"
	case code == 137:
		return "killed (exit 137: SIGKILL, or out of memory)"
	case code > 128:
		return fmt.Sprintf("stopped by signal %d (exit %d)", code-128, code)
	}
	return fmt.Sprintf("exit %d", code)
}

// blockedReason is why a server stopped by its configuration or data: the
// last warning or error line of its log.
func blockedReason(code int, tail []string) string {
	what := "no usable game data (exit 2)"
	if code == 3 {
		what = "bad configuration (exit 3)"
	}
	for i := len(tail) - 1; i >= 0; i-- {
		line := tail[i]
		if strings.Contains(line, "no usable maps") || strings.Contains(line, "rotation") ||
			strings.Contains(line, "password") || strings.Contains(line, "web_") || strings.Contains(line, "missing") ||
			strings.Contains(line, "cannot") || strings.Contains(line, "not a cache file") {
			return what + ": " + line
		}
	}
	return what + "; not started again until its spec changes"
}

// onLine reads a line of the server's standard output (JSON with
// server.log_format = json).
func (s *supervisor) onLine(text string) {
	var line struct {
		Time  string `json:"time"`
		Level string `json:"level"`
		Cat   string `json:"cat"`
		Msg   string `json:"msg"`
	}
	entry := agentproto.Log{Slug: s.slug, Time: time.Now().UnixMilli(), Source: "stdout"}
	if json.Unmarshal([]byte(text), &line) == nil && line.Msg != "" {
		entry.Level, entry.Cat, entry.Text = line.Level, line.Cat, line.Msg
		if t, err := time.Parse(time.RFC3339, line.Time); err == nil {
			entry.Time = t.UnixMilli()
		}
	} else {
		entry.Level, entry.Cat, entry.Text = "info", "stdout", text
	}
	if s.stdout != nil {
		s.stdout.WriteString(text + "\n")
	}
	switch {
	case entry.Cat == "server" && strings.HasPrefix(entry.Text, "ready ("):
		s.onReady()
	case entry.Cat == "health":
		s.mu.Lock()
		s.health = entry.Text
		s.mu.Unlock()
	case strings.Contains(entry.Text, "admin password"):
		// (a password the server made: never forwarded)
		entry.Text = "(a line with a password, kept on the node)"
	}
	if entry.Cat == "console" || entry.Cat == "chat" || (entry.Cat == "net" && strings.HasPrefix(entry.Text, "invite: ")) {
		entry.Private = true
	}
	entry = s.ring.add(entry)
	s.mu.Lock()
	waiter := s.consoleLines
	s.mu.Unlock()
	if waiter != nil && entry.Cat == "console" {
		select {
		case waiter <- entry:
		default:
		}
	}
	if s.a.isOnline() {
		s.a.send(agentproto.TLog, entry)
	}
}

// agentLine is a note of the agent's in the instance's log.
func (s *supervisor) agentLine(level, text string) {
	entry := s.ring.add(agentproto.Log{Slug: s.slug, Time: time.Now().UnixMilli(), Level: level, Cat: "agent",
		Text: text, Source: "agent"})
	if s.a.isOnline() {
		s.a.send(agentproto.TLog, entry)
	}
}

func (s *supervisor) onReady() {
	s.mu.Lock()
	if s.ready {
		s.mu.Unlock()
		return
	}
	s.ready = true
	s.setState(agentproto.Up, "")
	s.failures = 0
	s.mu.Unlock()
	s.report()
	s.poke()
}

// connect signs in to the server's web port once it is up and keeps one
// RCON connection subscribed to every group, reopened with backoff, and
// polls /healthz as a second opinion.
func (s *supervisor) connect(ctx context.Context, h runtime.Handle, adopted bool) {
	wait := time.Second
	healthTicker := time.NewTicker(10 * time.Second)
	defer healthTicker.Stop()
	stalledSince := time.Time{}
	for ctx.Err() == nil {
		s.mu.Lock()
		ready, client := s.ready, s.client
		s.mu.Unlock()
		if !ready && !adopted {
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		if s.a.Runtime.Name() == "process" && s.applied != nil && s.applied.OwnerPassword != "" && client.Role() == "" {
			// (the game's owner role over the web, from loopback: 7.1; an
			// older server refuses it, and the admin's password signs in)
			owner := &serverapi.Client{Base: client.Base, Password: s.applied.OwnerPassword, By: client.By, HTTP: client.HTTP}
			if owner.Login(ctx) == nil {
				s.mu.Lock()
				s.client, client = owner, owner
				s.mu.Unlock()
			}
		}
		rcon, err := serverapi.DialRCON(ctx, client)
		if err != nil {
			if adopted && !ready {
				// (an adopted server that does not answer yet may be
				// starting: its ready line comes again only on stdout)
				if state, ok, herr := client.Healthz(ctx); herr == nil && ok && state != "" {
					s.onReady()
				}
			}
			s.log.Debug("the RCON did not open", "err", err, "in", wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = min(wait*2, 30*time.Second)
			continue
		}
		wait = time.Second
		if adopted {
			s.onReady()
			adopted = false
		}
		s.mu.Lock()
		s.caps, s.serverBuild = rcon.Hello.Capabilities, rcon.Hello.Build
		after := s.seq
		s.mu.Unlock()
		s.report()
		if err := rcon.Subscribe(serverapi.Groups, after); err != nil {
			rcon.Close()
			continue
		}
		s.poke()
		ping := time.NewTicker(30 * time.Second)
	events:
		for {
			select {
			case <-ctx.Done():
				ping.Stop()
				rcon.Close()
				return
			case <-ping.C:
				_ = rcon.Ping()
			case <-healthTicker.C:
				if state, ok, err := client.Healthz(ctx); err == nil {
					if !ok && state == "stalled" {
						if stalledSince.IsZero() {
							stalledSince = time.Now()
						}
					} else {
						stalledSince = time.Time{}
					}
				}
				if !stalledSince.IsZero() {
					s.log.Warn("the server is stalled (/healthz: no frame for a minute): restarting it")
					s.agentLine("warn", "the server stalled (no frame for a minute): restarted")
					stalledSince = time.Time{}
					s.mu.Lock()
					s.reason = "stalled"
					s.mu.Unlock()
					go s.restart(context.Background())
				}
			case m, ok := <-rcon.Events():
				if !ok {
					break events
				}
				s.onEvent(m)
			}
		}
		ping.Stop()
		s.log.Debug("the RCON closed", "err", rcon.Err())
	}
}

// onEvent forwards an RCON event: the status to the coalesced stream, the
// others (once each, by seq) to the control plane and the ring.
func (s *supervisor) onEvent(m serverapi.Message) {
	switch m.Event {
	case "status":
		var e struct {
			Status json.RawMessage `json:"status"`
		}
		if json.Unmarshal(m.Raw, &e) == nil && len(e.Status) > 0 {
			s.a.queueStatus(s.slug, e.Status)
		}
		return
	case "log":
		// (the log's lines come from standard output)
		return
	}
	s.mu.Lock()
	if m.Seq != 0 && m.Seq <= s.seq {
		s.mu.Unlock()
		return
	}
	if m.Seq > s.seq {
		s.seq = m.Seq
	}
	run := s.run
	s.mu.Unlock()
	e := agentproto.Event{Slug: s.slug, Run: run, Seq: m.Seq, Time: m.Time, Event: m.Event, Data: m.Raw, Replay: m.Replay}
	s.events.add(e)
	if s.a.isOnline() {
		s.a.send(agentproto.TEvent, e)
	}
}

// applyLive applies a new spec to the running server: the settings and the
// rotation through its API, the gametype files, config.toml rewritten (so a
// restart agrees); what only a restart changes is marked.
func (s *supervisor) applyLive(ctx context.Context) {
	s.mu.Lock()
	if s.desired == nil || s.applied == nil || s.client == nil {
		s.mu.Unlock()
		return
	}
	d, before, client := *s.desired, *s.applied, s.client
	s.mu.Unlock()
	result := agentproto.Applied{Slug: s.slug, Generation: d.Generation, Applied: []string{}, Errors: []agentproto.SettingError{}}
	after := before
	after.Settings = maps.Clone(before.Settings)
	if after.Settings == nil {
		after.Settings = map[string]any{}
	}
	call, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	changed := map[string]any{}
	for name, value := range d.Settings {
		if old, ok := before.Settings[name]; !ok || !spec.Equal(old, value) {
			changed[name] = value
		}
	}
	for name := range before.Settings {
		if _, ok := d.Settings[name]; !ok {
			if setting, ok := spec.Lookup(name); ok {
				changed[name] = setting.Default
			}
		}
	}
	if len(changed) > 0 {
		answer, err := client.PutSettings(call, changed)
		switch {
		case err != nil:
			for name := range changed {
				result.Errors = append(result.Errors, agentproto.SettingError{Name: name, Code: "failed", Message: err.Error()})
			}
		default:
			for _, name := range answer.Applied {
				result.Applied = append(result.Applied, name)
				if v, ok := d.Settings[name]; ok {
					after.Settings[name] = v
				} else {
					delete(after.Settings, name)
				}
			}
			for _, e := range answer.Errors {
				result.Errors = append(result.Errors, agentproto.SettingError{Name: e.Name, Code: e.Code, Message: e.Message})
			}
		}
	}
	gametypesChanged := false
	for name, text := range d.Gametypes {
		if before.Gametypes[name] == text {
			continue
		}
		gametypesChanged = true
		if err := client.PutGametype(call, name, text); err != nil {
			result.Errors = append(result.Errors, agentproto.SettingError{Name: "gametypes/" + name, Code: errorCode(err), Message: err.Error()})
			continue
		}
		result.Applied = append(result.Applied, "gametypes/"+name)
	}
	for name := range before.Gametypes {
		if _, ok := d.Gametypes[name]; !ok {
			gametypesChanged = true
			if err := client.DeleteGametype(call, name); err != nil {
				if e, ok := serverapi.AsError(err); !ok || e.Status != http.StatusNotFound {
					result.Errors = append(result.Errors, agentproto.SettingError{Name: "gametypes/" + name, Code: errorCode(err), Message: err.Error()})
					continue
				}
			}
			result.Applied = append(result.Applied, "gametypes/"+name)
		}
	}
	after.Gametypes = d.Gametypes
	if d.Rotation != before.Rotation || gametypesChanged {
		entries := spec.RotationEntries(d.Rotation)
		if err := client.PutRotation(call, entries); err != nil {
			message := err.Error()
			if e, ok := serverapi.AsError(err); ok && len(e.Rejected) > 0 {
				var parts []string
				for _, r := range e.Rejected {
					parts = append(parts, fmt.Sprintf("%s: %s", r.Entry, r.Reason))
				}
				message = "the rotation was refused, nothing changed: " + strings.Join(parts, "; ")
			}
			result.Errors = append(result.Errors, agentproto.SettingError{Name: "server.rotation", Code: errorCode(err), Message: message})
		} else {
			result.Applied = append(result.Applied, "server.rotation")
			after.Rotation = d.Rotation
		}
	}
	// (what the server runs now, and the file a restart reads)
	after.Generation, after.Name, after.Running, after.Needs = d.Generation, d.Name, d.Running, d.Needs
	after.ConfigTOML = spec.ConfigTOML(s.slug, after.Settings, after.Rotation)
	dir := s.a.paths(s.slug).Instance
	_ = writeFile(filepath.Join(dir, "config.toml"), d.ConfigTOML, 0o640)
	result.RestartNeeded = d.RestartKey() != before.RestartKey()
	s.a.setApplied(s.slug, after)
	s.mu.Lock()
	s.applied = &after
	s.restartNeeded = result.RestartNeeded
	s.mu.Unlock()
	s.a.saveState()
	sort.Strings(result.Applied)
	if len(result.Applied) > 0 || len(result.Errors) > 0 || result.RestartNeeded {
		s.log.Info("a spec applied live", "generation", d.Generation, "applied", result.Applied,
			"refused", len(result.Errors), "restart_needed", result.RestartNeeded)
	}
	s.a.send(agentproto.TApplied, result)
}

func errorCode(err error) string {
	if e, ok := serverapi.AsError(err); ok {
		return e.Code
	}
	return "failed"
}

// report sends the instance's state.
func (s *supervisor) report() agentproto.Instance {
	s.mu.Lock()
	r := agentproto.Instance{Slug: s.slug, State: s.state, Reason: s.reason, Since: s.since.UnixMilli(),
		ServerBuild: s.serverBuild, Capabilities: s.caps}
	if s.code != nil && s.state != agentproto.Up && s.state != agentproto.Starting {
		code := *s.code
		r.Exit = &code
		r.Tail = s.tailed
	}
	if s.state == agentproto.Up && s.draining != "" {
		r.Reason = "draining: " + s.draining
	}
	if s.applied != nil {
		r.Generation, r.Build = s.applied.Generation, s.applied.Build
	}
	s.mu.Unlock()
	s.a.send(agentproto.TInstance, r)
	return r
}

// instanceReport is the instance in a hello or a heartbeat.
func (s *supervisor) instanceReport(usage bool) agentproto.InstanceReport {
	s.mu.Lock()
	h := s.handle
	r := agentproto.InstanceReport{Slug: s.slug, State: s.state, Since: s.since.UnixMilli(), RestartNeeded: s.restartNeeded}
	if s.applied != nil {
		r.Build, r.Generation = s.applied.Build, s.applied.Generation
	}
	s.mu.Unlock()
	if h != nil {
		r.ID = h.ID()
		if usage {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			r.RSS, r.CPU = h.Usage(ctx)
			cancel()
		}
	}
	return r
}

func (s *supervisor) lastSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

func (s *supervisor) runningBuild() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle != nil {
		return s.handle.Build()
	}
	return ""
}

func (s *supervisor) current() (runtime.Handle, *serverapi.Client, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handle, s.client, s.ready
}

// api relays a request to the server's API.
func (s *supervisor) api(ctx context.Context, r agentproto.API) agentproto.Reply {
	h, client, ready := s.current()
	if h == nil || client == nil || !ready {
		return agentproto.Fail(r.ID, "not_up", "the server is not up ("+s.stateText()+")")
	}
	if !strings.HasPrefix(r.Path, "/api/") && r.Path != "/healthz" && r.Path != "/metrics" {
		return agentproto.Fail(r.ID, "bad_request", "only the server's /api/, /healthz and /metrics are relayed")
	}
	call, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var body []byte
	if len(r.Body) > 0 && string(r.Body) != "null" {
		body = r.Body
	}
	status, answer, err := client.Do(call, r.Method, r.Path, body)
	if err != nil {
		return agentproto.Fail(r.ID, "unreachable", "the server's web port did not answer: "+err.Error())
	}
	reply := agentproto.Reply{ID: r.ID, OK: status >= 200 && status < 300, Status: status}
	if json.Valid(answer) {
		reply.Body = answer
	} else {
		reply.Body, _ = json.Marshal(string(answer))
	}
	return reply
}

func (s *supervisor) stateText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason != "" {
		return s.state + ": " + s.reason
	}
	return s.state
}

var consoleSecret = regexp.MustCompile(`(?i)password`)

// console types a line on the server's standard input (the owner's role)
// and returns what it printed for it: the echo "> line", then the console's
// lines until they stop coming (the console runs one command at a time, so
// they are matched by order).
func (s *supervisor) console(ctx context.Context, line string) ([]string, error) {
	h, _, _ := s.current()
	if h == nil {
		return nil, errors.New("the server is not running")
	}
	line = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(line, "\r", " "), "\n", " "))
	if line == "" {
		return nil, errors.New("an empty line")
	}
	s.consoleMu.Lock()
	defer s.consoleMu.Unlock()
	lines := make(chan agentproto.Log, 256)
	s.mu.Lock()
	s.consoleLines = lines
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.consoleLines = nil
		s.mu.Unlock()
	}()
	if _, err := io.WriteString(h.Stdin(), line+"\n"); err != nil {
		return nil, fmt.Errorf("the server's standard input: %w", err)
	}
	echo := "> " + line
	if consoleSecret.MatchString(line) {
		echo = "> (a password's command)"
	}
	var out []string
	echoed := false
	deadline := time.After(10 * time.Second)
	quiet := time.NewTimer(time.Hour)
	defer quiet.Stop()
	for {
		select {
		case l := <-lines:
			if !echoed {
				if l.Text == echo || strings.HasPrefix(l.Text, "> ") {
					echoed = true
					quiet.Reset(300 * time.Millisecond)
				}
				continue
			}
			if strings.HasPrefix(l.Text, "> ") {
				return out, nil
			}
			out = append(out, l.Text)
			quiet.Reset(300 * time.Millisecond)
		case <-quiet.C:
			return out, nil
		case <-deadline:
			if !echoed {
				return out, errors.New("the server did not echo the line in 10 seconds (is it loading a map?)")
			}
			return out, nil
		case <-ctx.Done():
			return out, ctx.Err()
		}
	}
}

// control runs a lifecycle request.
func (s *supervisor) control(ctx context.Context, r agentproto.Control) agentproto.Reply {
	ok := agentproto.Reply{ID: r.ID, OK: true}
	switch r.Action {
	case agentproto.Start:
		s.mu.Lock()
		s.blockedGen, s.quitGen, s.retryAt, s.failures = -1, -1, time.Time{}, 0
		s.mu.Unlock()
		s.poke()
		return ok
	case agentproto.Stop:
		h, _, _ := s.current()
		if h == nil {
			return ok
		}
		s.mu.Lock()
		s.stopping = true
		s.setState(agentproto.Stopping, "")
		s.mu.Unlock()
		s.report()
		_ = h.Stop(ctx, stopGrace)
		return ok
	case agentproto.Restart:
		if err := s.restart(ctx); err != nil {
			return agentproto.Fail(r.ID, "failed", err.Error())
		}
		return ok
	case agentproto.Drain:
		go s.drain(r)
		return ok
	case agentproto.Signal:
		h, _, _ := s.current()
		if h == nil {
			return agentproto.Fail(r.ID, "not_up", "the server is not running")
		}
		if err := h.Signal(ctx, r.Signal); err != nil {
			return agentproto.Fail(r.ID, "failed", err.Error())
		}
		return ok
	case agentproto.Logs:
		lines := s.ring.after(r.AfterN, r.Limit)
		body, _ := json.Marshal(lines)
		return agentproto.Reply{ID: r.ID, OK: true, Body: body}
	case agentproto.BanFile:
		if err := s.appendBan(r.Line); err != nil {
			return agentproto.Fail(r.ID, "failed", err.Error())
		}
		return ok
	case agentproto.FileTail:
		lines, err := s.fileTail(r.File, r.Limit)
		if err != nil {
			return agentproto.Fail(r.ID, "not_found", err.Error())
		}
		return agentproto.Reply{ID: r.ID, OK: true, Lines: lines}
	}
	return agentproto.Fail(r.ID, "bad_request", "unknown action "+r.Action)
}

// restart stops the server and starts it again from the spec (the
// container made afresh).
func (s *supervisor) restart(ctx context.Context) error {
	s.mu.Lock()
	h := s.handle
	if h == nil {
		s.blockedGen, s.quitGen, s.retryAt, s.failures = -1, -1, time.Time{}, 0
		s.mu.Unlock()
		s.poke()
		return nil
	}
	s.stopping, s.restartAfter = true, true
	s.setState(agentproto.Stopping, "restarting")
	s.mu.Unlock()
	s.report()
	return h.Stop(ctx, stopGrace)
}

// drain stops (or restarts) the server once its game ends or it is empty:
// with the game's sv_stop (7.3) when it has it, else by withdrawing its
// listing (sv_public 0), saying the notice, and waiting.
func (s *supervisor) drain(r agentproto.Control) {
	h, client, ready := s.current()
	if h == nil {
		return
	}
	after := r.After
	if after != "game" && after != "empty" {
		after = "now"
	}
	deadline := time.Duration(r.Deadline) * time.Second
	if deadline <= 0 {
		deadline = 30 * time.Minute
	}
	s.mu.Lock()
	s.draining = after
	s.stopping = true
	s.restartAfter = r.Then == "restart"
	caps := s.caps
	s.mu.Unlock()
	s.report()
	s.agentLine("info", fmt.Sprintf("draining: %s once %s (deadline %s)", map[bool]string{true: "a restart", false: "a stop"}[r.Then == "restart"], after, deadline))
	ctx, cancel := context.WithTimeout(s.a.ctx, deadline+agentproto.DrainEndGrace+stopGrace+10*time.Second)
	defer cancel()
	if after == "now" || !ready || client == nil {
		_ = h.Stop(ctx, stopGrace)
		return
	}
	if has(caps, capabilityStop) {
		command := "sv_stop"
		if r.Then == "restart" {
			command = "sv_restart"
		}
		line := command + " " + after
		if notice := strings.TrimSpace(r.Notice); notice != "" {
			line += " " + strconv.Quote(notice)
		}
		if _, err := s.console(ctx, line); err == nil {
			select {
			case <-time.After(deadline):
				if after == "game" && s.endGame(ctx, h) {
					return
				}
				s.agentLine("warn", "the drain's deadline passed: stopped")
				_ = h.Stop(ctx, stopGrace)
			case <-ctx.Done():
			case <-s.handleEnded(h):
			}
			return
		}
	}
	// (the fallback: no new players from the lists, the notice, then wait)
	_, _ = client.Command(ctx, "sv_public 0")
	if notice := strings.TrimSpace(r.Notice); notice != "" {
		_, _ = client.Command(ctx, "say "+strconv.Quote(notice))
	}
	started := s.lastSeq()
	end := time.After(deadline)
	ended := false
	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()
	for {
		select {
		case <-end:
			// (the game ended, the poll below sees it and stops the server)
			if after == "game" && !ended {
				ended = true
				s.agentLine("info", "the drain's deadline passed: the game is ended (sv_end_game), then the server stops")
				_, _ = client.Command(ctx, "sv_end_game")
				end = time.After(agentproto.DrainEndGrace)
				continue
			}
			s.agentLine("warn", "the drain's deadline passed: stopped")
			_ = h.Stop(ctx, stopGrace)
			return
		case <-s.handleEnded(h):
			return
		case <-poll.C:
			status, err := client.Status(ctx)
			if err != nil {
				continue
			}
			if status.Players == 0 || (after == "game" && s.gameEndedSince(started)) {
				_ = h.Stop(ctx, stopGrace)
				return
			}
		}
	}
}

// endGame ends a drained server's game at its drain's deadline, as
// sv_end_game ends it (its scores and postgame shown, not a stop in the
// middle of it), and waits DrainEndGrace for the drain to stop the server
// after the postgame: false when it did not (no game was being played, or
// it went on), for the stop.
func (s *supervisor) endGame(ctx context.Context, h runtime.Handle) bool {
	s.agentLine("info", "the drain's deadline passed: the game is ended (sv_end_game), then the server stops")
	lines, err := s.console(ctx, "sv_end_game")
	if err != nil || strings.Contains(strings.Join(lines, " "), "no game is being played") {
		return false
	}
	select {
	case <-s.handleEnded(h):
		return true
	case <-time.After(agentproto.DrainEndGrace):
		return false
	case <-ctx.Done():
		return true
	}
}

func (s *supervisor) gameEndedSince(seq int64) bool {
	for _, e := range s.events.all() {
		if e.Seq > seq && e.Event == "game_ended" {
			return true
		}
	}
	return false
}

func (s *supervisor) handleEnded(h runtime.Handle) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			s.mu.Lock()
			current := s.handle
			s.mu.Unlock()
			if current != h {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
	return done
}

func has(list []string, item string) bool {
	for _, x := range list {
		if x == item {
			return true
		}
	}
	return false
}

// appendBan writes a fleet ban's line to the instance's bans.txt, which the
// server reads at each join (the fallback without the game's POST
// /api/bans).
func (s *supervisor) appendBan(line string) error {
	line = strings.TrimSpace(line)
	if line == "" || strings.ContainsAny(line, "\r\n") {
		return errors.New("a ban is one line")
	}
	path := filepath.Join(s.a.paths(s.slug).Instance, "logs", "bans.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	_, err = f.WriteString(line + "\n")
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// fileTail is the last lines of one of the instance's files.
func (s *supervisor) fileTail(name string, limit int) ([]string, error) {
	files := map[string]string{"debug.txt": "logs/debug.txt", "stderr.txt": "logs/stderr.txt", "bans.txt": "logs/bans.txt",
		"stdout.txt": "logs/stdout.txt", "init.txt": "init.txt", "config.toml": "config.toml", "cheaters.txt": "logs/cheaters.txt"}
	rel, ok := files[name]
	if !ok {
		return nil, fmt.Errorf("%s is not a file the fleet shows", name)
	}
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	f, err := os.Open(filepath.Join(s.a.paths(s.slug).Instance, rel))
	if err != nil {
		return nil, fmt.Errorf("%s: no such file yet", name)
	}
	defer f.Close()
	// (the last 1 MB is enough for 500 lines)
	if info, err := f.Stat(); err == nil && info.Size() > 1<<20 {
		f.Seek(info.Size()-1<<20, io.SeekStart)
	}
	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > limit {
			lines = lines[1:]
		}
	}
	return lines, nil
}

// forget stops the server for good (the instance left the spec).
func (s *supervisor) forget() {
	s.mu.Lock()
	s.forgotten = true
	h := s.handle
	s.mu.Unlock()
	if h != nil {
		s.mu.Lock()
		s.stopping = true
		s.mu.Unlock()
		_ = h.Stop(context.Background(), stopGrace)
		select {
		case <-s.done:
		case <-time.After(stopGrace + 10*time.Second):
		}
	}
	s.poke()
}

// shutdown stops the server (the agent stops: its unit's stop).
func (s *supervisor) shutdown() {
	h, _, _ := s.current()
	if h == nil {
		return
	}
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	_ = h.Stop(context.Background(), stopGrace)
}

// detach lets the server run on without the agent.
func (s *supervisor) detach() {
	if h, _, _ := s.current(); h != nil {
		_ = h.Detach()
	}
}

// stderrFile is logs/stderr.txt, rotated at 10 MB.
func (s *supervisor) stderrFile() *rotatingFile {
	dir := filepath.Join(s.a.paths(s.slug).Instance, "logs")
	os.MkdirAll(dir, 0o750)
	if s.stdout == nil {
		s.stdout = &rotatingFile{path: filepath.Join(dir, "stdout.txt"), max: 10 << 20, keep: 5}
	}
	return &rotatingFile{path: filepath.Join(dir, "stderr.txt"), max: 10 << 20, keep: 2}
}

// lineWriter splits what is written into lines.
type lineWriter struct {
	buffer bytes.Buffer
	line   func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buffer.Write(p)
	for {
		i := bytes.IndexByte(w.buffer.Bytes(), '\n')
		if i < 0 {
			if w.buffer.Len() > 64<<10 {
				w.line(w.buffer.String())
				w.buffer.Reset()
			}
			return len(p), nil
		}
		line := strings.TrimRight(string(w.buffer.Next(i+1)), "\r\n")
		if line != "" {
			w.line(line)
		}
	}
}

func (w *lineWriter) flush() {
	if w.buffer.Len() > 0 {
		w.line(strings.TrimRight(w.buffer.String(), "\r\n"))
		w.buffer.Reset()
	}
}

// rotatingFile is a log file that starts again past max bytes, keeping
// keep old ones (.1, .2...).
type rotatingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	keep int
	file *os.File
	size int64
}

func (f *rotatingFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.file == nil {
		file, err := os.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
		if err != nil {
			return len(p), nil
		}
		info, _ := file.Stat()
		f.file, f.size = file, info.Size()
	}
	if f.size+int64(len(p)) > f.max {
		f.file.Close()
		for i := f.keep - 1; i >= 1; i-- {
			os.Rename(fmt.Sprintf("%s.%d", f.path, i), fmt.Sprintf("%s.%d", f.path, i+1))
		}
		os.Rename(f.path, f.path+".1")
		file, err := os.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
		if err != nil {
			f.file = nil
			return len(p), nil
		}
		f.file, f.size = file, 0
	}
	n, err := f.file.Write(p)
	f.size += int64(n)
	return n, err
}

func (f *rotatingFile) WriteString(text string) { f.Write([]byte(text)) }

func (f *rotatingFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.file != nil {
		err := f.file.Close()
		f.file = nil
		return err
	}
	return nil
}
