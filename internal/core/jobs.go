package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/live"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	h := sha256.New()
	n, err := io.Copy(h, file)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// The bulk actions.
const (
	ActionStart         = "start"
	ActionStop          = "stop"
	ActionRestart       = "restart"
	ActionDrainRestart  = "drain_restart"
	ActionDrainStop     = "drain_stop"
	ActionSay           = "say"
	ActionNext          = "next"
	ActionEndGame       = "end_game"
	ActionSetRotation   = "set_rotation"
	ActionChangeProfile = "change_profile"
	ActionUpdate        = "update"
	ActionAddGroup      = "add_group"
	ActionRemoveGroup   = "remove_group"
	ActionDelete        = "delete"
	ActionCommand       = "command"
)

// Actions is every bulk action, with its words.
var Actions = []struct{ ID, Name string }{
	{ActionStart, "Start"}, {ActionStop, "Stop"}, {ActionRestart, "Restart"},
	{ActionDrainRestart, "Drain and restart (rolling)"}, {ActionDrainStop, "Drain and stop"},
	{ActionSay, "Say"}, {ActionNext, "Next map"}, {ActionEndGame, "End the game"},
	{ActionSetRotation, "Set the rotation"}, {ActionChangeProfile, "Change the profile"},
	{ActionUpdate, "Update to a release (rolling)"}, {ActionAddGroup, "Add to a group"},
	{ActionRemoveGroup, "Remove from a group"}, {ActionDelete, "Delete"}, {ActionCommand, "Run a command"},
}

// rolling tells whether an action goes in waves.
func rolling(action string) bool {
	return action == ActionDrainRestart || action == ActionUpdate || action == ActionRestart
}

// JobParams is a job's parameters.
type JobParams struct {
	Action string   `json:"action"`
	Slugs  []string `json:"slugs"`
	// say's text, the rotation, a group, a profile's id, a channel, a command
	Text      string `json:"text,omitempty"`
	ProfileID int64  `json:"profile_id,omitempty"`
	Channel   string `json:"channel,omitempty"`
	// rolling: per node at once; drain: after (game, empty, now), notice,
	// deadline in seconds
	PerNode  int    `json:"per_node,omitempty"`
	After    string `json:"after,omitempty"`
	Notice   string `json:"notice,omitempty"`
	Deadline int    `json:"deadline,omitempty"`
	// the role of the one who asked (a command's)
	Role string `json:"role,omitempty"`
}

// The item states.
const (
	ItemWaiting = "waiting"
	ItemRunning = "running"
	ItemDone    = "done"
	ItemRefused = "refused"
	ItemOffline = "offline"
	ItemSkipped = "skipped"
)

type jobRunner struct {
	f       *Fleet
	mu      sync.Mutex
	cancels map[int64]context.CancelFunc
	waiters map[string][]chan agentproto.Applied
}

func newJobRunner(f *Fleet) *jobRunner {
	return &jobRunner{f: f, cancels: map[int64]context.CancelFunc{}, waiters: map[string][]chan agentproto.Applied{}}
}

func (j *jobRunner) applied(a agentproto.Applied) {
	j.mu.Lock()
	list := j.waiters[a.Slug]
	delete(j.waiters, a.Slug)
	j.mu.Unlock()
	for _, ch := range list {
		select {
		case ch <- a:
		default:
		}
	}
}

// waitApplied waits for an instance's agent to apply a generation.
func (j *jobRunner) waitApplied(ctx context.Context, slug string, generation int64, timeout time.Duration) (*agentproto.Applied, bool) {
	deadline := time.After(timeout)
	for {
		ch := make(chan agentproto.Applied, 1)
		j.mu.Lock()
		j.waiters[slug] = append(j.waiters[slug], ch)
		j.mu.Unlock()
		select {
		case a := <-ch:
			if a.Generation >= generation {
				return &a, true
			}
		case <-deadline:
			return nil, false
		case <-ctx.Done():
			return nil, false
		}
	}
}

