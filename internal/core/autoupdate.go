package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/live"
	"github.com/OpenCommunityEdition/opence-fleet/internal/serverapi"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// The game servers' update policy (the setting server_update_mode), named
// as the game's own server.auto_update, which the fleet keeps off in its
// servers (HALO_SERVER_AUTO_UPDATE=off): the fleet updates them instead.
const (
	// latest is not looked for by itself, and a new one raises no alert
	UpdateOff = "off"
	// latest is resolved every release_check_interval and a new one raises
	// release_out: the admins update the servers (the Releases page, a job)
	UpdateCheck = "check"
	// ... and the fleet updates the servers that follow latest itself, as
	// the game's install does: a job of ActionAutoUpdate
	UpdateInstall = "install"
)

// ActionAutoUpdate is the job of an automatic update (not a bulk action a
// person starts: the fleet does, as AutoUpdateActor).
const ActionAutoUpdate = "auto_update"

// AutoUpdateActor is the actor of the automatic updates.
const AutoUpdateActor = "auto:update"

// MinimumReleaseCheck is the shortest release_check_interval.
const MinimumReleaseCheck = 5 * time.Minute

// updateAfterGameNotice is what a server's players are told when an update
// of a new network version waits for their game's end (the game's words).
const updateAfterGameNotice = "This server updates after this game"

// The moments of an automatic update (Fleet's updateNotice and updatePoll
// are these, which tests shorten), and how often the release loop looks
// at the clock.
const (
	// the notice before a restart from the lobby (the game's
	// UPDATE_LOBBY_NOTICE_SECONDS)
	updateLobbyNotice = 30 * time.Second
	// how often a waiting update looks at its server
	autoUpdatePoll = 5 * time.Second
	releaseTick    = 30 * time.Second
)

// The ways a server goes to a new build.
const (
	// the same network version: once it has been empty for
	// update_empty_wait, a restart
	wayEmpty = "empty"
	// a new (or an unknown) network version, which its players' games can
	// no longer join once the others update: after the game
	wayGame = "game"
)

// updateWay is how a server running a network version goes to a build of
// another (0: not known). An unknown one is taken as new: the cautious way,
// which never leaves a server on a version its players have left.
func updateWay(running, next int) string {
	if running > 0 && next > 0 && running == next {
		return wayEmpty
	}
	return wayGame
}

// emptyTimer is how long a server has been empty, as the fleet sees it: a
// player seen, or a status not known, starts it again.
type emptyTimer struct {
	wait  time.Duration
	since time.Time
}

// observe takes a look at the server: whether it has now been empty for
// the wait.
func (e *emptyTimer) observe(now time.Time, players int, known bool) bool {
	if !known || players > 0 {
		e.since = time.Time{}
		return false
	}
	if e.since.IsZero() {
		e.since = now
	}
	return now.Sub(e.since) >= e.wait
}

// UpdateMode is the game servers' update policy.
func (f *Fleet) UpdateMode(ctx context.Context) string {
	switch mode := f.Setting(ctx, "server_update_mode"); mode {
	case UpdateOff, UpdateCheck, UpdateInstall:
		return mode
	}
	return settingDefaults["server_update_mode"]
}

// ReleaseCheckInterval is how often latest is resolved (the setting, read
// at each look: a change needs no restart).
func (f *Fleet) ReleaseCheckInterval(ctx context.Context) time.Duration {
	return max(f.settingDuration(ctx, "release_check_interval"), MinimumReleaseCheck)
}

// seedSettings keeps FLEET_RELEASE_CHECK_INTERVAL (or its flag) as the
// release check's value when the database has none: the variable is its
// first value, and the Settings page's (or the upgrade's one-off) after.
func (f *Fleet) seedSettings(ctx context.Context) {
	if f.Config.ReleaseCheckInterval <= 0 || f.Store.Setting(ctx, "release_check_interval") != "" {
		return
	}
	value := shortDuration(f.Config.ReleaseCheckInterval)
	if err := f.Store.SetSetting(ctx, db.SetSettingParams{Name: "release_check_interval", Value: value}); err == nil {
		f.Audit(ctx, "auto", "settings.set", "release_check_interval", value+" (FLEET_RELEASE_CHECK_INTERVAL's first value)")
	}
}

