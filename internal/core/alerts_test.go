package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// post is a webhook's delivery.
type post struct {
	path, contentType string
	body              map[string]any
}

// receiver is the webhooks' end: an HTTPS server that keeps what it gets and
// answers status.
type receiver struct {
	server *httptest.Server
	mu     sync.Mutex
	posts  []post
	status int
}

func receive(t *testing.T, f *Fleet) *receiver {
	t.Helper()
	r := &receiver{status: http.StatusNoContent}
	r.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		data, _ := io.ReadAll(req.Body)
		p := post{path: req.URL.Path, contentType: req.Header.Get("Content-Type")}
		json.Unmarshal(data, &p.body)
		r.mu.Lock()
		r.posts = append(r.posts, p)
		status := r.status
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(r.server.Close)
	f.HTTP = r.server.Client()
	return r
}

// to is the posts to a path.
func (r *receiver) to(path string) []post {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []post
	for _, p := range r.posts {
		if p.path == path {
			out = append(out, p)
		}
	}
	return out
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.posts)
}

// quiet checks that nothing more came for a moment.
func (r *receiver) quiet(t *testing.T, n int) {
	t.Helper()
	time.Sleep(300 * time.Millisecond)
	if got := r.count(); got != n {
		t.Fatalf("%d posts, not %d", got, n)
	}
}

// noSecret checks that none of the secrets is in the log or the audit.
func noSecret(t *testing.T, f *Fleet, logs *logBuffer, secrets ...string) {
	t.Helper()
	text := logs.String()
	for _, a := range audit(t, f) {
		text += "\n" + a.Actor + " " + a.Action + " " + a.Target + " " + a.Detail
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(text, s) {
			t.Errorf("a secret (%d characters) in the log or the audit", len(s))
		}
	}
}

func openAlerts(t *testing.T, f *Fleet, rule string) []db.Alert {
	t.Helper()
	all, _ := f.Store.ListOpenAlerts(context.Background())
	var out []db.Alert
	for _, a := range all {
		if a.Rule == rule {
			out = append(out, a)
		}
	}
	return out
}

