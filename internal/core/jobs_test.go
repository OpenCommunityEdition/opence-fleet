package core

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

func TestWaves(t *testing.T) {
	f, _ := fleet(t)
	for _, name := range []string{"a", "b", "c"} {
		node(t, f, name, spec.DefaultNode())
	}
	p := profile(t, f, "Ranked")
	onA := servers(t, f, p, 10, false, "a")
	onB := servers(t, f, p, 5, false, "b")
	onC := servers(t, f, p, 1, false, "c")
	cases := []struct {
		name    string
		action  string
		slugs   []string
		perNode int
		// the servers in each wave, and at most per node in one
		sizes []int
		most  int
	}{
		{"ten on a node, three at a time", ActionRestart, onA, 3, []int{3, 3, 3, 1}, 3},
		{"two nodes, two at a time on each", ActionDrainRestart, append(append([]string{}, onA[:5]...), onB...), 2,
			[]int{4, 4, 2}, 2},
		{"some of a node's", ActionUpdate, onA[2:5], 2, []int{2, 1}, 2},
		{"a node of one server", ActionUpdate, append(append([]string{}, onB[:2]...), onC...), 2, []int{3}, 2},
		{"all at once, not rolling", ActionSay, append(append([]string{}, onA...), onB...), 2, []int{15}, 10},
		{"one at a time", ActionRestart, onB, 1, []int{1, 1, 1, 1, 1}, 1},
	}
	for _, k := range cases {
		instances := map[string]db.Instance{}
		for _, slug := range k.slugs {
			instances[slug] = instance(t, f, slug)
		}
		waves := f.waves(JobParams{Action: k.action, Slugs: k.slugs, PerNode: k.perNode}, instances)
		if len(waves) != len(k.slugs) {
			t.Errorf("%s: %d of %d servers in a wave", k.name, len(waves), len(k.slugs))
			continue
		}
		sizes := make([]int, len(k.sizes))
		perNode := map[[2]int64]int{}
		bad := false
		for slug, w := range waves {
			if w < 0 || w >= len(sizes) {
				bad = true
				continue
			}
			sizes[w]++
			key := [2]int64{instances[slug].NodeID, int64(w)}
			perNode[key]++
			if perNode[key] > k.most {
				bad = true
			}
		}
		for i := range sizes {
			if sizes[i] != k.sizes[i] {
				bad = true
			}
		}
		if bad {
			t.Errorf("%s: waves %v (sizes %v, not %v)", k.name, waves, sizes, k.sizes)
		}
	}
}

