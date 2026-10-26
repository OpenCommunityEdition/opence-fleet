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

// updateRig is a node whose agent answers a drain at once and says the
// server ended blocked on the new build (no game data, say): an update
// that does not come up.
func updateRig(t *testing.T, before string) (*Fleet, []string) {
	t.Helper()
	f, _ := fleet(t)
	ctx := context.Background()
	for _, build := range []string{"b1", "b2"} {
		if err := f.AddLocalRelease(ctx, "test", build, "opence-server:"+build, build == "b1"); err != nil {
			t.Fatal(err)
		}
	}
	nodeID, token := node(t, f, "a", spec.DefaultNode())
	slugs := servers(t, f, profile(t, f, "Ranked"), 1, true, "a")
	report := func(slug, state, build, reason string, since time.Time) {
		data, _ := json.Marshal(agentproto.Instance{Slug: slug, State: state, Build: build, Reason: reason,
			Generation: 1, Since: since.UnixMilli()})
		f.Report(nodeID, agentproto.TInstance, data)
	}
	connect(t, f, token, func(r request) *agentproto.Reply {
		if r.Type == agentproto.TControl && r.Action == agentproto.Drain {
			go func() {
				time.Sleep(50 * time.Millisecond)
				report(r.Slug, "blocked", "b2", "no usable game data (exit 2)", time.Now())
			}()
		}
		return &agentproto.Reply{OK: true}
	})
	until(t, "the node online", func() bool { return f.Hub.Online(nodeID) })
	// (the node has both builds: the update's pull waits for nothing)
	heartbeat, _ := json.Marshal(agentproto.Heartbeat{Images: []string{"b1", "b2"}, Version: "test"})
	f.Report(nodeID, agentproto.THeartbeat, heartbeat)
	// (its state before the update: up on b1, or blocked already)
	report(slugs[0], before, "b1", map[string]string{"up": "", "blocked": "no usable game data (exit 2)"}[before],
		time.Now().Add(-time.Minute))
	until(t, "the server's state", func() bool {
		l := f.Live.Instance(slugs[0])
		return l != nil && l.State == before
	})
	return f, slugs
}

// A server that was up and does not come up on the new build is put back on
// its own, pinned to it, with an alert and a note for its page; the pin ends
// when its release is set again.
func TestUpdateRolledBack(t *testing.T) {
	f, slugs := updateRig(t, "up")
	ctx := context.Background()
	id, err := f.StartJob(ctx, "user:paul", JobParams{Action: ActionUpdate, Slugs: slugs, Channel: "b2", After: "now"})
	if err != nil {
		t.Fatal(err)
	}
	job, items := finished(t, f, id)
	if it := items[slugs[0]]; job.State != "failed" || it.State != ItemRefused ||
		!strings.Contains(it.Detail, "put back on b1, and pinned to it") {
		t.Fatalf("%+v %+v", job, it)
	}
	if i := instance(t, f, slugs[0]); i.Channel != "b1" {
		t.Fatalf("not pinned: %q", i.Channel)
	}
	note := f.PinNote(ctx, slugs[0])
	if !strings.Contains(note, "pinned to b1 after a failed update to b2") || !strings.Contains(note, "no usable game data") {
		t.Fatalf("the note: %q", note)
	}
	if _, err := f.Store.GetOpenAlert(ctx, db.GetOpenAlertParams{Rule: "update_rolled_back", Target: slugs[0]}); err != nil {
		t.Fatalf("no alert: %v", err)
	}
	// a person sets its release again: the pin, and its alert, end
	i, _ := f.Instance(ctx, slugs[0])
	if err := f.UpdateInstance(ctx, "user:paul", slugs[0], InstanceUpdate{Name: i.Name, ProfileID: i.ProfileID,
		Overrides: i.Overrides, KeyID: i.KeyID, Channel: "", Notes: i.Notes}); err != nil {
		t.Fatal(err)
	}
	if note := f.PinNote(ctx, slugs[0]); note != "" {
		t.Fatalf("still pinned: %q", note)
	}
}

// A server that was not up before the update either (no game data) is not
// the new build's fault: it stays on the new build, unpinned, no alert of a
// rollback, and the job stops all the same.
func TestUpdateOfAServerThatWasNotUp(t *testing.T) {
	f, slugs := updateRig(t, "blocked")
	ctx := context.Background()
	id, err := f.StartJob(ctx, "user:paul", JobParams{Action: ActionUpdate, Slugs: slugs, Channel: "b2", After: "now"})
	if err != nil {
		t.Fatal(err)
	}
	job, items := finished(t, f, id)
	if it := items[slugs[0]]; job.State != "failed" || it.State != ItemRefused ||
		!strings.Contains(it.Detail, "left on it, as it was not up before the update either") {
		t.Fatalf("%+v %+v", job, it)
	}
	if i := instance(t, f, slugs[0]); i.Channel != "b2" {
		t.Fatalf("its release: %q, not b2", i.Channel)
	}
	if note := f.PinNote(ctx, slugs[0]); note != "" {
		t.Fatalf("pinned: %q", note)
	}
}
