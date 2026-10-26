package fleet_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// TestManyEvents is SQLite under a busy fleet (OPENCE_FLEET_PLAN.md 11): a
// node's 100 servers report a burst of events and statuses, as their agent
// would; every event is kept (one writer, in batches), none is dropped, and
// the statuses stay in memory.
func TestManyEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("a load test: not in -short")
	}
	h := start(t)
	ctx := context.Background()
	settings := spec.DefaultNode()
	settings.Runtime = "process"
	settings.TunnelPorts = spec.PortRange{First: 43000, Last: 43199}
	settings.WebPorts = spec.PortRange{First: 43200, Last: 43399}
	token, err := h.fleet.JoinToken(ctx, "test", "busy", settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.fleet.Join(ctx, core.JoinRequest{Token: token, Name: "busy", Runtime: "process"}); err != nil {
		t.Fatal(err)
	}
	node, err := h.fleet.NodeByName(ctx, "busy")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := h.fleet.CreateProfile(ctx, "user:paul", core.ProfileInput{Name: "Busy", Rotation: "bloodgulch:ctf"})
	if err != nil {
		t.Fatal(err)
	}
	slugs, _, err := h.fleet.CreateInstances(ctx, "user:paul", core.CreateRequest{ProfileID: profile, Nodes: []string{"busy"},
		Count: 100, Pattern: "Busy #{n}"})
	if err != nil || len(slugs) != 100 {
		t.Fatal(len(slugs), err)
	}

	const perServer = 60
	started := time.Now()
	for seq := int64(1); seq <= perServer; seq++ {
		for _, slug := range slugs {
			kind, data := "kill", map[string]any{"killer": 1, "victim": 2, "weapon": "pistol"}
			if seq%3 == 0 {
				kind, data = "chat", map[string]any{"slot": 1, "name": "Kestrel", "channel": "everyone", "text": "gg"}
			}
			data["type"], data["event"], data["seq"], data["time"] = "event", kind, seq, time.Now().UnixMilli()
			raw, _ := json.Marshal(data)
			message, _ := json.Marshal(agentproto.Event{Slug: slug, Run: 1, Seq: seq, Time: time.Now().UnixMilli(), Event: kind,
				Data: raw})
			h.fleet.Report(node.ID, agentproto.TEvent, message)
			if seq%10 == 0 {
				status, _ := json.Marshal(agentproto.Status{Slug: slug,
					Status: json.RawMessage(fmt.Sprintf(`{"ok":true,"state":"playing","players":%d,"max_players":16}`, seq%16))})
				h.fleet.Report(node.ID, agentproto.TStatus, status)
			}
		}
	}
	reported := time.Since(started)
	want := 100 * perServer
	var kept int
	h.until("every event kept", 30*time.Second, func() bool {
		rows, err := h.fleet.Store.ListEventsSince(ctx, db.ListEventsSinceParams{ID: 0, Limit: int64(want + 100)})
		kept = len(rows)
		return err == nil && kept >= want
	})
	t.Logf("%d events reported in %s, all kept in %s", want, reported.Round(time.Millisecond),
		time.Since(started).Round(time.Millisecond))
	if kept != want {
		t.Fatalf("%d events kept, %d reported", kept, want)
	}
	if reported > 10*time.Second {
		t.Fatalf("reporting %d events took %s: the agents' readers would fall behind", want, reported)
	}
}
