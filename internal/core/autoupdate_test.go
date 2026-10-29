package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

func TestUpdateWay(t *testing.T) {
	for _, tt := range []struct {
		running, next int
		want          string
	}{
		{39, 39, wayEmpty},
		{39, 40, wayGame},
		// (an unknown network version is taken as a new one)
		{0, 39, wayGame},
		{39, 0, wayGame},
		{0, 0, wayGame},
	} {
		if got := updateWay(tt.running, tt.next); got != tt.want {
			t.Errorf("running %d, next %d: %s, want %s", tt.running, tt.next, got, tt.want)
		}
	}
}

func TestEmptyTimer(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	e := emptyTimer{wait: 10 * time.Minute}
	steps := []struct {
		at      time.Duration
		players int
		known   bool
		want    bool
	}{
		{0, 0, true, false},
		{9 * time.Minute, 0, true, false},
		// a player joins: the ten minutes start again once they leave
		{9*time.Minute + 30*time.Second, 2, true, false},
		{10 * time.Minute, 0, true, false},
		{19 * time.Minute, 0, true, false},
		{20 * time.Minute, 0, true, true},
		// a status not known starts it again too
		{21 * time.Minute, 0, false, false},
		{22 * time.Minute, 0, true, false},
		{32 * time.Minute, 0, true, true},
	}
	for _, s := range steps {
		if got := e.observe(start.Add(s.at), s.players, s.known); got != s.want {
			t.Fatalf("at %s with %d players (known %v): %v, want %v", s.at, s.players, s.known, got, s.want)
		}
	}
	if !(&emptyTimer{}).observe(start, 0, true) {
		t.Fatal("a wait of 0 is not at once")
	}
}

func TestUpdateSettings(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	// the defaults: install, every 30 minutes
	if f.UpdateMode(ctx) != UpdateInstall || f.ReleaseCheckInterval(ctx) != 30*time.Minute ||
		f.settingDuration(ctx, "update_empty_wait") != 10*time.Minute {
		t.Fatalf("%s %s", f.UpdateMode(ctx), f.ReleaseCheckInterval(ctx))
	}
	for name, value := range map[string]string{"server_update_mode": "auto", "release_check_interval": "1m",
		"update_empty_wait": "soon", "drain_deadline": "48h", "rolling_per_node": "0"} {
		if err := f.SetSetting(ctx, "user:paul", name, value); err == nil {
			t.Errorf("%s %q was taken", name, value)
		}
	}
	for name, value := range map[string]string{"server_update_mode": UpdateCheck, "release_check_interval": "2h",
		"update_empty_wait": "0s"} {
		if err := f.SetSetting(ctx, "user:paul", name, value); err != nil {
			t.Errorf("%s %q: %v", name, value, err)
		}
	}
	// (read at each look: no restart)
	if f.UpdateMode(ctx) != UpdateCheck || f.ReleaseCheckInterval(ctx) != 2*time.Hour {
		t.Fatalf("%s %s", f.UpdateMode(ctx), f.ReleaseCheckInterval(ctx))
	}
}

// FLEET_RELEASE_CHECK_INTERVAL is the release check's first value alone:
// once the database has one (the Settings page's, the upgrade's one-off),
// the variable changes nothing.
func TestReleaseCheckSeed(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	f.Config.ReleaseCheckInterval = 6 * time.Hour
	f.seedSettings(ctx)
	if got := f.Setting(ctx, "release_check_interval"); got != "6h" {
		t.Fatalf("seeded %q", got)
	}
	if err := f.SetSetting(ctx, "user:paul", "release_check_interval", "45m"); err != nil {
		t.Fatal(err)
	}
	f.Config.ReleaseCheckInterval = 3 * time.Hour
	f.seedSettings(ctx)
	if got := f.ReleaseCheckInterval(ctx); got != 45*time.Minute {
		t.Fatalf("the variable overrode the page's value: %s", got)
	}
	// (a value the upgrade wrote is kept the same way)
	g, _ := fleet(t)
	g.Store.SetSetting(ctx, db.SetSettingParams{Name: "release_check_interval", Value: "30m"})
	g.Config.ReleaseCheckInterval = 6 * time.Hour
	g.seedSettings(ctx)
	if got := g.ReleaseCheckInterval(ctx); got != 30*time.Minute {
		t.Fatalf("the variable overrode the upgrade's value: %s", got)
	}
}

