package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agent/runtime"
	"github.com/OpenCommunityEdition/opence-fleet/internal/agent/runtime/container"
	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/config"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

// fakeRuntime runs fake servers, which exit with a code on demand.
type fakeRuntime struct {
	mu      sync.Mutex
	handles []*fakeHandle
	// the next start's error
	startErr error
	// how long before now the next servers started (a long run)
	ranFor  time.Duration
	removed []string
}

func (r *fakeRuntime) Name() string                          { return "fake" }
func (r *fakeRuntime) Describe(context.Context) string       { return "fake 1" }
func (r *fakeRuntime) Ready(context.Context, string) bool    { return true }
func (r *fakeRuntime) Prune(context.Context, []string) error { return nil }
func (r *fakeRuntime) Adopt(context.Context) (map[string]runtime.Handle, []string, error) {
	return nil, nil, nil
}
func (r *fakeRuntime) Fetch(context.Context, spec.Image, spec.Release, runtime.Progress) error {
	return nil
}

func (r *fakeRuntime) Start(_ context.Context, i spec.Instance, _ spec.Node, _ runtime.Paths) (runtime.Handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.startErr; err != nil {
		r.startErr = nil
		return nil, err
	}
	h := newFakeHandle(i.Slug, i.Generation, i.Build, time.Now().Add(-r.ranFor))
	r.handles = append(r.handles, h)
	return h, nil
}

func (r *fakeRuntime) Remove(_ context.Context, slug string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, slug)
	return nil
}

// starts is how many servers were started.
func (r *fakeRuntime) starts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.handles)
}

func (r *fakeRuntime) last() *fakeHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.handles[len(r.handles)-1]
}

type fakeHandle struct {
	slug       string
	generation int64
	build      string
	started    time.Time
	once       sync.Once
	done       chan struct{}
	code       int
	stops      atomic.Int32
	// lines for its standard output
	lines chan string
}

func newFakeHandle(slug string, generation int64, build string, started time.Time) *fakeHandle {
	return &fakeHandle{slug: slug, generation: generation, build: build, started: started, done: make(chan struct{}),
		lines: make(chan string)}
}

// exit ends the server with a code (the first one stands).
func (h *fakeHandle) exit(code int) {
	h.once.Do(func() {
		h.code = code
		close(h.done)
	})
}

func (h *fakeHandle) ID() string                             { return "fake-" + h.slug }
func (h *fakeHandle) Slug() string                           { return h.slug }
func (h *fakeHandle) Generation() int64                      { return h.generation }
func (h *fakeHandle) Build() string                          { return h.build }
func (h *fakeHandle) Started() time.Time                     { return h.started }
func (h *fakeHandle) Stdin() io.Writer                       { return io.Discard }
func (h *fakeHandle) Web() string                            { return "http://127.0.0.1:1" }
func (h *fakeHandle) Detach() error                          { return nil }
func (h *fakeHandle) Signal(context.Context, string) error   { return nil }
func (h *fakeHandle) Usage(context.Context) (int64, float64) { return 0, 0 }

func (h *fakeHandle) Output(stdout, _ io.Writer) error {
	for {
		select {
		case line := <-h.lines:
			io.WriteString(stdout, line+"\n")
		case <-h.done:
			return nil
		}
	}
}

// say writes a line on the server's standard output (read by the
// supervisor's watch, as a server's are).
func (h *fakeHandle) say(line string) { h.lines <- line }

func (h *fakeHandle) Wait(context.Context) (int, error) {
	<-h.done
	return h.code, nil
}

// Stop is SIGTERM: exit 143.
func (h *fakeHandle) Stop(context.Context, time.Duration) error {
	h.stops.Add(1)
	h.exit(143)
	return nil
}

// rig is an agent with the fake runtime and one supervisor, driven by hand
// (its loop is not run: converge and onExit are called by the test).
type rig struct {
	t  *testing.T
	a  *Agent
	rt *fakeRuntime
	s  *supervisor
}

