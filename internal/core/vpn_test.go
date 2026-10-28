package core

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

// A server with the vpn capability that holds no list for two hours, or
// whose lists are all over 48 hours old, is an alert (vpn_list), asked by
// the alerts' loop through its agent; resolved once it holds a fresh one.
func TestVPNListAlert(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	start := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	clk := stop(f, start)
	nodeID, token := node(t, f, "a", spec.DefaultNode())
	slug := servers(t, f, profile(t, f, "Ranked"), 1, false, "a")[0]
	var mu sync.Mutex
	answer := `{"ok":true,"policy":"link","fail":"open","lists":[],"file":"","allow_ranges":0,"block_ranges":0,"held":false,"refused":{}}`
	asked := 0
	connect(t, f, token, func(r request) *agentproto.Reply {
		mu.Lock()
		defer mu.Unlock()
		if r.Type == agentproto.TAPI && r.Path == "/api/vpn" {
			asked++
			return ok(answer)
		}
		return ok(`{}`)
	})
	until(t, "the node online", func() bool { return f.Hub.Online(nodeID) })
	f.Live.SetInstance(nodeID, agentproto.Instance{Slug: slug, State: agentproto.Up, Capabilities: []string{"events", "vpn", "link"}})
	f.Live.SetStatus(nodeID, slug, json.RawMessage(`{"ok":true,"state":"lobby"}`))
	set := func(text string) {
		mu.Lock()
		answer = text
		mu.Unlock()
	}
	times := func() int {
		mu.Lock()
		defer mu.Unlock()
		return asked
	}
	// (each evaluation asks once its five minutes are over, in the background)
	evaluate := func(want int) {
		f.evaluate(ctx)
		until(t, "the lists asked", func() bool { return times() >= want })
		time.Sleep(50 * time.Millisecond)
	}
	evaluate(1)
	clk.add(time.Minute)
	f.evaluate(ctx)
	if times() != 1 {
		t.Fatalf("asked %d times in five minutes", times())
	}
	clk.add(time.Hour)
	evaluate(2)
	if open := openAlerts(t, f, "vpn_list"); len(open) != 0 {
		t.Fatalf("an alert after an hour: %+v", open)
	}
	clk.add(time.Hour + time.Minute)
	evaluate(3)
	open := openAlerts(t, f, "vpn_list")
	if len(open) != 1 || open[0].Target != slug || !strings.Contains(open[0].Detail, "no VPN list held") ||
		!strings.Contains(open[0].Detail, "let in meanwhile") {
		t.Fatalf("%+v", open)
	}
	// a fresh list: resolved
	fresh := clk.now().Add(-time.Hour).Unix()
	set(`{"ok":true,"policy":"link","fail":"open","held":true,"refused":{},"lists":[{"master":"https://master.example",` +
		`"key_id":"k","serial":3,"generated":` + itoa(fresh) + `,"fetched":null,"ranges":30582,"error":""}]}`)
	clk.add(5 * time.Minute)
	evaluate(4)
	until(t, "the alert resolved", func() bool { return len(openAlerts(t, f, "vpn_list")) == 0 })

	// lists all over 48 hours old: an alert at once
	old := clk.now().Add(-72 * time.Hour).Unix()
	set(`{"ok":true,"policy":"link","fail":"closed","held":true,"refused":{},"lists":[{"master":"https://master.example",` +
		`"key_id":"k","serial":3,"generated":` + itoa(old) + `,"fetched":null,"ranges":30582,"error":"unreachable"}]}`)
	clk.add(5 * time.Minute)
	evaluate(5)
	until(t, "the old lists' alert", func() bool {
		open := openAlerts(t, f, "vpn_list")
		return len(open) == 1 && strings.Contains(open[0].Detail, "3 days old")
	})
	// a server whose policy is off needs no list
	set(`{"ok":true,"policy":"off","fail":"open","held":false,"refused":{},"lists":[]}`)
	clk.add(5 * time.Minute)
	evaluate(6)
	until(t, "the alert resolved", func() bool { return len(openAlerts(t, f, "vpn_list")) == 0 })
	if RuleName("vpn_list") == "vpn_list" {
		t.Error("vpn_list has no words in Rules")
	}
}

// A join refused for the VPN policy is kept, counted for the server's card,
// and a line of the feed without the machine's address.
func TestJoinRefused(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	nodeID, _ := node(t, f, "a", spec.DefaultNode())
	slug := servers(t, f, profile(t, f, "Ranked"), 1, false, "a")[0]
	before := f.Now().Add(-time.Minute)
	for seq, reason := range []string{"vpn_link_required", "vpn_blocked"} {
		data, _ := json.Marshal(map[string]any{"event": "join_refused", "seq": seq + 1, "name": "Chief", "reason": reason,
			"address": "198.51.100.13", "hwid": "feed0003"})
		e, _ := json.Marshal(agentproto.Event{Slug: slug, Run: 1, Seq: int64(seq + 1), Event: "join_refused", Data: data})
		f.Report(nodeID, agentproto.TEvent, e)
	}
	until(t, "the refusals kept", func() bool { return f.RefusedSince(ctx, slug, before) == 2 })
	if n := f.RefusedSince(ctx, slug, f.Now().Add(time.Minute)); n != 0 {
		t.Fatalf("%d refusals after now", n)
	}
	feed := f.Live.Feed(10, slug)
	if len(feed) != 2 || feed[0].Kind != "mod" || feed[0].Text != "Chief refused at the door: a VPN (the server blocks them)" ||
		strings.Contains(feed[1].Text, "198.51.100") {
		t.Fatalf("%+v", feed)
	}
}