// The upgrade's one-off (migration 0005, tested in package store) is told
// once: a second start tells nothing, and keeps what an admin set since.
func TestAnnounceUpdatePolicyOnce(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	f.Store.SetSetting(ctx, db.SetSettingParams{Name: "server_update_mode", Value: UpdateInstall})
	f.Store.SetSetting(ctx, db.SetSettingParams{Name: "release_check_interval", Value: "30m"})
	f.Store.SetSetting(ctx, db.SetSettingParams{Name: "update_policy_migrated", Value: "pending"})
	f.announceUpdatePolicy(ctx)
	a, ok := audited(t, f, "settings.migrate")
	if !ok || a.Actor != "auto:upgrade" || !strings.Contains(a.Detail, `"server_update_mode":"install"`) {
		t.Fatalf("%+v", a)
	}
	alert, err := f.Store.GetOpenAlert(ctx, db.GetOpenAlertParams{Rule: "update_policy", Target: Version})
	if err != nil || !strings.Contains(alert.Detail, "set to install") {
		t.Fatalf("the owners were not told: %+v %v", alert, err)
	}
	if got := f.Store.Setting(ctx, "update_policy_migrated"); got != Version {
		t.Fatalf("the marker is %q", got)
	}
	// an admin changes the policy; the next start tells nothing again
	if err := f.SetSetting(ctx, "user:paul", "server_update_mode", UpdateCheck); err != nil {
		t.Fatal(err)
	}
	f.announceUpdatePolicy(ctx)
	count := 0
	for _, row := range audit(t, f) {
		if row.Action == "settings.migrate" {
			count++
		}
	}
	if count != 1 || f.UpdateMode(ctx) != UpdateCheck {
		t.Fatalf("told %d times; the policy is %s", count, f.UpdateMode(ctx))
	}
}

// autoRig is a node whose agent restarts a server onto latest when told to
// (a restart, or a drain), and a server up on b1, whose status the test
// says.
type autoRig struct {
	t      *testing.T
	f      *Fleet
	clock  *clock
	nodeID int64
	agent  *agent
	slugs  []string
}

func newAutoRig(t *testing.T, count int) *autoRig {
	t.Helper()
	f, _ := fleet(t)
	f.updatePoll, f.updateNotice = 10*time.Millisecond, 50*time.Millisecond
	r := &autoRig{t: t, f: f, clock: stop(f, time.Unix(1_800_000_000, 0))}
	ctx := context.Background()
	for _, build := range []string{"b1", "b2", "b3"} {
		if err := f.AddLocalRelease(ctx, "test", build, "opence-server:"+build, build == "b1"); err != nil {
			t.Fatal(err)
		}
	}
	var token string
	r.nodeID, token = node(t, f, "a", spec.DefaultNode())
	r.slugs = servers(t, f, profile(t, f, "Ranked"), count, true, "a")
	r.agent = connect(t, f, token, func(q request) *agentproto.Reply {
		if q.Type == agentproto.TControl && (q.Action == agentproto.Restart || q.Action == agentproto.Drain) {
			go func() {
				time.Sleep(30 * time.Millisecond)
				latest, _ := f.ResolveChannel(context.Background(), "latest")
				r.report(q.Slug, latest, time.Now())
			}()
		}
		if q.Type == agentproto.TAPI {
			return ok(`{"ok":true}`)
		}
		return &agentproto.Reply{OK: true}
	})
	until(t, "the node online", func() bool { return f.Hub.Online(r.nodeID) })
	heartbeat, _ := json.Marshal(agentproto.Heartbeat{Images: []string{"b1", "b2", "b3"}, Version: "test"})
	until(t, "the node's builds", func() bool {
		f.Report(r.nodeID, agentproto.THeartbeat, heartbeat)
		l := f.Live.Node(r.nodeID)
		return l != nil && l.Online && len(l.Heartbeat.Images) == 3
	})
	for _, slug := range r.slugs {
		r.report(slug, "b1", time.Now().Add(-time.Minute))
		until(t, "the server up", func() bool {
			l := f.Live.Instance(slug)
			return l != nil && l.State == agentproto.Up
		})
	}
	return r
}

// report says a server is up on a build.
func (r *autoRig) report(slug, build string, since time.Time) {
	data, _ := json.Marshal(agentproto.Instance{Slug: slug, State: agentproto.Up, Build: build, Generation: 1,
		Since: since.UnixMilli()})
	r.f.Report(r.nodeID, agentproto.TInstance, data)
}

