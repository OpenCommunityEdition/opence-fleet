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

func TestBanLine(t *testing.T) {
	now := time.Date(2026, 10, 26, 21, 4, 5, 0, time.UTC)
	until := int64(1793000000)
	cases := []struct {
		name string
		ban  db.FleetBan
		want string
	}{
		{"an address for ever", db.FleetBan{Address: "203.0.113.7", Names: "Chief", Reason: "aimbot"},
			"2026-10-26 21:04:05\tip=203.0.113.7\thwid=none\tdiscord_username=none\tdiscord_id=none\tplayers=Chief\treason=fleet ban: aimbot"},
		{"a hardware id for a while", db.FleetBan{Hwid: "0123456789abcdef0123456789abcdef", Until: &until},
			"2026-10-26 21:04:05\tip=none\thwid=0123456789abcdef0123456789abcdef\tdiscord_username=none\tdiscord_id=none" +
				"\tplayers=none\treason=fleet ban\tuntil=1793000000"},
		// (a verified Discord account's id bare: the server matches it)
		{"a Discord account", db.FleetBan{DiscordID: "300000000000000002", Names: "Pablo"},
			"2026-10-26 21:04:05\tip=none\thwid=none\tdiscord_username=none\tdiscord_id=300000000000000002\tplayers=Pablo\treason=fleet ban"},
		// (a tab or a new line in a name would make another field, or line)
		{"words kept to the printable", db.FleetBan{Address: "2001:db8::1", Names: "Chief\tip=1.2.3.4\nx", Reason: "wall\rhack é"},
			"2026-10-26 21:04:05\tip=2001:db8::1\thwid=none\tdiscord_username=none\tdiscord_id=none\tplayers=Chiefip=1.2.3.4x" +
				"\treason=fleet ban: wallhack "},
	}
	for _, c := range cases {
		if got := BanLine(c.ban, now); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// The server reads bans.txt a line of 511 characters at a time: a fleet
// ban's line stays shorter, its until whole.
func TestBanLineFitsTheServer(t *testing.T) {
	until := int64(1793000000)
	long := strings.Repeat("x", 2000)
	line := BanLine(db.FleetBan{Address: "203.0.113.7", Hwid: long, DiscordID: long, Names: long, Reason: long, Until: &until},
		time.Now())
	if len(line)+1 > 511 {
		t.Fatalf("a line of %d characters", len(line))
	}
	if !strings.HasSuffix(line, "\tuntil=1793000000") || strings.Count(line, "\t") != 7 {
		t.Fatalf("%q", line)
	}
}

func TestCreateBan(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	c := stop(f, now)
	for _, in := range []BanInput{{}, {Names: "Chief"}, {Address: "203.0.113"}, {Address: "a.b.c.d"}, {DiscordID: "12ab"},
		{DiscordID: "123456789012345678901"}, {Address: "203.0.113.7", DiscordID: "self-reported"}} {
		if e, ok := AsError(func() error { _, err := f.CreateBan(ctx, "user:paul", in); return err }()); !ok || e.Code != "schema" {
			t.Errorf("%+v: %v", in, e)
		}
	}
	forever, err := f.CreateBan(ctx, "user:paul", BanInput{Address: " 203.0.113.7 ", Names: "Chief", Reason: "aimbot"})
	if err != nil {
		t.Fatal(err)
	}
	day, err := f.CreateBan(ctx, "user:paul", BanInput{HWID: "abcd", Reason: "speed", Duration: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := f.Store.GetBan(ctx, day)
	if b.Until == nil || *b.Until != now.Add(24*time.Hour).Unix() || b.CreatedBy != "user:paul" {
		t.Fatalf("%+v", b)
	}
	b, _ = f.Store.GetBan(ctx, forever)
	if b.Until != nil || b.Address != "203.0.113.7" {
		t.Fatalf("%+v", b)
	}
	if a, ok := audited(t, f, "ban.create"); !ok || a.Actor != "user:paul" {
		t.Fatalf("%+v", a)
	}
	// a verified Discord account's id alone is a ban
	discord, err := f.CreateBan(ctx, "user:paul", BanInput{DiscordID: " 300000000000000002 ", Reason: "vpn hopping"})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := f.Store.GetBan(ctx, discord); b.DiscordID != "300000000000000002" || b.Address != "" || b.Hwid != "" {
		t.Fatalf("%+v", b)
	}
	if err := f.Unban(ctx, "user:paul", discord); err != nil {
		t.Fatal(err)
	}
	active := func() []int64 {
		bans, err := f.Bans(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, b := range bans {
			ids = append(ids, b.ID)
		}
		return ids
	}
	if ids := active(); len(ids) != 2 {
		t.Fatalf("%v", ids)
	}
	// a day on, the day's ban is over (and kept in the list of all)
	c.add(24*time.Hour + time.Second)
	if ids := active(); len(ids) != 1 || ids[0] != forever {
		t.Fatalf("%v", ids)
	}
	if all, _ := f.Bans(ctx, true); len(all) != 3 {
		t.Fatalf("%d bans in all", len(all))
	}
	// a removed ban is no longer active
	if err := f.Unban(ctx, "user:paul", forever); err != nil {
		t.Fatal(err)
	}
	if ids := active(); len(ids) != 0 {
		t.Fatalf("%v", ids)
	}
	if e, ok := AsError(f.Unban(ctx, "user:paul", 99)); !ok || e.Code != "not_found" {
		t.Fatalf("%v", e)
	}
}

// ban is the body of a POST /api/players/<slot>/ban an agent passed on.
type ban struct {
	slot     string
	Name     string `json:"name"`
	Reason   string `json:"reason"`
	Duration string `json:"duration"`
}

func bansOf(a *agent) []ban {
	var out []ban
	for _, r := range a.got(agentproto.TAPI) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/ban") {
			var b ban
			json.Unmarshal(r.Body, &b)
			b.slot = strings.TrimSuffix(strings.TrimPrefix(r.Path, "/api/players/"), "/ban")
			out = append(out, b)
		}
	}
	return out
}

// A machine of the list that joins is banned on sight: by its address, by
// its hardware id, or by the server's player list when the event has
// neither; an expired ban is not applied.
func TestBanOnSight(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	clk := stop(f, time.Now())
	_, token := node(t, f, "a", spec.DefaultNode())
	slug := servers(t, f, profile(t, f, "Ranked"), 1, false, "a")[0]
	a := connect(t, f, token, func(r request) *agentproto.Reply {
		if r.Type == agentproto.TAPI && r.Path == "/api/players" {
			return ok(`{"players":[{"slot":5,"name":"Arbiter","address":"198.51.100.20","hwid":"feedface"}]}`)
		}
		return ok(`{}`)
	})
	until(t, "the node online", func() bool { return f.Hub.Online(instance(t, f, slug).NodeID) })
	if _, err := f.CreateBan(ctx, "user:paul", BanInput{Address: "203.0.113.7", Reason: "aimbot"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreateBan(ctx, "user:paul", BanInput{HWID: "deadbeef", Reason: "speed", Duration: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreateBan(ctx, "user:paul", BanInput{HWID: "feedface", Reason: "old", Duration: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreateBan(ctx, "user:paul", BanInput{DiscordID: "300000000000000002", Reason: "vpn hopping"}); err != nil {
		t.Fatal(err)
	}
	// (the expired one: a minute is over)
	clk.add(2 * time.Minute)

	cases := []struct {
		name  string
		ev    eventFields
		slot  string
		bans  bool
		parts []string
	}{
		{"by address", eventFields{Slot: 1, Name: "Chief", Address: "203.0.113.7", HWID: "0000"}, "1", true,
			[]string{"fleet ban: aimbot"}},
		{"by hardware id", eventFields{Slot: 2, Name: "Johnson", Address: "192.0.2.1", HWID: "deadbeef"}, "2", true,
			[]string{"fleet ban: speed"}},
		{"another machine", eventFields{Slot: 3, Name: "Keyes", Address: "203.0.113.70", HWID: "deadbee"}, "", false, nil},
		{"an expired ban", eventFields{Slot: 4, Name: "Cortana", Address: "192.0.2.2", HWID: "feedface"}, "", false, nil},
		// (no address in the event: the player list's; feedface's ban is over)
		{"by the player list", eventFields{Slot: 5, Name: "Arbiter"}, "", false, nil},
		{"by a verified Discord account", eventFields{Slot: 6, Name: "Pablo", Address: "198.51.100.9", HWID: "abab",
			DiscordID: "300000000000000002", DiscordVerified: true}, "6", true, []string{"fleet ban: vpn hopping"}},
		// (what a machine says of its account is anyone's to say)
		{"a self-reported Discord account", eventFields{Slot: 7, Name: "Kat", Address: "198.51.100.10", HWID: "cdcd",
			DiscordID: "300000000000000002"}, "", false, nil},
	}
	for _, c := range cases {
		before := len(bansOf(a))
		f.checkJoin(ctx, slug, c.ev)
		got := bansOf(a)[before:]
		if !c.bans {
			if len(got) != 0 {
				t.Errorf("%s: banned %+v", c.name, got)
			}
			continue
		}
		if len(got) != 1 || got[0].slot != c.slot || got[0].Name != c.ev.Name {
			t.Errorf("%s: %+v", c.name, got)
			continue
		}
		for _, p := range c.parts {
			if !strings.Contains(got[0].Reason, p) {
				t.Errorf("%s: the reason %q", c.name, got[0].Reason)
			}
		}
	}
	// a ban for a while is a ban for the rest of it on the server
	if got := bansOf(a); len(got) < 2 || got[1].Duration != "58m" || got[0].Duration != "" {
		t.Fatalf("%+v", got)
	}
	// the player list's machine, banned while it was away
	if _, err := f.CreateBan(ctx, "user:paul", BanInput{Address: "198.51.100.20", Reason: "later"}); err != nil {
		t.Fatal(err)
	}
	before := len(bansOf(a))
	f.checkJoin(ctx, slug, eventFields{Slot: 5, Name: "Arbiter"})
	if got := bansOf(a)[before:]; len(got) != 1 || got[0].slot != "5" {
		t.Fatalf("%+v", got)
	}
	var onSight int
	for _, row := range audit(t, f) {
		if row.Action == "ban.on_sight" {
			onSight++
			if row.Actor != "auto" || row.Target != slug {
				t.Fatalf("%+v", row)
			}
		}
	}
	if onSight != 4 {
		t.Fatalf("%d bans on sight in the audit", onSight)
	}
}

// A fleet ban reaches every server: a line of bans.txt through the agent
// where the server is not up, pending where its node is offline.
func TestBanApplied(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	_, token := node(t, f, "a", spec.DefaultNode())
	node(t, f, "b", spec.DefaultNode())
	p := profile(t, f, "Ranked")
	online := servers(t, f, p, 1, false, "a")[0]
	offline := servers(t, f, p, 1, false, "b")[0]
	a := connect(t, f, token, func(r request) *agentproto.Reply { return &agentproto.Reply{OK: true} })
	until(t, "the node online", func() bool { return f.Hub.Online(instance(t, f, online).NodeID) })
	id, err := f.CreateBan(ctx, "user:paul", BanInput{Address: "203.0.113.7", Names: "Chief", Reason: "aimbot"})
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]db.InstanceBanState{}
	until(t, "the ban applied", func() bool {
		rows, _ := f.Store.ListBanStatesForBan(ctx, id)
		for _, r := range rows {
			states[r.Slug] = r
		}
		return len(rows) == 2
	})
	if s := states[online]; s.State != "applied" || s.Detail != "bans.txt" {
		t.Fatalf("%+v", s)
	}
	if s := states[offline]; s.State != "pending" {
		t.Fatalf("%+v", s)
	}
	controls := a.got(agentproto.TControl)
	if len(controls) != 1 || controls[0].Action != agentproto.BanFile || controls[0].Slug != online ||
		!strings.Contains(controls[0].Line, "\tip=203.0.113.7\t") || !strings.HasSuffix(controls[0].Line, "\treason=fleet ban: aimbot") {
		t.Fatalf("%+v", controls)
	}
}

func TestImportBans(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour).Unix()
	future := time.Now().Add(time.Hour).Unix()
	text := strings.Join([]string{
		"# a comment",
		"2026-01-01 10:00:00\tip=203.0.113.7\thwid=none\tdiscord_username=none\tdiscord_id=none\tplayers=Chief\treason=aimbot",
		"2026-01-01 10:00:00\tip=\thwid=abcd\tdiscord_username=none\tdiscord_id=none\tplayers=Johnson\treason=speed\tuntil=" + itoa(future),
		"2026-01-01 10:00:00\tip=192.0.2.1\thwid=none\tdiscord_username=none\tdiscord_id=none\tplayers=Keyes\treason=old\tuntil=" + itoa(past),
		"2026-01-01 10:00:00\tip=none\thwid=none\tplayers=nobody\treason=nothing",
		"2026-01-01 10:00:00\tip=none\thwid=none\tdiscord_username=pablo\tdiscord_id=300000000000000002\tplayers=Pablo\treason=vpn",
		"2026-01-01 10:00:00\tip=none\thwid=none\tdiscord_username=kat (self-reported)\tdiscord_id=300000000000000003 (self-reported)" +
			"\tplayers=Kat\treason=said so",
		"",
	}, "\n")
	n, err := f.ImportBans(ctx, "user:paul", text)
	if err != nil || n != 3 {
		t.Fatal(n, err)
	}
	bans, _ := f.Bans(ctx, false)
	byName := map[string]db.FleetBan{}
	for _, b := range bans {
		byName[b.Names] = b
	}
	if b := byName["Chief"]; b.Address != "203.0.113.7" || b.Hwid != "" || b.Until != nil || b.SourceSlug != "import" {
		t.Fatalf("%+v", b)
	}
	if b := byName["Johnson"]; b.Hwid != "abcd" || b.Address != "" || b.Until == nil {
		t.Fatalf("%+v", b)
	}
	if b := byName["Pablo"]; b.DiscordID != "300000000000000002" || b.Address != "" || b.Hwid != "" {
		t.Fatalf("%+v", b)
	}
	if _, ok := byName["Kat"]; ok {
		t.Fatal("a self-reported Discord account was imported")
	}
}

// A ban of a verified Discord account reaches the servers whose game has
// them (the link capability) by POST /api/bans' discord_id; a server
// without waits (unsupported) until its game says it has, and a ban by
// address too reaches it without the Discord part.
func TestDiscordBanApplied(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	nodeID, token := node(t, f, "a", spec.DefaultNode())
	slugs := servers(t, f, profile(t, f, "Ranked"), 2, false, "a")
	linked, older := slugs[0], slugs[1]
	a := connect(t, f, token, func(r request) *agentproto.Reply {
		if r.Type == agentproto.TAPI && r.Path == "/api/bans" {
			return ok(`{"ok":true,"n":0}`)
		}
		return ok(`{}`)
	})
	until(t, "the node online", func() bool { return f.Hub.Online(nodeID) })
	f.Live.SetInstance(nodeID, agentproto.Instance{Slug: linked, State: agentproto.Up,
		Capabilities: []string{"events", "bans", "ban_address", "vpn", "link"}})
	f.Live.SetInstance(nodeID, agentproto.Instance{Slug: older, State: agentproto.Up, Capabilities: []string{"events", "bans", "ban_address"}})
	posts := func(slug string) []map[string]any {
		var out []map[string]any
		for _, r := range a.got(agentproto.TAPI) {
			if r.Slug == slug && r.Method == http.MethodPost && r.Path == "/api/bans" {
				var body map[string]any
				json.Unmarshal(r.Body, &body)
				out = append(out, body)
			}
		}
		return out
	}
	state := func(id int64, slug string) db.InstanceBanState {
		rows, _ := f.Store.ListBanStatesForBan(ctx, id)
		for _, r := range rows {
			if r.Slug == slug {
				return r
			}
		}
		return db.InstanceBanState{}
	}
	id, err := f.CreateBan(ctx, "user:paul", BanInput{DiscordID: "300000000000000002", Names: "Pablo", Reason: "vpn hopping"})
	if err != nil {
		t.Fatal(err)
	}
	until(t, "the ban applied", func() bool { return state(id, linked).State != "" && state(id, older).State != "" })
	if s := state(id, linked); s.State != "applied" || s.Detail != "POST /api/bans" {
		t.Fatalf("%+v", s)
	}
	if got := posts(linked); len(got) != 1 || got[0]["discord_id"] != "300000000000000002" || got[0]["ip"] != nil {
		t.Fatalf("%+v", got)
	}
	if s := state(id, older); s.State != "unsupported" || !strings.Contains(s.Detail, "link capability") {
		t.Fatalf("%+v", s)
	}
	if got := posts(older); len(got) != 0 {
		t.Fatalf("a Discord ban sent to a server without the link capability: %+v", got)
	}
	// a ban by address and account: the older server gets the address alone
	both, err := f.CreateBan(ctx, "user:paul", BanInput{Address: "198.51.100.9", DiscordID: "300000000000000004", Reason: "both"})
	if err != nil {
		t.Fatal(err)
	}
	until(t, "the second ban applied", func() bool { return state(both, older).State != "" && state(both, linked).State != "" })
	if got := posts(older); len(got) != 1 || got[0]["ip"] != "198.51.100.9" || got[0]["discord_id"] != nil {
		t.Fatalf("%+v", got)
	}
	if got := posts(linked); len(got) != 2 || got[1]["ip"] != "198.51.100.9" || got[1]["discord_id"] != "300000000000000004" {
		t.Fatalf("%+v", got)
	}
	// the older server updated: its hello names link, and the ban that
	// waited is applied
	data, _ := json.Marshal(agentproto.Instance{Slug: older, State: agentproto.Up, Capabilities: []string{"ban_address", "link"}})
	f.Report(nodeID, agentproto.TInstance, data)
	until(t, "the waiting ban applied", func() bool { return state(id, older).State == "applied" })
	if got := posts(older); len(got) != 2 || got[1]["discord_id"] != "300000000000000002" {
		t.Fatalf("%+v", got)
	}
}

// A server's own ban of a verified player is imported with the account; a
// self-reported one is not.
func TestImportServerBanDiscord(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	nodeID, token := node(t, f, "a", spec.DefaultNode())
	slug := servers(t, f, profile(t, f, "Ranked"), 1, false, "a")[0]
	connect(t, f, token, func(r request) *agentproto.Reply {
		if r.Type == agentproto.TAPI && r.Path == "/api/bans" && r.Method == http.MethodGet {
			return ok(`{"ok":true,"bans":[
				{"n":0,"when":"2026-10-26 12:00:00","ip":"","hwid":"","discord_id":"300000000000000003","discord_verified":false,"players":"Kat","reason":"said so","until":null},
				{"n":1,"when":"2026-10-26 12:00:00","ip":"","hwid":"","discord_id":"300000000000000002","discord_verified":true,"players":"Pablo","reason":"vpn","until":null}]}`)
		}
		return ok(`{}`)
	})
	until(t, "the node online", func() bool { return f.Hub.Online(nodeID) })
	f.importServerBan(ctx, slug, eventFields{Event: "ban", Name: "Pablo", By: "web admin"})
	f.importServerBan(ctx, slug, eventFields{Event: "ban", Name: "Kat", By: "web admin"})
	bans, _ := f.Bans(ctx, false)
	if len(bans) != 1 || bans[0].DiscordID != "300000000000000002" || bans[0].Names != "Pablo" || bans[0].SourceSlug != slug {
		t.Fatalf("%+v", bans)
	}
}