// shortDuration writes a duration as a person would: 6h, 45m, 90s.
func shortDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

// announceUpdatePolicy tells, once, what the upgrade's one-off did
// (migration 0005 set the policy to install and the check to 30m, and left
// update_policy_migrated pending): an audit row, the log, an alert for the
// owners (and the webhooks) and a line of the feed. The marker then names
// this version, so that no later start tells it again.
func (f *Fleet) announceUpdatePolicy(ctx context.Context) {
	if f.Store.Setting(ctx, "update_policy_migrated") != "pending" {
		return
	}
	if err := f.Store.SetSetting(ctx, db.SetSettingParams{Name: "update_policy_migrated", Value: Version}); err != nil {
		f.Log.Error("the update policy's change was not marked told", "err", err)
		return
	}
	text := fmt.Sprintf("The game servers' update policy was set to install, and their release check to every 30m, "+
		"by the upgrade to opence-fleet %s: the servers that follow latest now update by themselves (a server of "+
		"the same network version once it has been empty for 10 minutes, one of a new version after its game). "+
		"Change it on the Settings page.", Version)
	f.Audit(ctx, "auto:upgrade", "settings.migrate", "server_update_mode", map[string]string{"server_update_mode": UpdateInstall,
		"release_check_interval": "30m", "version": Version})
	f.Log.Info("the update policy was set to install / 30m by the upgrade", "version", Version)
	f.raiseAlert(ctx, "update_policy", Version, text)
	f.Live.AddFeed(live.FeedItem{Kind: "info", Event: "update_policy", Text: "servers that follow latest now update by themselves"})
}

// followsLatest tells whether an instance follows latest (by its own
// channel, its profile's or the fleet's default): one pinned to a build is
// never moved by an update of latest.
func (f *Fleet) followsLatest(ctx context.Context, row db.Instance) bool {
	var p *Profile
	if row.ProfileID != nil {
		if profile, err := f.Profile(ctx, *row.ProfileID); err == nil {
			p = &profile
		}
	}
	channel := f.Channel(ctx, row, p)
	return channel == "latest" || channel == ""
}

// reconcileUpdates is the install policy's look (after each release check,
// and when the policy becomes install): the servers that follow latest,
// run and are up on another build get an automatic update to it, those
// already in a job's hands excepted. An automatic update of an older build
// still under way is superseded: cancelled, its servers in the new one.
func (f *Fleet) reconcileUpdates(ctx context.Context) {
	if f.UpdateMode(ctx) != UpdateInstall {
		return
	}
	f.autoMu.Lock()
	defer f.autoMu.Unlock()
	latest, err := f.Store.GetLatestRelease(ctx)
	if err != nil {
		return
	}
	build := latest.Build
	busy := map[string]bool{}
	jobs, _ := f.Store.ListUnfinishedJobs(ctx)
	for _, job := range jobs {
		var p JobParams
		json.Unmarshal([]byte(job.Params), &p)
		if job.Kind == ActionAutoUpdate && p.Channel != build {
			f.endJob(ctx, AutoUpdateActor, job.ID, "superseded by "+build)
			continue
		}
		items, _ := f.Store.ListJobItems(ctx, job.ID)
		for _, it := range items {
			if it.State == ItemWaiting || it.State == ItemRunning {
				busy[it.Slug] = true
			}
		}
	}
	rows, err := f.Store.ListInstances(ctx)
	if err != nil {
		return
	}
	var slugs []string
	for _, row := range rows {
		if row.Kind != "managed" || row.Running == 0 || busy[row.Slug] || !f.followsLatest(ctx, row) {
			continue
		}
		// (a server not up, or draining, runs latest at its next start)
		l := f.Live.Instance(row.Slug)
		if l == nil || l.State != agentproto.Up || l.Build == "" || l.Build == build || draining(l) {
			continue
		}
		slugs = append(slugs, row.Slug)
	}
	if len(slugs) == 0 {
		return
	}
	per := 0
	fmt.Sscan(f.Setting(ctx, "rolling_per_node"), &per)
	if per <= 0 {
		per = 2
	}
	p := JobParams{Action: ActionAutoUpdate, Slugs: slugs, Channel: build, PerNode: per, After: "game",
		Notice: updateAfterGameNotice, Deadline: int(f.settingDuration(ctx, "drain_deadline") / time.Second)}
	if p.Deadline <= 0 {
		p.Deadline = 1800
	}
	waves := map[string]int{}
	for _, slug := range slugs {
		waves[slug] = 0
	}
	if _, err := f.createJob(ctx, AutoUpdateActor, "Update automatically to "+build, p, waves); err != nil {
		f.Log.Error("an automatic update was not started", "build", build, "err", err)
	}
}