// status says a server's status: its game's state, its players and its
// network version.
func (r *autoRig) status(slug, state string, players, network int) {
	body, _ := json.Marshal(map[string]any{"ok": true, "state": state, "players": players, "network_version": network})
	data, _ := json.Marshal(agentproto.Status{Slug: slug, Status: body})
	r.f.Report(r.nodeID, agentproto.TStatus, data)
}

// release makes a build latest, with a network version (0: unknown).
func (r *autoRig) release(build string, network int) {
	ctx := context.Background()
	r.f.Store.SetReleaseNetworkVersion(ctx, db.SetReleaseNetworkVersionParams{NetworkVersion: int64(network), Build: build,
		NetworkSource: "label"})
	if err := r.f.SetLatest(ctx, "test", build); err != nil {
		r.t.Fatal(err)
	}
}

// autoJobs is the automatic updates not finished.
func (r *autoRig) autoJobs() []db.Job {
	jobs, _ := r.f.Store.ListUnfinishedJobs(context.Background())
	var out []db.Job
	for _, job := range jobs {
		if job.Kind == ActionAutoUpdate {
			out = append(out, job)
		}
	}
	return out
}

// started reconciles and finds the automatic update it starts.
func (r *autoRig) started() int64 {
	r.t.Helper()
	r.f.reconcileUpdates(context.Background())
	jobs := r.autoJobs()
	if len(jobs) != 1 {
		r.t.Fatalf("%d automatic updates", len(jobs))
	}
	if jobs[0].Actor != AutoUpdateActor {
		r.t.Fatalf("the job's actor: %q", jobs[0].Actor)
	}
	return jobs[0].ID
}

// detail waits for an item's words to say something.
func (r *autoRig) detail(id int64, slug, words string) {
	r.t.Helper()
	until(r.t, "the item to say "+words, func() bool {
		_, items, _ := r.f.Job(context.Background(), id)
		for _, it := range items {
			if it.Slug == slug && strings.Contains(it.Detail, words) {
				return true
			}
		}
		return false
	})
}

func (r *autoRig) controls(action string) []request {
	var out []request
	for _, q := range r.agent.got(agentproto.TControl) {
		if q.Action == action {
			out = append(out, q)
		}
	}
	return out
}

// A build of the same network version waits until the server has been
// empty for 10 minutes on end (a player who joins starts them again), then
// restarts it.
func TestAutoUpdateSameNetworkVersion(t *testing.T) {
	r := newAutoRig(t, 1)
	slug := r.slugs[0]
	r.status(slug, "playing", 4, 39)
	r.release("b2", 39)
	id := r.started()
	r.detail(id, slug, "waits until it has been empty for 10m (network version 39, b2's 39)")
	step := func(players int, d time.Duration) {
		r.status(slug, "lobby", players, 39)
		time.Sleep(60 * time.Millisecond)
		r.clock.add(d)
		time.Sleep(60 * time.Millisecond)
	}
	step(0, 9*time.Minute)
	step(1, 0)
	step(0, 9*time.Minute)
	if n := len(r.controls(agentproto.Restart)); n != 0 {
		t.Fatalf("restarted before 10 minutes empty on end (%d)", n)
	}
	step(0, 2*time.Minute)
	job, items := finished(t, r.f, id)
	if job.State != "done" || items[slug].State != ItemDone || items[slug].Detail != "runs b2" {
		t.Fatalf("%+v %+v", job, items[slug])
	}
	if len(r.controls(agentproto.Restart)) != 1 || len(r.controls(agentproto.Drain)) != 0 {
		t.Fatalf("restarts %d, drains %d", len(r.controls(agentproto.Restart)), len(r.controls(agentproto.Drain)))
	}
	if a, ok := audited(t, r.f, "instance.restart"); !ok || a.Actor != AutoUpdateActor {
		t.Fatalf("%+v", a)
	}
	// (its channel is untouched: it follows latest still)
	if i := instance(t, r.f, slug); i.Channel != "" {
		t.Fatalf("its channel is %q", i.Channel)
	}
}

// A build of a new network version drains a server in a game after it,
// its players told, with the deadline.
func TestAutoUpdateNewNetworkVersion(t *testing.T) {
	r := newAutoRig(t, 1)
	slug := r.slugs[0]
	r.status(slug, "playing", 8, 39)
	r.release("b2", 40)
	id := r.started()
	job, items := finished(t, r.f, id)
	if job.State != "done" || items[slug].State != ItemDone {
		t.Fatalf("%+v %+v", job, items[slug])
	}
	drains := r.controls(agentproto.Drain)
	if len(drains) != 1 || drains[0].After != "game" || drains[0].Notice != "This server updates after this game" ||
		drains[0].Deadline != 1800 {
		t.Fatalf("%+v", drains)
	}
}