func newRig(t *testing.T) *rig {
	t.Helper()
	rt := &fakeRuntime{}
	a := New(&config.AgentConfig{Dir: t.TempDir(), Name: "test", URL: "http://127.0.0.1:1", Token: "x"}, "test",
		slog.New(slog.DiscardHandler), rt, http.DefaultClient)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.ctx = ctx
	a.state = a.loadState()
	a.builds["b1"] = ""
	return &rig{t: t, a: a, rt: rt, s: newSupervisor(a, "alpha")}
}

func (r *rig) desire(generation int64, running bool) {
	r.s.desire(spec.Instance{Slug: "alpha", Generation: generation, Build: "b1", Running: running}, spec.DefaultNode())
}

func (r *rig) converge() { r.s.converge(r.a.ctx) }

// exit ends the running server with a code and applies its exit.
func (r *rig) exit(code int) {
	r.t.Helper()
	r.rt.last().exit(code)
	r.applyExit()
}

// applyExit waits for the watch's exit and applies it.
func (r *rig) applyExit() {
	r.t.Helper()
	select {
	case e := <-r.s.exited:
		r.s.onExit(e)
	case <-time.After(5 * time.Second):
		r.t.Fatal("no exit")
	}
}

// waitFor waits (5 s at most) for what the supervisor's goroutines do.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("waited for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// stops waits for a server's stops (converge stops it in a goroutine).
func stops(t *testing.T, h *fakeHandle, n int32) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d stops", n), func() bool { return h.stops.Load() >= n })
	// (and no more)
	time.Sleep(5 * time.Millisecond)
	if got := h.stops.Load(); got != n {
		t.Fatalf("%d stops, want %d", got, n)
	}
}

type snapshot struct {
	state, reason string
	failures      int
	retryAt       time.Time
	running       bool
	code          *int
}

func (r *rig) now() snapshot {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return snapshot{state: r.s.state, reason: r.s.reason, failures: r.s.failures, retryAt: r.s.retryAt,
		running: r.s.handle != nil, code: r.s.code}
}

func (r *rig) expect(state string, starts int) snapshot {
	r.t.Helper()
	n := r.now()
	if n.state != state {
		r.t.Fatalf("state %s (%s), want %s", n.state, n.reason, state)
	}
	if got := r.rt.starts(); got != starts {
		r.t.Fatalf("%d starts, want %d", got, starts)
	}
	return n
}

// started starts the server (generation 1, running).
func (r *rig) started() {
	r.t.Helper()
	r.desire(1, true)
	r.converge()
	n := r.expect(agentproto.Starting, 1)
	if !n.running {
		r.t.Fatal("no handle")
	}
}

func TestExitFiveStartsAgainAtOnce(t *testing.T) {
	r := newRig(t)
	r.started()
	r.exit(1)
	if r.now().failures != 1 {
		t.Fatal("not a failure")
	}
	// (the backoff passed)
	r.s.mu.Lock()
	r.s.retryAt = time.Now().Add(-time.Millisecond)
	r.s.mu.Unlock()
	r.converge()
	r.expect(agentproto.Starting, 2)

	r.exit(5)
	n := r.expect(agentproto.Starting, 2)
	if n.failures != 0 || !n.retryAt.IsZero() || !strings.Contains(n.reason, "exit 5") {
		t.Fatalf("%+v", n)
	}
	r.converge()
	r.expect(agentproto.Starting, 3)
}

func TestExitZeroHoldsUntilTheNextGeneration(t *testing.T) {
	r := newRig(t)
	r.started()
	r.exit(0)
	n := r.expect(agentproto.Stopped, 1)
	if !strings.Contains(n.reason, "a quit") || n.code == nil || *n.code != 0 {
		t.Fatalf("%+v", n)
	}
	r.converge()
	r.desire(1, true)
	r.converge()
	r.expect(agentproto.Stopped, 1)

	r.desire(2, true)
	r.converge()
	r.expect(agentproto.Starting, 2)
	if r.rt.last().generation != 2 {
		t.Fatal("not the new generation")
	}
}

func TestExitZeroWhenNotDesiredRunning(t *testing.T) {
	r := newRig(t)
	r.started()
	// (the spec says stopped, but the server ended by itself first)
	r.s.mu.Lock()
	r.s.desired.Running = false
	r.s.mu.Unlock()
	r.exit(0)
	n := r.expect(agentproto.Stopped, 1)
	if n.reason != "the server stopped (exit 0)" {
		t.Fatalf("%q", n.reason)
	}
	r.converge()
	r.expect(agentproto.Stopped, 1)
}