// StartJob makes a job of a bulk action and runs it.
func (f *Fleet) StartJob(ctx context.Context, actor string, p JobParams) (int64, error) {
	if len(p.Slugs) == 0 {
		return 0, Refuse(http.StatusBadRequest, "schema", "choose servers")
	}
	valid := false
	title := p.Action
	for _, a := range Actions {
		if a.ID == p.Action {
			valid, title = true, a.Name
		}
	}
	if !valid {
		return 0, Refuse(http.StatusBadRequest, "schema", "%q is not a bulk action", p.Action)
	}
	switch p.Action {
	case ActionSay, ActionCommand:
		if strings.TrimSpace(p.Text) == "" {
			return 0, Refuse(http.StatusBadRequest, "schema", "the text is empty")
		}
	case ActionSetRotation:
		if len(spec.RotationEntries(p.Text)) == 0 {
			return 0, Refuse(http.StatusBadRequest, "schema", "the rotation has no entry")
		}
	case ActionChangeProfile:
		if _, err := f.Profile(ctx, p.ProfileID); err != nil {
			return 0, err
		}
	case ActionUpdate:
		if p.Channel == "" {
			p.Channel = "latest"
		}
		if _, err := f.ResolveChannel(ctx, p.Channel); err != nil {
			return 0, err
		}
	case ActionAddGroup, ActionRemoveGroup:
		if groupName(p.Text) == "" {
			return 0, Refuse(http.StatusBadRequest, "schema", "a group's name")
		}
	}
	if p.PerNode <= 0 {
		p.PerNode, _ = strconv.Atoi(f.Setting(ctx, "rolling_per_node"))
		if p.PerNode <= 0 {
			p.PerNode = 2
		}
	}
	if p.After == "" {
		p.After = "game"
	}
	if p.Deadline <= 0 {
		d, err := time.ParseDuration(f.Setting(ctx, "drain_deadline"))
		if err != nil {
			d = 30 * time.Minute
		}
		p.Deadline = int(d / time.Second)
	}
	instances := map[string]db.Instance{}
	for _, slug := range p.Slugs {
		row, err := f.Store.GetInstance(ctx, slug)
		if err != nil {
			return 0, NotFound("server " + slug)
		}
		instances[slug] = row
	}
	waves := f.waves(p, instances)
	if p.Action == ActionSay || p.Action == ActionCommand || p.Action == ActionSetRotation {
		title += ": " + truncate(p.Text, 60)
	}
	if p.Action == ActionUpdate {
		title += " " + p.Channel
	}
	return f.createJob(ctx, actor, title, p, waves)
}