// draining tells whether a server is draining (its agent says so).
func draining(l *live.Instance) bool { return strings.HasPrefix(l.Reason, "draining") }

// superseded is autoOne's word for a build latest is no longer.
const superseded = "superseded"

// endJob ends a job that has not finished, saying why (CancelJob's, for the
// fleet's own reasons).
func (f *Fleet) endJob(ctx context.Context, actor string, id int64, why string) {
	if job, err := f.Store.GetJob(ctx, id); err != nil || job.State != "queued" && job.State != "running" {
		return
	}
	f.jobs.mu.Lock()
	cancel := f.jobs.cancels[id]
	f.jobs.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	f.Store.UpdateJob(ctx, db.UpdateJobParams{State: "cancelled", FinishedAt: ptrInt(f.Now().Unix()), Detail: why, ID: id})
	f.Audit(ctx, actor, "job.cancel", fmt.Sprint(id), why)
	f.Live.Notify(liveChange("job", fmt.Sprint(id)))
}

// autoUpdate runs an automatic update: the build fetched on every node
// first (so that a restart waits for nothing), then each server on its own,
// at its moment (updateWay), rolling_per_node of a node restarting at once.
// A server that does not come up on the build is put back (landed) and
// stops the servers still waiting.
func (j *jobRunner) autoUpdate(ctx context.Context, id int64, p JobParams, items []db.JobItem) {
	f := j.f
	f.fetchZipFor(ctx, p.Channel, items)
	j.prepareUpdate(ctx, id, p, items, AutoUpdateActor)
	if ctx.Err() != nil {
		return
	}
	waiting, stop := context.WithCancel(ctx)
	defer stop()
	var mu sync.Mutex
	failed := ""
	slots := map[int64]chan struct{}{}
	var wg sync.WaitGroup
	for _, it := range items {
		if it.State != ItemWaiting && it.State != ItemRunning {
			continue
		}
		row, err := f.Store.GetInstance(ctx, it.Slug)
		if err != nil {
			j.item(ctx, id, it.Slug, ItemSkipped, "removed")
			continue
		}
		slot := slots[row.NodeID]
		if slot == nil {
			per := p.PerNode
			if total, _ := f.Store.ListInstancesByNode(ctx, row.NodeID); len(total) <= 1 || per < 1 {
				per = 1
			}
			slot = make(chan struct{}, per)
			slots[row.NodeID] = slot
		}
		wg.Add(1)
		go func(slug string) {
			defer wg.Done()
			state, detail, broke := j.autoOne(ctx, waiting, id, p, slug, slot)
			if state == superseded {
				// (the job ends, and the next look starts the newer one)
				f.endJob(context.Background(), AutoUpdateActor, id, "superseded by "+detail)
				go f.reconcileUpdates(context.Background())
				return
			}
			if ctx.Err() != nil {
				// (cancelled, or superseded: the items stay as they were)
				return
			}
			if state == "" {
				mu.Lock()
				state, detail = ItemSkipped, "the job stopped: "+failed
				mu.Unlock()
			}
			if broke {
				mu.Lock()
				failed = slug + ": " + detail
				mu.Unlock()
				stop()
			}
			j.item(ctx, id, slug, state, detail)
		}(it.Slug)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	state, detail := "done", ""
	if failed != "" {
		state, detail = "failed", "stopped: "+failed
		f.raiseAlert(context.Background(), "update_failed", fmt.Sprint(id), failed)
	} else {
		all, _ := f.Store.ListJobItems(context.Background(), id)
		left := 0
		for _, it := range all {
			if it.State != ItemDone {
				left++
			}
		}
		if left > 0 {
			detail = fmt.Sprintf("%d of %d not updated (the next check tries them again)", left, len(all))
		}
	}
	f.Store.UpdateJob(context.Background(), db.UpdateJobParams{State: state, FinishedAt: ptrInt(f.Now().Unix()), Detail: detail, ID: id})
	f.Live.Notify(liveChange("job", fmt.Sprint(id)))
}

// fetchZipFor has the control plane fetch a release's zip when a node of
// the process runtime runs one of the servers (a container's node pulls
// the image by itself).
func (f *Fleet) fetchZipFor(ctx context.Context, build string, items []db.JobItem) {
	r, err := f.Store.GetRelease(ctx, build)
	if err != nil || r.ZipSha256 != "" || r.Local != 0 {
		return
	}
	for _, it := range items {
		row, err := f.Store.GetInstance(ctx, it.Slug)
		if err != nil {
			continue
		}
		if n, err := f.NodeByID(ctx, row.NodeID); err == nil && n.Settings.Runtime == "process" {
			if err := f.FetchZip(ctx, AutoUpdateActor, build); err != nil {
				f.Log.Warn("a release's zip was not fetched for an automatic update", "build", build, "err", err)
			}
			return
		}
	}
}

// autoOne takes one server to the job's build: it waits (until waiting
// ends) for its moment, then for its node's turn, restarts it, and waits
// for it on the build. Its item's state and words ("" when waiting ended
// first; superseded and latest's build when latest moved on), and whether
// it did not come up on the build.
func (j *jobRunner) autoOne(ctx, waiting context.Context, id int64, p JobParams, slug string, slot chan struct{}) (string, string, bool) {
	f := j.f
	build := p.Channel
	wait := f.settingDuration(ctx, "update_empty_wait")
	timer := emptyTimer{wait: wait}
	way, versions, said := "", "", ""
	tell := func(detail string) {
		if detail != said {
			said = detail
			j.item(ctx, id, slug, ItemRunning, detail)
		}
	}
	var l *live.Instance
	for {
		row, err := f.Store.GetInstance(ctx, slug)
		if err != nil {
			return ItemSkipped, "removed", false
		}
		if row.Running == 0 {
			return ItemSkipped, "stopped: it runs " + build + " at its next start", false
		}
		if !f.followsLatest(ctx, row) {
			return ItemSkipped, "no longer follows latest", false
		}
		if latest, _ := f.ResolveChannel(ctx, "latest"); latest != build {
			return superseded, latest, false
		}
		if !f.Hub.Online(row.NodeID) {
			return ItemOffline, "the node is offline (the next check tries it again)", false
		}
		l = f.Live.Instance(slug)
		turn := false
		switch {
		case l != nil && l.State == agentproto.Up && l.Build == build:
			return ItemSkipped, "runs " + build + " already", false
		case l == nil || l.State == agentproto.Starting || l.State == agentproto.Stopping || draining(l):
			tell("waits for the server (" + stateWords(l) + ")")
		case l.State != agentproto.Up:
			return ItemSkipped, stateWords(l) + ": it runs " + build + " at its next start", false
		default:
			if way == "" {
				running, next := f.runningNetworkVersion(ctx, l), f.releaseNetworkVersion(ctx, build)
				way = updateWay(running, next)
				versions = fmt.Sprintf("network version %s, %s's %s", versionWords(running), build, versionWords(next))
			}
			turn = true
			if way == wayEmpty {
				players, known := 0, l.Status != nil
				if known {
					players = l.Status.Players
				}
				if turn = timer.observe(f.Now(), players, known); !turn {
					tell(fmt.Sprintf("waits until it has been empty for %s (%s)", shortDuration(wait), versions))
				}
			}
		}
		if turn {
			select {
			case slot <- struct{}{}:
			default:
				turn = false
				tell("waits for its node's turn (rolling_per_node)")
			}
		}
		if turn {
			break
		}
		select {
		case <-waiting.Done():
			return "", "", false
		case <-time.After(f.updatePoll):
		}
	}
	defer func() { <-slot }()
	previous, was, before := l.Build, stateWords(l), f.since(slug)
	timeout := 5 * time.Minute
	var err error
	if way == wayEmpty {
		tell("restarts onto " + build + ": empty for " + shortDuration(wait) + " (" + versions + ")")
		err = f.Restart(ctx, AutoUpdateActor, slug)
	} else {
		timeout, err = j.afterGame(ctx, id, p, slug, versions, tell)
	}
	if err != nil {
		return ItemRefused, err.Error(), false
	}
	state := f.waitUp(ctx, slug, timeout, build, before)
	i, err := f.Instance(context.Background(), slug)
	if err != nil {
		return ItemSkipped, "removed", false
	}
	outcome, detail := j.landed(ctx, i, build, previous, true, was, state)
	return outcome, detail, state != "up" && ctx.Err() == nil
}

// afterGame restarts a server onto a build of a new (or an unknown) network
// version as the game's install does: at once when nobody plays; from the
// lobby after a notice of updateLobbyNotice; in a game, at its end (a drain
// after it, its players told so), the game ended with its scores at the
// drain's deadline. How long to wait for it to come up.
func (j *jobRunner) afterGame(ctx context.Context, id int64, p JobParams, slug, versions string, tell func(string)) (time.Duration, error) {
	f := j.f
	deadline := time.Duration(p.Deadline) * time.Second
	drain := func() (time.Duration, error) {
		tell("restarts onto " + p.Channel + " after its game, ended at the latest in " + shortDuration(deadline) +
			" (" + versions + ")")
		return deadline + agentproto.DrainEndGrace + 5*time.Minute,
			f.Drain(ctx, AutoUpdateActor, slug, "game", p.Notice, deadline, true)
	}
	status := func() *serverapi.Status {
		if l := f.Live.Instance(slug); l != nil {
			return l.Status
		}
		return nil
	}
	st := status()
	if st == nil || inGame(st) {
		return drain()
	}
	if st.Players > 0 {
		notice := fmt.Sprintf("This server restarts to update in %d seconds", int(f.updateNotice.Round(time.Second)/time.Second))
		if err := f.Call(ctx, slug, http.MethodPost, "/api/say", map[string]string{"text": notice}, nil); err != nil {
			f.Log.Info("a server's update notice was not said", "slug", slug, "err", err)
		}
		tell("restarts onto " + p.Channel + " from the lobby, after a notice (" + versions + ")")
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(f.updateNotice):
		}
		// (a game that began meanwhile is waited for)
		if st := status(); st == nil || inGame(st) {
			return drain()
		}
	} else {
		tell("restarts onto " + p.Channel + ": nobody plays (" + versions + ")")
	}
	return 5 * time.Minute, f.Restart(ctx, AutoUpdateActor, slug)
}

// inGame tells whether a server's game is being played (or loads, or shows
// its postgame).
func inGame(st *serverapi.Status) bool {
	return st.State == "loading" || st.State == "playing" || st.State == "postgame"
}

func versionWords(n int) string {
	if n <= 0 {
		return "unknown"
	}
	return fmt.Sprint(n)
}