func TestExitTwoAndThreeBlock(t *testing.T) {
	for _, c := range []struct {
		code int
		what string
	}{{2, "no usable game data (exit 2)"}, {3, "bad configuration (exit 3)"}} {
		r := newRig(t)
		r.started()
		r.rt.last().say(`{"level":"error","cat":"config","msg":"server.rotation: nowhere is not a map"}`)
		r.exit(c.code)
		n := r.expect(agentproto.Blocked, 1)
		if !strings.HasPrefix(n.reason, c.what) || !strings.Contains(n.reason, "nowhere is not a map") {
			t.Fatalf("exit %d: %q", c.code, n.reason)
		}
		if n.failures != 0 || !n.retryAt.IsZero() {
			t.Fatalf("exit %d: a backoff: %+v", c.code, n)
		}
		r.converge()
		r.desire(1, true)
		r.converge()
		r.expect(agentproto.Blocked, 1)
		// (a stop keeps it blocked)
		r.desire(1, false)
		r.converge()
		r.expect(agentproto.Blocked, 1)

		r.desire(2, true)
		r.converge()
		r.expect(agentproto.Starting, 2)
	}
}

func TestBlockedReasonWithoutALine(t *testing.T) {
	if got := blockedReason(3, []string{"server: hello"}); got != "bad configuration (exit 3); not started again until its spec changes" {
		t.Fatalf("%q", got)
	}
}