// A build of an unknown network version is taken as a new one: from a
// lobby with players, a notice, then a restart.
func TestAutoUpdateUnknownNetworkVersionFromTheLobby(t *testing.T) {
	r := newAutoRig(t, 1)
	slug := r.slugs[0]
	r.status(slug, "lobby", 3, 39)
	r.release("b2", 0)
	id := r.started()
	job, items := finished(t, r.f, id)
	if job.State != "done" || items[slug].State != ItemDone {
		t.Fatalf("%+v %+v", job, items[slug])
	}
	said := false
	for _, q := range r.agent.got(agentproto.TAPI) {
		if q.Path == "/api/say" && strings.Contains(string(q.Body), "restarts to update in") {
			said = true
		}
	}
	if !said || len(r.controls(agentproto.Restart)) != 1 || len(r.controls(agentproto.Drain)) != 0 {
		t.Fatalf("said %v, restarts %d, drains %d", said, len(r.controls(agentproto.Restart)), len(r.controls(agentproto.Drain)))
	}
}

// A newer build supersedes an automatic update still waiting: cancelled,
// and one to the new build in its place (never two of a server).
func TestAutoUpdateSuperseded(t *testing.T) {
	r := newAutoRig(t, 1)
	slug := r.slugs[0]
	r.status(slug, "lobby", 2, 39)
	r.release("b2", 39)
	first := r.started()
	r.detail(first, slug, "waits until it has been empty")
	// (a second look while it waits starts nothing)
	if again := r.started(); again != first {
		t.Fatalf("a second job %d", again)
	}
	r.release("b3", 39)
	second := r.started()
	job, _, _ := r.f.Job(context.Background(), first)
	if second == first || job.State != "cancelled" || job.Detail != "superseded by b3" {
		t.Fatalf("%d %d %+v", first, second, job)
	}
	r.detail(second, slug, "b3's 39")
	if again := r.started(); again != second {
		t.Fatalf("a third job %d", again)
	}
}

// A server pinned to a build, or stopped, is not updated; the check policy
// starts nothing.
func TestAutoUpdateLeavesPinnedAndStopped(t *testing.T) {
	r := newAutoRig(t, 3)
	ctx := context.Background()
	pinned, stopped, follower := r.slugs[0], r.slugs[1], r.slugs[2]
	i, _ := r.f.Instance(ctx, pinned)
	if err := r.f.UpdateInstance(ctx, "user:paul", pinned, InstanceUpdate{Name: i.Name, ProfileID: i.ProfileID,
		Overrides: i.Overrides, Channel: "b1"}); err != nil {
		t.Fatal(err)
	}
	r.f.Store.SetInstanceRunning(ctx, db.SetInstanceRunningParams{Running: 0, Slug: stopped})
	for _, slug := range r.slugs {
		r.status(slug, "lobby", 1, 39)
	}
	r.f.SetSetting(ctx, "user:paul", "server_update_mode", UpdateCheck)
	r.release("b2", 39)
	r.f.reconcileUpdates(ctx)
	if jobs := r.autoJobs(); len(jobs) != 0 {
		t.Fatalf("the check policy started %d", len(jobs))
	}
	r.f.SetSetting(ctx, "user:paul", "server_update_mode", UpdateInstall)
	until(t, "an automatic update", func() bool { return len(r.autoJobs()) == 1 })
	_, items, _ := r.f.Job(ctx, r.autoJobs()[0].ID)
	if len(items) != 1 || items[0].Slug != follower {
		t.Fatalf("%+v", items)
	}
}

// A release whose network version is not known learns it from a server
// that runs it.
func TestNetworkVersionFromAServer(t *testing.T) {
	r := newAutoRig(t, 1)
	r.f.SetSetting(context.Background(), "user:paul", "server_update_mode", UpdateOff)
	r.status(r.slugs[0], "lobby", 0, 41)
	release, _ := r.f.Store.GetRelease(context.Background(), "b1")
	if release.NetworkVersion != 41 || release.NetworkSource != "server" {
		t.Fatalf("%+v", release)
	}
}