func TestStartJobRefusals(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	node(t, f, "a", spec.DefaultNode())
	slugs := servers(t, f, profile(t, f, "Ranked"), 2, false, "a")
	for _, c := range []struct {
		name string
		p    JobParams
		code string
	}{
		{"no servers", JobParams{Action: ActionStart}, "schema"},
		{"an unknown action", JobParams{Action: "reboot", Slugs: slugs}, "schema"},
		{"say nothing", JobParams{Action: ActionSay, Slugs: slugs, Text: "  "}, "schema"},
		{"an empty rotation", JobParams{Action: ActionSetRotation, Slugs: slugs, Text: " , "}, "schema"},
		{"an unknown profile", JobParams{Action: ActionChangeProfile, Slugs: slugs, ProfileID: 99}, "not_found"},
		{"no release", JobParams{Action: ActionUpdate, Slugs: slugs}, "no_release"},
		{"no group", JobParams{Action: ActionAddGroup, Slugs: slugs, Text: "!!"}, "schema"},
		{"an unknown server", JobParams{Action: ActionStop, Slugs: []string{slugs[0], "nope"}}, "not_found"},
	} {
		if _, err := f.StartJob(ctx, "user:paul", c.p); err == nil {
			t.Errorf("%s: started", c.name)
		} else if e, ok := AsError(err); !ok || e.Code != c.code {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// finished waits for a job to end: its state and items.
func finished(t *testing.T, f *Fleet, id int64) (db.Job, map[string]db.JobItem) {
	t.Helper()
	var job db.Job
	until(t, "the job's end", func() bool {
		job, _, _ = f.Job(context.Background(), id)
		return job.FinishedAt != nil
	})
	_, items, _ := f.Job(context.Background(), id)
	bySlug := map[string]db.JobItem{}
	for _, it := range items {
		bySlug[it.Slug] = it
	}
	return job, bySlug
}

func TestJobOnAnOfflineNode(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	node(t, f, "a", spec.DefaultNode())
	slugs := servers(t, f, profile(t, f, "Ranked"), 2, false, "a")
	id, err := f.StartJob(ctx, "user:paul", JobParams{Action: ActionSay, Slugs: slugs, Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	job, items := finished(t, f, id)
	if job.State != "done" || job.Detail != "2 of 2 not done" || job.Title != "Say: hello" || job.Actor != "user:paul" {
		t.Fatalf("%+v", job)
	}
	for _, slug := range slugs {
		if items[slug].State != ItemOffline {
			t.Fatalf("%+v", items[slug])
		}
	}
	// what needs no agent is done all the same
	id, err = f.StartJob(ctx, "user:paul", JobParams{Action: ActionAddGroup, Slugs: slugs, Text: "EU West"})
	if err != nil {
		t.Fatal(err)
	}
	if job, items := finished(t, f, id); job.State != "done" || job.Detail != "" || items[slugs[1]].State != ItemDone {
		t.Fatalf("%+v %+v", job, items)
	}
	if members, _ := f.GroupMembers(ctx, "eu-west"); len(members) != 2 {
		t.Fatalf("%v", members)
	}
}

// A cancelled job stops between its waves: what runs ends, the next wave
// never starts.
func TestCancelJob(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	_, token := node(t, f, "a", spec.DefaultNode())
	slugs := servers(t, f, profile(t, f, "Ranked"), 4, true, "a")
	// (the agent takes the restarts and never says)
	a := connect(t, f, token, func(r request) *agentproto.Reply {
		if r.Type == agentproto.TControl {
			return nil
		}
		return &agentproto.Reply{OK: true}
	})
	until(t, "the node online", func() bool { return f.Hub.Online(instance(t, f, slugs[0]).NodeID) })
	id, err := f.StartJob(ctx, "user:paul", JobParams{Action: ActionRestart, Slugs: slugs, PerNode: 2})
	if err != nil {
		t.Fatal(err)
	}
	until(t, "the first wave's restarts", func() bool { return len(a.got(agentproto.TControl)) == 2 })
	if err := f.CancelJob(ctx, "user:paul", id); err != nil {
		t.Fatal(err)
	}
	until(t, "the first wave's end", func() bool {
		_, items, _ := f.Job(ctx, id)
		for _, it := range items {
			if it.State == ItemRunning {
				return false
			}
		}
		return true
	})
	time.Sleep(300 * time.Millisecond)
	job, items, _ := f.Job(ctx, id)
	if job.State != "cancelled" || job.Detail != "cancelled by user:paul" {
		t.Fatalf("%+v", job)
	}
	if n := len(a.got(agentproto.TControl)); n != 2 {
		t.Fatalf("%d restarts asked", n)
	}
	waiting := 0
	for _, it := range items {
		if it.Wave == 1 && it.State == ItemWaiting {
			waiting++
		}
	}
	if waiting != 2 {
		t.Fatalf("%+v", items)
	}
	if e, ok := AsError(f.CancelJob(ctx, "user:paul", id)); !ok || e.Code != "finished" {
		t.Fatalf("%v", e)
	}
	if a, ok := audited(t, f, "job.cancel"); !ok || a.Actor != "user:paul" {
		t.Fatalf("%+v", a)
	}
}

// A rotation is set on every server or on none: one refusal puts back those
// set, and the rest are not tried.
func TestSetRotationAllOrNone(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	_, token := node(t, f, "a", spec.DefaultNode())
	slugs := servers(t, f, profile(t, f, "Ranked"), 3, false, "a")
	a := connect(t, f, token, func(r request) *agentproto.Reply {
		if r.Type != agentproto.TAPI || r.Path != "/api/rotation" {
			return ok(`{}`)
		}
		if r.Method == http.MethodGet {
			return ok(`{"entries":[{"text":"old_` + r.Slug + `:ctf"}]}`)
		}
		if r.Slug == slugs[1] && !strings.Contains(string(r.Body), "old_") {
			return &agentproto.Reply{OK: true, Status: 400, Body: json.RawMessage(`{"ok":false,"error":{"code":"rejected",` +
				`"message":"the rotation was refused","rejected":[{"index":0,"entry":"bloodgulch:nope","reason":"no gametype nope"}]}}`)}
		}
		return ok(`{}`)
	})
	until(t, "the node online", func() bool { return f.Hub.Online(instance(t, f, slugs[0]).NodeID) })
	id, err := f.StartJob(ctx, "user:paul", JobParams{Action: ActionSetRotation, Slugs: slugs, Text: "bloodgulch:nope, sidewinder:ctf"})
	if err != nil {
		t.Fatal(err)
	}
	job, items := finished(t, f, id)
	if job.State != "failed" || !strings.Contains(job.Detail, "set on none") || !strings.Contains(job.Detail, "no gametype nope") {
		t.Fatalf("%+v", job)
	}
	// the first set, then put back
	var puts []string
	for _, r := range a.got(agentproto.TAPI) {
		if r.Method == http.MethodPut {
			var body struct {
				Entries []string `json:"entries"`
			}
			json.Unmarshal(r.Body, &body)
			puts = append(puts, r.Slug+" "+strings.Join(body.Entries, ","))
		}
	}
	want := []string{slugs[0] + " bloodgulch:nope,sidewinder:ctf", slugs[1] + " bloodgulch:nope,sidewinder:ctf",
		slugs[0] + " old_" + slugs[0] + ":ctf"}
	if strings.Join(puts, "; ") != strings.Join(want, "; ") {
		t.Fatalf("puts %q", puts)
	}
	if it := items[slugs[0]]; it.State != ItemSkipped || !strings.Contains(it.Detail, "put back") {
		t.Errorf("the one put back: %+v", it)
	}
	if it := items[slugs[1]]; it.State != ItemRefused || !strings.Contains(it.Detail, "no gametype nope") {
		t.Errorf("the one that refused: %+v", it)
	}
	if it := items[slugs[2]]; it.State != ItemSkipped || !strings.Contains(it.Detail, "not set") {
		t.Errorf("the one not tried: %+v", it)
	}
	if a, ok := audited(t, f, "rotation.set"); !ok || a.Actor != "user:paul" || !strings.Contains(a.Detail, `"outcome":"failed"`) {
		t.Fatalf("%+v", a)
	}
}