// createJob keeps a job and its items (each in its wave), audits it and
// runs it.
func (f *Fleet) createJob(ctx context.Context, actor, title string, p JobParams, waves map[string]int) (int64, error) {
	data, _ := json.Marshal(p)
	var id int64
	err := f.Store.Tx(ctx, func(q *db.Queries) error {
		var err error
		id, err = q.CreateJob(ctx, db.CreateJobParams{Kind: p.Action, Title: title, Params: string(data), State: "queued",
			Actor: actor, CreatedAt: f.Now().Unix()})
		if err != nil {
			return err
		}
		for slug, wave := range waves {
			if err := q.CreateJobItem(ctx, db.CreateJobItemParams{JobID: id, Slug: slug, Wave: int64(wave), State: ItemWaiting,
				UpdatedAt: f.Now().Unix()}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	f.Audit(ctx, actor, "job.start", strconv.FormatInt(id, 10), map[string]any{"action": p.Action, "servers": p.Slugs})
	f.jobs.run(id)
	return id, nil
}

func truncate(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[:n] + "…"
}

// waves assigns each instance its wave: a rolling action takes per_node of
// each node at a time (one at a time on a node with one server); the others
// all at once.
func (f *Fleet) waves(p JobParams, instances map[string]db.Instance) map[string]int {
	out := map[string]int{}
	if !rolling(p.Action) {
		for slug := range instances {
			out[slug] = 0
		}
		return out
	}
	byNode := map[int64][]string{}
	for slug, i := range instances {
		byNode[i.NodeID] = append(byNode[i.NodeID], slug)
	}
	for node, slugs := range byNode {
		sort.Strings(slugs)
		per := p.PerNode
		if total, _ := f.Store.ListInstancesByNode(context.Background(), node); len(total) <= 1 {
			per = 1
		}
		for i, slug := range slugs {
			out[slug] = i / per
		}
	}
	return out
}

func (j *jobRunner) run(id int64) {
	ctx, cancel := context.WithCancel(context.Background())
	j.mu.Lock()
	j.cancels[id] = cancel
	j.mu.Unlock()
	go func() {
		defer func() {
			j.mu.Lock()
			delete(j.cancels, id)
			j.mu.Unlock()
			cancel()
		}()
		j.execute(ctx, id)
	}()
}

// resume goes on with the jobs a stop of the control plane left running.
func (j *jobRunner) resume(ctx context.Context) {
	jobs, err := j.f.Store.ListUnfinishedJobs(ctx)
	if err != nil {
		return
	}
	for _, job := range jobs {
		j.f.Log.Info("a job goes on", "job", job.ID, "kind", job.Kind)
		j.run(job.ID)
	}
}

// CancelJob stops a job between its waves (what runs finishes).
func (f *Fleet) CancelJob(ctx context.Context, actor string, id int64) error {
	job, err := f.Store.GetJob(ctx, id)
	if err != nil {
		return NotFound("the job")
	}
	if job.State != "queued" && job.State != "running" {
		return Refuse(http.StatusConflict, "finished", "the job is %s", job.State)
	}
	f.jobs.mu.Lock()
	cancel := f.jobs.cancels[id]
	f.jobs.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	f.Store.UpdateJob(ctx, db.UpdateJobParams{State: "cancelled", FinishedAt: ptrInt(f.Now().Unix()), Detail: "cancelled by " + actor, ID: id})
	f.Audit(ctx, actor, "job.cancel", strconv.FormatInt(id, 10), nil)
	f.Live.Notify(liveChange("job", strconv.FormatInt(id, 10)))
	return nil
}

func (j *jobRunner) item(ctx context.Context, job int64, slug, state, detail string) {
	j.f.Store.UpdateJobItem(context.Background(), db.UpdateJobItemParams{State: state, Detail: detail, UpdatedAt: j.f.Now().Unix(),
		JobID: job, Slug: slug})
	j.f.Live.Notify(liveChange("job", strconv.FormatInt(job, 10)))
}

func (j *jobRunner) execute(ctx context.Context, id int64) {
	f := j.f
	job, err := f.Store.GetJob(ctx, id)
	if err != nil {
		return
	}
	var p JobParams
	json.Unmarshal([]byte(job.Params), &p)
	actor := job.Actor
	f.Store.UpdateJob(ctx, db.UpdateJobParams{State: "running", ID: id})
	f.Live.Notify(liveChange("job", strconv.FormatInt(id, 10)))
	items, _ := f.Store.ListJobItems(ctx, id)
	if p.Action == ActionSetRotation {
		j.setRotation(ctx, id, p, items, actor)
		return
	}
	if p.Action == ActionAutoUpdate {
		j.autoUpdate(ctx, id, p, items)
		return
	}
	if p.Action == ActionUpdate {
		j.prepareUpdate(ctx, id, p, items, actor)
	}
	waves := map[int64][]db.JobItem{}
	var order []int64
	for _, it := range items {
		if it.State == ItemDone || it.State == ItemRefused || it.State == ItemSkipped {
			continue
		}
		if _, ok := waves[it.Wave]; !ok {
			order = append(order, it.Wave)
		}
		waves[it.Wave] = append(waves[it.Wave], it)
	}
	sort.Slice(order, func(a, b int) bool { return order[a] < order[b] })
	failed := ""
	for _, wave := range order {
		if ctx.Err() != nil {
			return
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, it := range waves[wave] {
			wg.Add(1)
			go func(slug string) {
				defer wg.Done()
				j.item(ctx, id, slug, ItemRunning, "")
				state, detail := j.one(ctx, p, slug, actor)
				j.item(ctx, id, slug, state, detail)
				if p.Action == ActionUpdate && state == ItemRefused {
					mu.Lock()
					failed = slug + ": " + detail
					mu.Unlock()
				}
			}(it.Slug)
		}
		wg.Wait()
		if failed != "" {
			// (an update that does not come up stops the job: the rest stay
			// on their release)
			f.Store.UpdateJob(context.Background(), db.UpdateJobParams{State: "failed", FinishedAt: ptrInt(f.Now().Unix()),
				Detail: "stopped: " + failed, ID: id})
			for _, rest := range items {
				if rest.Wave > wave {
					j.item(ctx, id, rest.Slug, ItemSkipped, "the job stopped")
				}
			}
			f.raiseAlert(context.Background(), "update_failed", strconv.FormatInt(id, 10), failed)
			f.Live.Notify(liveChange("job", strconv.FormatInt(id, 10)))
			return
		}
	}
	if ctx.Err() != nil {
		return
	}
	state := "done"
	items, _ = f.Store.ListJobItems(context.Background(), id)
	refused := 0
	for _, it := range items {
		if it.State != ItemDone {
			refused++
		}
	}
	detail := ""
	if refused > 0 {
		detail = fmt.Sprintf("%d of %d not done", refused, len(items))
	}
	f.Store.UpdateJob(context.Background(), db.UpdateJobParams{State: state, FinishedAt: ptrInt(f.Now().Unix()), Detail: detail, ID: id})
	f.Live.Notify(liveChange("job", strconv.FormatInt(id, 10)))
}

// one applies an action to one instance: its item's state and words.
func (j *jobRunner) one(ctx context.Context, p JobParams, slug, actor string) (string, string) {
	f := j.f
	i, err := f.Instance(ctx, slug)
	if err != nil {
		return ItemSkipped, "removed"
	}
	if !f.Hub.Online(i.NodeID) && p.Action != ActionDelete && p.Action != ActionAddGroup && p.Action != ActionRemoveGroup &&
		p.Action != ActionChangeProfile {
		return ItemOffline, "the node is offline"
	}
	refused := func(err error) (string, string) { return ItemRefused, err.Error() }
	switch p.Action {
	case ActionStart:
		if err := f.SetRunning(ctx, actor, slug, true); err != nil {
			return refused(err)
		}
		if state := f.waitState(ctx, slug, 3*time.Minute, "up", "crashed", "blocked"); state != "up" {
			return ItemRefused, "not up: " + stateWords(f.Live.Instance(slug))
		}
	case ActionStop:
		if err := f.SetRunning(ctx, actor, slug, false); err != nil {
			return refused(err)
		}
	case ActionRestart:
		before := f.since(slug)
		if err := f.Restart(ctx, actor, slug); err != nil {
			return refused(err)
		}
		if state := f.waitUp(ctx, slug, 3*time.Minute, "", before); state != "up" {
			return ItemRefused, "not up: " + stateWords(f.Live.Instance(slug))
		}
	case ActionDrainRestart, ActionDrainStop:
		restart := p.Action == ActionDrainRestart
		before := f.since(slug)
		if err := f.Drain(ctx, actor, slug, p.After, p.Notice, time.Duration(p.Deadline)*time.Second, restart); err != nil {
			return refused(err)
		}
		wait := time.Duration(p.Deadline)*time.Second + agentproto.DrainEndGrace + 3*time.Minute
		if restart {
			if state := f.waitUp(ctx, slug, wait, "", before); state != "up" {
				return ItemRefused, "not up again: " + stateWords(f.Live.Instance(slug))
			}
		} else if state := f.waitState(ctx, slug, wait, "stopped", "crashed", "blocked"); state == "" {
			return ItemRefused, "did not stop in time"
		}
	case ActionUpdate:
		return j.update(ctx, p, i, actor)
	case ActionSay:
		if err := f.Call(ctx, slug, http.MethodPost, "/api/say", map[string]string{"text": p.Text}, nil); err != nil {
			return refused(err)
		}
	case ActionNext:
		if err := f.Call(ctx, slug, http.MethodPost, "/api/rotation/next", map[string]any{}, nil); err != nil {
			return refused(err)
		}
	case ActionEndGame:
		if _, err := f.Command(ctx, actor, slug, "sv_end_game", "admin"); err != nil {
			return refused(err)
		}
	case ActionCommand:
		lines, err := f.Command(ctx, actor, slug, p.Text, p.Role)
		if err != nil {
			return refused(err)
		}
		return ItemDone, strings.Join(lines, " · ")
	case ActionChangeProfile:
		pid := p.ProfileID
		if err := f.UpdateInstance(ctx, actor, slug, InstanceUpdate{Name: i.Name, ProfileID: &pid, Overrides: i.Overrides,
			KeyID: i.KeyID, Channel: i.Channel, Notes: i.Notes}); err != nil {
			return refused(err)
		}
	case ActionAddGroup, ActionRemoveGroup:
		if err := f.SetGroups(ctx, actor, []string{slug}, p.Text, p.Action == ActionAddGroup); err != nil {
			return refused(err)
		}
	case ActionDelete:
		if err := f.DeleteInstance(ctx, actor, slug); err != nil {
			return refused(err)
		}
	}
	return ItemDone, ""
}

func stateWords(l *live.Instance) string {
	if l == nil {
		return "unknown"
	}
	if l.Reason != "" {
		return l.State + ": " + l.Reason
	}
	return l.State
}

// since is when an instance's state began (zero when it has none yet).
func (f *Fleet) since(slug string) time.Time {
	if l := f.Live.Instance(slug); l != nil {
		return l.Since
	}
	return time.Time{}
}

// waitUp waits for an instance to be up again (on a build, "": any) after a
// restart asked at a moment (its state's start then): "up", or the state it
// stopped in.
func (f *Fleet) waitUp(ctx context.Context, slug string, timeout time.Duration, build string, before time.Time) string {
	deadline := time.After(timeout)
	changes, cancel := f.Live.Subscribe()
	defer cancel()
	for {
		l := f.Live.Instance(slug)
		if l != nil && l.Since.After(before) {
			if l.State == "up" && (build == "" || l.Build == build) {
				return "up"
			}
			if l.State == "crashed" || l.State == "blocked" {
				return l.State
			}
		}
		select {
		case <-ctx.Done():
			return ""
		case <-deadline:
			return ""
		case <-changes:
		case <-time.After(2 * time.Second):
		}
	}
}

// prepareUpdate has every node pull the release before the waves (so that
// a restart waits for nothing).
func (j *jobRunner) prepareUpdate(ctx context.Context, id int64, p JobParams, items []db.JobItem, actor string) {
	f := j.f
	build, err := f.ResolveChannel(ctx, p.Channel)
	if err != nil {
		return
	}
	nodes := map[int64]bool{}
	for _, it := range items {
		if i, err := f.Store.GetInstance(ctx, it.Slug); err == nil {
			nodes[i.NodeID] = true
		}
	}
	// (a spec naming the build: an instance of the job's, given the channel
	// at its own wave; the pull is asked by a spec that lists the image)
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		ready := true
		for n := range nodes {
			l := f.Live.Node(n)
			if l == nil || !l.Online {
				continue
			}
			has := false
			for _, b := range l.Heartbeat.Images {
				if b == build {
					has = true
				}
			}
			if !has {
				ready = false
				f.prefetch(ctx, n, build)
			}
		}
		if ready {
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// prefetch asks a node to fetch a build before an update reaches its
// servers: an agent fetches every build its spec's images name, so the
// spec carries the build as an image even before any instance runs it.
func (f *Fleet) prefetch(ctx context.Context, node int64, build string) {
	f.prefetchMu.Lock()
	if f.prefetching == nil {
		f.prefetching = map[int64]map[string]bool{}
	}
	if f.prefetching[node] == nil {
		f.prefetching[node] = map[string]bool{}
	}
	already := f.prefetching[node][build]
	f.prefetching[node][build] = true
	f.prefetchMu.Unlock()
	if !already {
		f.Push(node)
	}
}

// update moves one instance to the job's channel: drained, restarted on the
// new build, up and listed again. A server that was up and does not come up
// on it is put back on its build, pinned there, with an alert
// (update_rolled_back); one that was not up before either (no game data, a
// configuration refused) stays on the new build, which is not what failed.
// Either way the job stops.
func (j *jobRunner) update(ctx context.Context, p JobParams, i Instance, actor string) (string, string) {
	f := j.f
	build, err := f.ResolveChannel(ctx, p.Channel)
	if err != nil {
		return ItemRefused, err.Error()
	}
	l := f.Live.Instance(i.Slug)
	previous, wasUp, was := "", false, stateWords(l)
	if l != nil {
		previous, wasUp = l.Build, l.State == "up"
	}
	if previous == build && l != nil && l.State == "up" && !l.RestartNeeded {
		return ItemSkipped, "runs " + build + " already"
	}
	channel := p.Channel
	if err := f.UpdateInstance(ctx, actor, i.Slug, InstanceUpdate{Name: i.Name, ProfileID: i.ProfileID, Overrides: i.Overrides,
		KeyID: i.KeyID, Channel: channel, Notes: i.Notes}); err != nil {
		return ItemRefused, err.Error()
	}
	f.PushNow(ctx, i.NodeID)
	if i.Running == 0 {
		return ItemDone, "stopped: it runs " + build + " at its next start"
	}
	before := f.since(i.Slug)
	if err := f.Drain(ctx, actor, i.Slug, p.After, p.Notice, time.Duration(p.Deadline)*time.Second, true); err != nil {
		return ItemRefused, err.Error()
	}
	wait := time.Duration(p.Deadline)*time.Second + agentproto.DrainEndGrace + 5*time.Minute
	state := f.waitUp(ctx, i.Slug, wait, build, before)
	return j.landed(ctx, i, build, previous, wasUp, was, state)
}

// landed is the outcome of an update restarted onto a build, from the state
// it came to (waitUp's): up on it, or put back on its previous build and
// pinned there (update_rolled_back) when it was up before. A job cancelled
// meanwhile is not a failure of the build, and rolls nothing back.
func (j *jobRunner) landed(ctx context.Context, i Instance, build, previous string, wasUp bool, was, state string) (string, string) {
	f := j.f
	if state == "up" {
		return ItemDone, "runs " + build
	}
	if ctx.Err() != nil {
		return ItemSkipped, "the job was cancelled while it restarted"
	}
	now := stateWords(f.Live.Instance(i.Slug))
	if !wasUp {
		// (it did not run before the update either: what stops it is not the
		// build, and its old one would do no better)
		return ItemRefused, fmt.Sprintf("did not come up on %s (%s): left on it, as it was not up before the "+
			"update either (%s)", build, now, was)
	}
	if previous == "" {
		return ItemRefused, fmt.Sprintf("did not come up on %s (%s): left on it, as the build it ran is not known",
			build, now)
	}
	// (the rollback: started again on the build it ran, pinned to it, so that
	// a restart does not take the new one again)
	f.UpdateInstance(context.Background(), "auto", i.Slug, InstanceUpdate{Name: i.Name, ProfileID: i.ProfileID,
		Overrides: i.Overrides, KeyID: i.KeyID, Channel: previous, Notes: i.Notes})
	f.Restart(context.Background(), "auto", i.Slug)
	f.raiseAlert(context.Background(), "update_rolled_back", i.Slug, fmt.Sprintf("pinned to %s after a failed "+
		"update to %s (%s): its Release set to follow a channel again ends it", previous, build, now))
	return ItemRefused, fmt.Sprintf("did not come up on %s (%s): put back on %s, and pinned to it", build, now, previous)
}

// PinNote is why an instance is pinned to its build, after an update that
// did not come up ("" when it is not).
func (f *Fleet) PinNote(ctx context.Context, slug string) string {
	a, err := f.Store.GetOpenAlert(ctx, db.GetOpenAlertParams{Rule: "update_rolled_back", Target: slug})
	if err != nil {
		return ""
	}
	return a.Detail
}

// setRotation sets a rotation on every instance, or on none: each is set,
// and when one refuses, those set are put back.
func (j *jobRunner) setRotation(ctx context.Context, id int64, p JobParams, items []db.JobItem, actor string) {
	f := j.f
	entries := spec.RotationEntries(p.Text)
	type done struct {
		slug    string
		entries []string
	}
	var set []done
	refusal := ""
	// (items is as it was before: the servers tried are not those "not set")
	tried := map[string]bool{}
	for _, it := range items {
		tried[it.Slug] = true
		j.item(ctx, id, it.Slug, ItemRunning, "")
		var before struct {
			Entries []struct {
				Text string `json:"text"`
			} `json:"entries"`
		}
		if err := f.Call(ctx, it.Slug, http.MethodGet, "/api/rotation", nil, &before); err != nil {
			refusal = it.Slug + ": " + err.Error()
			j.item(ctx, id, it.Slug, ItemRefused, err.Error())
			break
		}
		if err := f.Call(ctx, it.Slug, http.MethodPut, "/api/rotation", map[string]any{"entries": entries}, nil); err != nil {
			refusal = it.Slug + ": " + err.Error()
			j.item(ctx, id, it.Slug, ItemRefused, err.Error())
			break
		}
		var old []string
		for _, e := range before.Entries {
			old = append(old, e.Text)
		}
		set = append(set, done{it.Slug, old})
		j.item(ctx, id, it.Slug, ItemDone, "")
	}
	state, detail := "done", ""
	if refusal != "" {
		for _, d := range set {
			f.Call(context.Background(), d.slug, http.MethodPut, "/api/rotation", map[string]any{"entries": d.entries}, nil)
			j.item(ctx, id, d.slug, ItemSkipped, "put back: another server refused the rotation")
		}
		for _, it := range items {
			if it.State == ItemWaiting && !tried[it.Slug] {
				j.item(ctx, id, it.Slug, ItemSkipped, "not set: another server refused the rotation")
			}
		}
		state, detail = "failed", "refused, set on none: "+refusal
	}
	f.Audit(context.Background(), actor, "rotation.set", strconv.FormatInt(id, 10), map[string]any{"rotation": p.Text, "outcome": state})
	f.Store.UpdateJob(context.Background(), db.UpdateJobParams{State: state, FinishedAt: ptrInt(f.Now().Unix()), Detail: detail, ID: id})
	f.Live.Notify(liveChange("job", strconv.FormatInt(id, 10)))
}

// Job is a job and its items.
func (f *Fleet) Job(ctx context.Context, id int64) (db.Job, []db.JobItem, error) {
	job, err := f.Store.GetJob(ctx, id)
	if err != nil {
		return db.Job{}, nil, NotFound("the job")
	}
	items, err := f.Store.ListJobItems(ctx, id)
	return job, items, err
}