func TestAlertsAndWebhooks(t *testing.T) {
	f, logs := fleet(t)
	ctx := context.Background()
	clk := stop(f, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	f.PublicURL = "https://fleet.example"
	r := receive(t, f)
	if err := f.SetSetting(ctx, "user:paul", "name", "Halo night"); err != nil {
		t.Fatal(err)
	}
	node(t, f, "a", spec.DefaultNode())
	slug := servers(t, f, profile(t, f, "Ranked"), 1, false, "a")[0]
	discordURL := r.server.URL + "/api/webhooks/42/discord-SECRET-token"
	jsonURL := r.server.URL + "/hooks/json-SECRET-token"
	if _, err := f.AddWebhook(ctx, "user:paul", "Discord", discordURL, "discord", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.AddWebhook(ctx, "user:paul", "", jsonURL, "json", []string{"instance_crashed"}); err != nil {
		t.Fatal(err)
	}

	// an alert opens: told to both
	f.raiseAlert(ctx, "instance_crashed", slug, "exit 4\nsegfault")
	until(t, "the open alert told", func() bool { return r.count() == 2 })
	d := r.to("/api/webhooks/42/discord-SECRET-token")[0]
	if d.contentType != "application/json" || d.body["username"] != "OpenCE Fleet" {
		t.Fatalf("%+v", d)
	}
	embed := d.body["embeds"].([]any)[0].(map[string]any)
	want := map[string]any{"title": "A server crashed (exit 1, 4, a signal): " + slug, "description": "exit 4\nsegfault",
		"color": float64(0xf87171), "timestamp": "2026-10-26T12:00:00Z", "url": "https://fleet.example/servers/" + slug}
	for k, v := range want {
		if embed[k] != v {
			t.Errorf("the embed's %s is %v, not %v", k, embed[k], v)
		}
	}
	if embed["footer"].(map[string]any)["text"] != "Halo night" {
		t.Errorf("%v", embed["footer"])
	}
	// (no @everyone from a server's words)
	if parse := d.body["allowed_mentions"].(map[string]any)["parse"].([]any); len(parse) != 0 {
		t.Errorf("%v", parse)
	}
	j := r.to("/hooks/json-SECRET-token")[0]
	wantJSON := map[string]any{"fleet": "Halo night", "rule": "instance_crashed", "rule_name": "A server crashed (exit 1, 4, a signal)",
		"target": slug, "detail": "exit 4\nsegfault", "state": "open", "time": float64(clk.now().Unix()),
		"url": "https://fleet.example/servers/" + slug}
	for k, v := range wantJSON {
		if j.body[k] != v {
			t.Errorf("the JSON's %s is %v, not %v", k, j.body[k], v)
		}
	}
	if len(j.body) != len(wantJSON) {
		t.Errorf("%v", j.body)
	}

	// the same alert again while it is open: not told twice, its last seen
	// moved
	clk.add(time.Minute)
	f.raiseAlert(ctx, "instance_crashed", slug, "exit 4 again")
	r.quiet(t, 2)
	open := openAlerts(t, f, "instance_crashed")
	if len(open) != 1 || open[0].LastSeen != clk.now().Unix() || open[0].Detail != "exit 4 again" {
		t.Fatalf("%+v", open)
	}
	// (the labels of the webhooks told, which the resolution is told to)
	host := strings.TrimPrefix(r.server.URL, "https://")
	until(t, "the alert marked told", func() bool {
		a, _ := f.Store.GetAlert(ctx, open[0].ID)
		return a.Delivered == host+", Discord" || a.Delivered == "Discord, "+host
	})

	// a rule the JSON webhook does not take: Discord's alone
	f.raiseAlert(ctx, "disk_low", "a", "5 % free")
	until(t, "the disk alert told", func() bool { return r.count() == 3 })
	if len(r.to("/hooks/json-SECRET-token")) != 1 {
		t.Fatal("the JSON webhook was told of a rule it does not take")
	}

	// resolved: said where it was told
	f.resolveAlert("instance_crashed", slug)
	until(t, "the resolution told", func() bool { return r.count() == 5 })
	if len(openAlerts(t, f, "instance_crashed")) != 0 {
		t.Fatal("still open")
	}
	d = r.to("/api/webhooks/42/discord-SECRET-token")[2]
	embed = d.body["embeds"].([]any)[0].(map[string]any)
	if embed["title"] != "Resolved: A server crashed (exit 1, 4, a signal): "+slug || embed["color"] != float64(0x4ade80) {
		t.Fatalf("%v", embed)
	}
	if j := r.to("/hooks/json-SECRET-token"); j[1].body["state"] != "resolved" {
		t.Fatalf("%v", j[1].body)
	}
	// (resolving what is not open is nothing)
	f.resolveAlert("instance_crashed", slug)
	r.quiet(t, 5)

	// once resolved, the next time is a new alert, told again
	f.raiseAlert(ctx, "instance_crashed", slug, "exit 1")
	until(t, "the new alert told", func() bool { return r.count() == 7 })
	if again := openAlerts(t, f, "instance_crashed"); len(again) != 1 || again[0].ID == open[0].ID {
		t.Fatalf("%+v", again)
	}
	noSecret(t, f, logs, "SECRET", discordURL, jsonURL)
}

func TestWebhooks(t *testing.T) {
	f, logs := fleet(t)
	ctx := context.Background()
	r := receive(t, f)
	for _, raw := range []string{"http://example.com/hook", "example.com/hook", "https://", "ftp://example.com/x"} {
		if _, err := f.AddWebhook(ctx, "user:paul", "", raw, "", nil); err == nil {
			t.Errorf("%s: kept", raw)
		} else if e, ok := AsError(err); !ok || e.Code != "schema" {
			t.Errorf("%s: %v", raw, err)
		}
	}
	// the kind from the host when it is not said
	for _, c := range []struct{ url, kind string }{
		{"https://discord.com/api/webhooks/1/discord-SECRET-a", "discord"},
		{"https://hooks.example.org/b-SECRET", "json"},
	} {
		id, err := f.AddWebhook(ctx, "user:paul", "", c.url, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		w, _ := f.Store.GetWebhook(ctx, id)
		if w.Kind != c.kind || w.Rules != "*" || w.Label != WebhookHost(c.url) {
			t.Errorf("%s: %+v", c.url, w)
		}
	}
	hooks, _ := f.Webhooks(ctx)
	if len(hooks) != 2 || hooks[0].URL != "https://discord.com/api/webhooks/1/discord-SECRET-a" {
		t.Fatalf("%+v", hooks)
	}

	// a test message, and its failure in words without the URL
	url := r.server.URL + "/hook/test-SECRET"
	id, err := f.AddWebhook(ctx, "user:paul", "Ops", url, "json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.TestWebhook(ctx, "user:paul", id); err != nil {
		t.Fatal(err)
	}
	if p := r.to("/hook/test-SECRET"); len(p) != 1 || p[0].body["rule"] != "test" || p[0].body["fleet"] != "OpenCE Fleet" {
		t.Fatalf("%+v", p)
	}
	r.mu.Lock()
	r.status = http.StatusInternalServerError
	r.mu.Unlock()
	err = f.TestWebhook(ctx, "user:paul", id)
	if err == nil || !strings.Contains(err.Error(), "answered 500") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("%v", err)
	}
	w, _ := f.Store.GetWebhook(ctx, id)
	if !strings.Contains(w.LastStatus, "500") {
		t.Fatalf("%q", w.LastStatus)
	}
	r.server.Close()
	err = f.TestWebhook(ctx, "user:paul", id)
	if err == nil || !strings.Contains(err.Error(), "did not answer") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("%v", err)
	}
	if a, ok := audited(t, f, "webhook.test"); !ok || a.Actor != "user:paul" || a.Target != "Ops" {
		t.Fatalf("%+v", a)
	}
	if err := f.DeleteWebhook(ctx, "user:paul", id); err != nil {
		t.Fatal(err)
	}
	if e, ok := AsError(f.TestWebhook(ctx, "user:paul", id)); !ok || e.Code != "not_found" {
		t.Fatalf("%v", e)
	}
	noSecret(t, f, logs, "SECRET")
}

func TestAlertByHand(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	f.raiseAlert(ctx, "speed_hack", "ranked-1", "machine 3: kicked")
	open := openAlerts(t, f, "speed_hack")
	if len(open) != 1 {
		t.Fatalf("%+v", open)
	}
	if err := f.AcknowledgeAlert(ctx, "user:paul", open[0].ID); err != nil {
		t.Fatal(err)
	}
	if a, _ := f.Store.GetAlert(ctx, open[0].ID); a.AcknowledgedBy != "user:paul" || a.ResolvedAt != nil {
		t.Fatalf("%+v", a)
	}
	if err := f.ResolveAlertByID(ctx, "user:paul", open[0].ID); err != nil {
		t.Fatal(err)
	}
	if a, _ := f.Store.GetAlert(ctx, open[0].ID); a.ResolvedAt == nil {
		t.Fatalf("%+v", a)
	}
	for _, action := range []string{"alert.acknowledge", "alert.resolve"} {
		if a, ok := audited(t, f, action); !ok || a.Actor != "user:paul" {
			t.Errorf("%s: %+v", action, a)
		}
	}
	if e, ok := AsError(f.ResolveAlertByID(ctx, "user:paul", 99)); !ok || e.Code != "not_found" {
		t.Fatalf("%v", e)
	}
}

// A node offline for two minutes with servers is an alert, resolved when it
// is back.
func TestNodeOffline(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	clk := stop(f, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	id, token := node(t, f, "a", spec.DefaultNode())
	servers(t, f, profile(t, f, "Ranked"), 1, false, "a")
	a := connect(t, f, token, func(request) *agentproto.Reply { return &agentproto.Reply{OK: true} })
	until(t, "the node online", func() bool { return f.Hub.Online(id) })
	clk.add(time.Second)
	a.close()
	// (its last word is the disconnect's)
	until(t, "the node offline", func() bool {
		n, _ := f.Store.GetNode(ctx, id)
		return !f.Hub.Online(id) && n.LastSeen != nil && *n.LastSeen == clk.now().Unix()
	})
	clk.add(time.Minute)
	f.evaluate(ctx)
	if open := openAlerts(t, f, "node_offline"); len(open) != 0 {
		t.Fatalf("an alert after a minute: %+v", open)
	}
	clk.add(2 * time.Minute)
	f.evaluate(ctx)
	open := openAlerts(t, f, "node_offline")
	if len(open) != 1 || open[0].Target != "a" || !strings.Contains(open[0].Detail, "with 1 servers") {
		t.Fatalf("%+v", open)
	}
	connect(t, f, token, func(request) *agentproto.Reply { return &agentproto.Reply{OK: true} })
	until(t, "the alert resolved", func() bool { return len(openAlerts(t, f, "node_offline")) == 0 })
}