func TestCrashBackoff(t *testing.T) {
	r := newRig(t)
	r.started()
	codes := []int{1, 4, 139, 137, 1, 1}
	waits := []time.Duration{10 * time.Second, 30 * time.Second, time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	reasons := []string{"a fatal error in the game (exit 1)", "a port in use (exit 4)", "stopped by signal 11 (exit 139)",
		"killed (exit 137: SIGKILL, or out of memory)", "exit 1", "exit 1"}
	for i, code := range codes {
		before := time.Now()
		r.exit(code)
		after := time.Now()
		n := r.expect(agentproto.Crashed, i+1)
		if n.failures != i+1 {
			t.Fatalf("exit %d: %d failures, want %d", code, n.failures, i+1)
		}
		if n.retryAt.Before(before.Add(waits[i])) || n.retryAt.After(after.Add(waits[i])) {
			t.Fatalf("exit %d (failure %d): retry in %s, want %s", code, i+1, n.retryAt.Sub(after), waits[i])
		}
		if !strings.Contains(n.reason, reasons[i]) && i < 4 {
			t.Fatalf("exit %d: %q", code, n.reason)
		}
		if !strings.Contains(n.reason, "started again in "+waits[i].String()) {
			t.Fatalf("exit %d: %q", code, n.reason)
		}
		// not before its time
		r.converge()
		r.expect(agentproto.Crashed, i+1)
		// (the wait passed)
		r.s.mu.Lock()
		r.s.retryAt = time.Now().Add(-time.Millisecond)
		r.s.mu.Unlock()
		r.converge()
		r.expect(agentproto.Starting, i+2)
	}

	// a run of more than 10 minutes starts the count again
	r.rt.mu.Lock()
	r.rt.ranFor = 11 * time.Minute
	r.rt.mu.Unlock()
	r.s.mu.Lock()
	r.s.retryAt = time.Now().Add(-time.Millisecond)
	r.s.mu.Unlock()
	r.exit(1)
	r.s.mu.Lock()
	r.s.retryAt = time.Now().Add(-time.Millisecond)
	r.s.mu.Unlock()
	r.converge()
	before := time.Now()
	r.exit(1)
	n := r.now()
	if n.failures != 1 || n.retryAt.Sub(before) > 10*time.Second+time.Second {
		t.Fatalf("after a long run: %+v", n)
	}

	// ready resets the count too
	r.rt.mu.Lock()
	r.rt.ranFor = 0
	r.rt.mu.Unlock()
	r.s.mu.Lock()
	r.s.retryAt = time.Now().Add(-time.Millisecond)
	r.s.failures = 3
	r.s.mu.Unlock()
	r.converge()
	r.rt.last().say(`{"level":"info","cat":"server","msg":"ready (a test)"}`)
	waitFor(t, "up", func() bool { return r.now().state == agentproto.Up })
	r.expect(agentproto.Up, len(codes)+3)
	r.exit(1)
	if n := r.now(); n.failures != 1 {
		t.Fatalf("after ready: %d failures", n.failures)
	}
}

func TestStartFailure(t *testing.T) {
	r := newRig(t)
	r.rt.startErr = errors.New("no such image")
	r.desire(1, true)
	r.converge()
	n := r.expect(agentproto.Crashed, 0)
	if n.failures != 1 || n.code == nil || *n.code != 1 || !strings.Contains(n.reason, "no such image") {
		t.Fatalf("%+v", n)
	}
	r.s.mu.Lock()
	r.s.retryAt = time.Now().Add(-time.Millisecond)
	r.s.mu.Unlock()
	r.rt.startErr = &container.PortError{Port: "2302/udp", Err: errors.New("address already in use")}
	r.converge()
	n = r.expect(agentproto.Crashed, 0)
	if n.failures != 2 || *n.code != 4 || time.Until(n.retryAt) < 29*time.Second {
		t.Fatalf("%+v", n)
	}
}

func TestPendingUntilTheBuild(t *testing.T) {
	r := newRig(t)
	r.s.desire(spec.Instance{Slug: "alpha", Generation: 1, Build: "b2", Running: true}, spec.DefaultNode())
	r.converge()
	n := r.expect(agentproto.Pending, 0)
	if n.reason != "waiting for b2 to be fetched" {
		t.Fatalf("%q", n.reason)
	}
	r.a.mu.Lock()
	r.a.builds["b2"] = ""
	r.a.mu.Unlock()
	r.converge()
	r.expect(agentproto.Starting, 1)
}

func TestFleetStop(t *testing.T) {
	r := newRig(t)
	r.started()
	r.desire(1, false)
	r.converge()
	r.expect(agentproto.Stopping, 1)
	r.applyExit()
	n := r.expect(agentproto.Stopped, 1)
	if n.reason != "stopped by the fleet" || n.running {
		t.Fatalf("%+v", n)
	}
	if r.rt.last().stops.Load() != 1 {
		t.Fatal("not stopped once")
	}
	r.converge()
	r.expect(agentproto.Stopped, 1)
	// started by the next spec that says so
	r.desire(2, true)
	r.converge()
	r.expect(agentproto.Starting, 2)
}

func TestFleetStopWhileTheSpecSaysRunning(t *testing.T) {
	r := newRig(t)
	r.started()
	if reply := r.s.control(context.Background(), agentproto.Control{ID: "1", Action: agentproto.Stop}); !reply.OK {
		t.Fatalf("%+v", reply)
	}
	r.applyExit()
	n := r.expect(agentproto.Stopped, 1)
	if n.reason != "stopped by the fleet" {
		t.Fatalf("%q", n.reason)
	}
	// held: the spec still says running, of the same generation
	r.converge()
	r.expect(agentproto.Stopped, 1)
	// a start
	if reply := r.s.control(context.Background(), agentproto.Control{ID: "2", Action: agentproto.Start}); !reply.OK {
		t.Fatalf("%+v", reply)
	}
	r.converge()
	r.expect(agentproto.Starting, 2)
}

func TestFleetRestart(t *testing.T) {
	r := newRig(t)
	r.started()
	r.exit(1)
	r.s.mu.Lock()
	r.s.retryAt = time.Now().Add(-time.Millisecond)
	r.s.mu.Unlock()
	r.converge()
	r.expect(agentproto.Starting, 2)

	if err := r.s.restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := r.now(); n.state != agentproto.Stopping || n.reason != "restarting" {
		t.Fatalf("%+v", n)
	}
	r.applyExit()
	n := r.expect(agentproto.Starting, 2)
	if n.reason != "restarted by the fleet" || n.failures != 0 || !n.retryAt.IsZero() {
		t.Fatalf("%+v", n)
	}
	r.converge()
	r.expect(agentproto.Starting, 3)

	// a restart of a blocked server starts it
	r.exit(3)
	r.expect(agentproto.Blocked, 3)
	if err := r.s.restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.converge()
	r.expect(agentproto.Starting, 4)
}

func TestAStaleExitIsIgnored(t *testing.T) {
	r := newRig(t)
	r.started()
	old := r.rt.last()
	r.exit(5)
	r.converge()
	r.expect(agentproto.Starting, 2)
	// (the first server's exit again: not the running one's)
	r.s.onExit(exit{handle: old, code: 1})
	n := r.expect(agentproto.Starting, 2)
	if n.failures != 0 || !n.running {
		t.Fatalf("%+v", n)
	}
}

func TestAdoptedServerWaitsForItsSpec(t *testing.T) {
	r := newRig(t)
	h := newFakeHandle("alpha", 7, "b1", time.Now().Add(-time.Hour))
	r.s.adopt(h, spec.Instance{Slug: "alpha", Generation: 7, Build: "b1", WebPassword: "x"})
	if n := r.now(); n.state != agentproto.Starting || !n.running {
		t.Fatalf("%+v", n)
	}
	// no spec yet (the control plane is down): left alone
	for range 3 {
		r.converge()
	}
	time.Sleep(10 * time.Millisecond)
	if h.stops.Load() != 0 {
		t.Fatal("an adopted server was stopped before its spec was known")
	}
	if n := r.now(); n.state != agentproto.Starting || !n.running {
		t.Fatalf("%+v", n)
	}
	// its spec says running: still left alone
	r.desire(7, true)
	r.converge()
	time.Sleep(10 * time.Millisecond)
	if h.stops.Load() != 0 {
		t.Fatal("a running adopted server was stopped")
	}
	// its spec says stopped: stopped
	r.desire(7, false)
	r.converge()
	stops(t, h, 1)
	r.applyExit()
	n := r.expect(agentproto.Stopped, 0)
	if n.reason != "stopped by the fleet" {
		t.Fatalf("%q", n.reason)
	}
}

func TestAdoptedServerTheSpecStops(t *testing.T) {
	r := newRig(t)
	h := newFakeHandle("alpha", 7, "b1", time.Now())
	r.desire(7, false)
	r.s.adopt(h, spec.Instance{})
	r.converge()
	if n := r.now(); n.state != agentproto.Stopping {
		t.Fatalf("%+v", n)
	}
	stops(t, h, 1)
	// (one stop, however often it converges)
	r.converge()
	stops(t, h, 1)
	r.applyExit()
	r.expect(agentproto.Stopped, 0)
}

func TestForgottenAdoptedServerIsStopped(t *testing.T) {
	r := newRig(t)
	h := newFakeHandle("alpha", 7, "b1", time.Now())
	r.s.adopt(h, spec.Instance{})
	r.s.mu.Lock()
	r.s.forgotten = true
	r.s.mu.Unlock()
	r.converge()
	stops(t, h, 1)
	r.applyExit()
	if n := r.now(); n.state != agentproto.Stopped || n.reason != "removed" {
		t.Fatalf("%+v", n)
	}
}

// TestLoop runs the supervisor's own loop: a crash is started again once
// its backoff passes, an exit 5 at once.
func TestLoop(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(r.a.ctx)
	go r.s.loop(ctx)
	t.Cleanup(func() {
		cancel()
		<-r.s.done
	})
	r.desire(1, true)
	waitFor(t, "the start", func() bool { return r.rt.starts() == 1 })
	r.rt.last().exit(5)
	waitFor(t, "the start after exit 5", func() bool { return r.rt.starts() == 2 })
	r.rt.last().exit(1)
	waitFor(t, "the crash", func() bool { return r.now().state == agentproto.Crashed })
	if r.rt.starts() != 2 {
		t.Fatal("started again before its backoff")
	}
	r.desire(1, false)
	waitFor(t, "stopped", func() bool { return r.now().state == agentproto.Stopped })
	if n := r.now(); !n.retryAt.IsZero() {
		t.Fatalf("a stopped server keeps its retry: %+v", n)
	}
}
