package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/live"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// Rules is every alert rule, with its words.
var Rules = []struct{ ID, Name string }{
	{"node_offline", "A node offline for 2 minutes"},
	{"instance_crashed", "A server crashed (exit 1, 4, a signal)"},
	{"instance_crash_loop", "A server crash-looping (3 in 10 minutes)"},
	{"instance_blocked", "A server stopped by its configuration or game data (exit 2, 3)"},
	{"probe_failed", "A master's probe failed for 5 minutes"},
	{"publisher_pending", "A publisher key's application pending"},
	{"publisher_suspended", "A publisher suspended or revoked"},
	{"quota_exceeded", "More servers than a key's quota"},
	{"frame_time", "Frames over 33 ms for 5 minutes"},
	{"release_out", "A release out for a channel that follows latest"},
	{"fleet_update", "A release of the fleet out"},
	{"disk_low", "A node's disk under 10 % free"},
	{"speed_hack", "A speed hack"},
	{"update_failed", "An update that did not come up"},
	{"update_rolled_back", "A server put back on its build, pinned, after an update that did not come up"},
}

// RuleName is a rule's words.
func RuleName(id string) string {
	for _, r := range Rules {
		if r.ID == id {
			return r.Name
		}
	}
	return id
}

// raiseAlert opens an alert (or touches the open one: an open alert is not
// told twice), and tells the webhooks that subscribe to its rule.
func (f *Fleet) raiseAlert(ctx context.Context, rule, target, detail string) {
	f.alertsMu.Lock()
	defer f.alertsMu.Unlock()
	now := f.Now().Unix()
	if open, err := f.Store.GetOpenAlert(ctx, db.GetOpenAlertParams{Rule: rule, Target: target}); err == nil {
		f.Store.TouchAlert(ctx, db.TouchAlertParams{LastSeen: now, Detail: detail, ID: open.ID})
		return
	}
	id, err := f.Store.OpenAlert(ctx, db.OpenAlertParams{Rule: rule, Target: target, Detail: detail, FirstSeen: now, LastSeen: now})
	if err != nil {
		f.Log.Error("an alert was not kept", "rule", rule, "err", err)
		return
	}
	f.Log.Warn("alert", "rule", rule, "target", target, "detail", firstLine(detail))
	f.Live.Notify(liveChange("alert", rule))
	f.Live.Notify(liveChange("tiles", ""))
	go f.deliver(id, "open")
}

// resolveAlert resolves an open alert, and says so where it was told.
func (f *Fleet) resolveAlert(rule, target string) {
	ctx := context.Background()
	f.alertsMu.Lock()
	defer f.alertsMu.Unlock()
	open, err := f.Store.GetOpenAlert(ctx, db.GetOpenAlertParams{Rule: rule, Target: target})
	if err != nil {
		return
	}
	f.Store.ResolveAlert(ctx, db.ResolveAlertParams{ResolvedAt: ptrInt(f.Now().Unix()), ID: open.ID})
	f.Live.Notify(liveChange("alert", rule))
	f.Live.Notify(liveChange("tiles", ""))
	if open.Delivered != "" {
		go f.deliver(open.ID, "resolved")
	}
}

// AcknowledgeAlert marks an alert seen (it stays open until its condition
// clears).
func (f *Fleet) AcknowledgeAlert(ctx context.Context, actor string, id int64) error {
	if err := f.Store.AcknowledgeAlert(ctx, db.AcknowledgeAlertParams{AcknowledgedBy: actor, ID: id}); err != nil {
		return err
	}
	f.Audit(ctx, actor, "alert.acknowledge", strconv.FormatInt(id, 10), nil)
	f.Live.Notify(liveChange("alert", ""))
	return nil
}

// ResolveAlertByID resolves an alert by hand.
func (f *Fleet) ResolveAlertByID(ctx context.Context, actor string, id int64) error {
	a, err := f.Store.GetAlert(ctx, id)
	if err != nil {
		return NotFound("the alert")
	}
	f.resolveAlert(a.Rule, a.Target)
	f.Audit(ctx, actor, "alert.resolve", strconv.FormatInt(id, 10), nil)
	return nil
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// Webhook is a webhook with its URL opened.
type Webhook struct {
	db.Webhook
	URL   string
	Rules []string
}

// Webhooks lists the webhooks (their URLs are secrets: the pages show
// their host alone).
func (f *Fleet) Webhooks(ctx context.Context) ([]Webhook, error) {
	rows, err := f.Store.ListWebhooks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Webhook, len(rows))
	for i, r := range rows {
		u, _ := f.Key.OpenString(r.UrlEnc, "webhook:"+strconv.FormatInt(r.ID, 10))
		out[i] = Webhook{Webhook: r, URL: u, Rules: strings.Split(r.Rules, ",")}
	}
	return out, nil
}

// WebhookHost is a webhook's host, which the pages show (the URL holds its
// token).
func WebhookHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(a URL)"
	}
	return u.Host
}

// AddWebhook keeps a webhook (Discord's, or a generic JSON one) for rules
// ("*": all).
func (f *Fleet) AddWebhook(ctx context.Context, actor, label, raw, kind string, rules []string) (int64, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return 0, Refuse(http.StatusBadRequest, "schema", "a webhook is an https:// URL")
	}
	if kind != "discord" && kind != "json" {
		kind = "discord"
		if !strings.Contains(u.Host, "discord") {
			kind = "json"
		}
	}
	if len(rules) == 0 {
		rules = []string{"*"}
	}
	if strings.TrimSpace(label) == "" {
		label = u.Host
	}
	id, err := f.Store.CreateWebhook(ctx, db.CreateWebhookParams{Label: label, UrlEnc: []byte{0}, Kind: kind,
		Rules: strings.Join(rules, ","), CreatedAt: f.Now().Unix()})
	if err != nil {
		return 0, err
	}
	// (sealed with its id, which only the insert gives)
	f.Store.DB.ExecContext(ctx, `UPDATE webhooks SET url_enc = ? WHERE id = ?`,
		f.Key.SealString(u.String(), "webhook:"+strconv.FormatInt(id, 10)), id)
	f.Audit(ctx, actor, "webhook.add", label, map[string]any{"host": u.Host, "kind": kind, "rules": rules})
	return id, nil
}

// DeleteWebhook removes a webhook.
func (f *Fleet) DeleteWebhook(ctx context.Context, actor string, id int64) error {
	w, err := f.Store.GetWebhook(ctx, id)
	if err != nil {
		return NotFound("the webhook")
	}
	if err := f.Store.DeleteWebhook(ctx, id); err != nil {
		return err
	}
	f.Audit(ctx, actor, "webhook.delete", w.Label, nil)
	return nil
}

// TestWebhook sends a webhook a test message.
func (f *Fleet) TestWebhook(ctx context.Context, actor string, id int64) error {
	hooks, err := f.Webhooks(ctx)
	if err != nil {
		return err
	}
	for _, h := range hooks {
		if h.ID == id {
			err := f.post(ctx, h, db.Alert{Rule: "test", Target: "the fleet", Detail: "A test of this webhook, from " +
				f.Name(ctx) + ".", FirstSeen: f.Now().Unix()}, "open")
			status := "ok"
			if err != nil {
				status = err.Error()
			}
			f.Store.UpdateWebhookStatus(ctx, db.UpdateWebhookStatusParams{LastStatus: status, ID: id})
			f.Audit(ctx, actor, "webhook.test", h.Label, status)
			return err
		}
	}
	return NotFound("the webhook")
}

func (f *Fleet) deliver(id int64, state string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a, err := f.Store.GetAlert(ctx, id)
	if err != nil {
		return
	}
	hooks, err := f.Webhooks(ctx)
	if err != nil {
		return
	}
	var told []string
	for _, h := range hooks {
		if !subscribes(h.Rules, a.Rule) {
			continue
		}
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if err = f.post(ctx, h, a, state); err == nil {
				break
			}
			time.Sleep(time.Duration(attempt+1) * 3 * time.Second)
		}
		status := "ok"
		if err != nil {
			status = err.Error()
			f.Log.Warn("a webhook failed", "webhook", h.Label, "host", WebhookHost(h.URL), "err", err)
		} else {
			told = append(told, h.Label)
		}
		f.Store.UpdateWebhookStatus(ctx, db.UpdateWebhookStatusParams{LastStatus: status, ID: h.ID})
	}
	if len(told) > 0 && state == "open" {
		f.Store.SetAlertDelivered(ctx, db.SetAlertDeliveredParams{Delivered: strings.Join(told, ", "), ID: id})
	}
}

func subscribes(rules []string, rule string) bool {
	for _, r := range rules {
		if r == "*" || r == rule {
			return true
		}
	}
	return false
}

// post sends an alert to a webhook: Discord's embed, or a JSON object.
func (f *Fleet) post(ctx context.Context, h Webhook, a db.Alert, state string) error {
	link := ""
	if f.PublicURL != "" {
		link = f.PublicURL + "/alerts"
		if a.Target != "" && !strings.Contains(a.Target, "@") && !strings.Contains(a.Target, " ") {
			if _, err := f.Store.GetInstance(ctx, a.Target); err == nil {
				link = f.PublicURL + "/servers/" + url.PathEscape(a.Target)
			}
		}
	}
	var body any
	if h.Kind == "discord" {
		colour := 0xf87171
		title := RuleName(a.Rule) + ": " + a.Target
		if state == "resolved" {
			colour, title = 0x4ade80, "Resolved: "+title
		}
		detail := a.Detail
		if len(detail) > 1800 {
			detail = detail[:1800] + "…"
		}
		embed := map[string]any{"title": truncate(title, 250), "description": detail, "color": colour,
			"timestamp": time.Unix(a.FirstSeen, 0).UTC().Format(time.RFC3339),
			"footer":    map[string]string{"text": f.Name(ctx)}}
		if link != "" {
			embed["url"] = link
		}
		body = map[string]any{"username": "OpenCE Fleet", "embeds": []any{embed},
			"allowed_mentions": map[string]any{"parse": []string{}}}
	} else {
		body = map[string]any{"fleet": f.Name(ctx), "rule": a.Rule, "rule_name": RuleName(a.Rule), "target": a.Target,
			"detail": a.Detail, "state": state, "time": a.FirstSeen, "url": link}
	}
	data, _ := json.Marshal(body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := f.HTTP.Do(request)
	if err != nil {
		// (the error names the URL, whose token is a secret)
		return fmt.Errorf("%s did not answer", WebhookHost(h.URL))
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return fmt.Errorf("%s answered %s", WebhookHost(h.URL), response.Status)
	}
	return nil
}

// conditions that must last before they alert
type lastingConditions struct {
	sync.Mutex
	since map[string]time.Time
}

func (f *Fleet) holds(key string, on bool, now time.Time, d time.Duration) bool {
	lasting := f.lasting
	lasting.Lock()
	defer lasting.Unlock()
	if !on {
		delete(lasting.since, key)
		return false
	}
	since, ok := lasting.since[key]
	if !ok {
		lasting.since[key] = now
		return false
	}
	return now.Sub(since) >= d
}

// alertsLoop evaluates the rules over the live state every 30 seconds.
func (f *Fleet) alertsLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
		f.evaluate(ctx)
	}
}

func (f *Fleet) evaluate(ctx context.Context) {
	now := f.Now()
	nodes, _ := f.Nodes(ctx)
	instances, _ := f.Store.ListInstances(ctx)
	count := map[int64]int{}
	for _, i := range instances {
		count[i.NodeID]++
	}
	for _, n := range nodes {
		online := f.Hub.Online(n.ID)
		if !online && count[n.ID] > 0 && n.LastSeen != nil && now.Sub(time.Unix(*n.LastSeen, 0)) > 2*time.Minute {
			f.raiseAlert(ctx, "node_offline", n.Name, fmt.Sprintf("%s has been offline since %s, with %d servers",
				n.Name, time.Unix(*n.LastSeen, 0).In(f.Location()).Format("15:04"), count[n.ID]))
		} else if online {
			f.resolveAlert("node_offline", n.Name)
		}
	}
	latest, _ := f.Store.GetLatestRelease(ctx)
	behind := false
	for slug, l := range f.Live.Instances() {
		if l.Status == nil {
			f.holds("probe:"+slug, false, now, 0)
			f.holds("frame:"+slug, false, now, 0)
			continue
		}
		failing := ""
		for _, m := range l.Status.Masters {
			if m.Probe != "" && m.Probe != "ok" && m.Probe != "pending" {
				failing = fmt.Sprintf("%s: probe %s %s", m.URL, m.Probe, m.Message)
			}
		}
		if f.holds("probe:"+slug, failing != "", now, 5*time.Minute) {
			n, _ := f.Store.GetInstance(ctx, slug)
			f.raiseAlert(ctx, "probe_failed", slug, failing+fmt.Sprintf(" (open UDP %d on its node, and set the node's public address if it is behind a NAT)", n.TunnelPort))
		} else if failing == "" {
			f.resolveAlert("probe_failed", slug)
		}
		slow := l.Status.FrameMsMax > 33
		if f.holds("frame:"+slug, slow, now, 5*time.Minute) {
			f.raiseAlert(ctx, "frame_time", slug, fmt.Sprintf("the longest frame of the last minute is %.1f ms", l.Status.FrameMsMax))
		} else if !slow {
			f.resolveAlert("frame_time", slug)
		}
		if latest.Build != "" && l.Build != "" && l.Build != latest.Build {
			behind = true
		}
	}
	if !behind && latest.Build != "" {
		f.resolveAlert("release_out", latest.Build)
	}
	// (a speed hack's alert is a moment's: resolved an hour on)
	open, _ := f.Store.ListOpenAlerts(ctx)
	for _, a := range open {
		if a.Rule == "speed_hack" && now.Unix()-a.LastSeen > 3600 {
			f.resolveAlert(a.Rule, a.Target)
		}
	}
}

// checkDisk raises disk_low from a heartbeat.
func (f *Fleet) checkDisk(node int64, h agentproto.Heartbeat) {
	n := f.Live.Node(node)
	if n == nil || h.DiskTotal == 0 {
		return
	}
	free := float64(h.DiskTotal-h.DiskUsed) / float64(h.DiskTotal)
	if free < 0.10 {
		f.raiseAlert(context.Background(), "disk_low", n.Name, fmt.Sprintf("%.0f %% free on %s's node folder", free*100, n.Name))
	} else if free > 0.12 {
		f.resolveAlert("disk_low", n.Name)
	}
}

// FleetUpdate raises the alert of a release of the fleet out.
func (f *Fleet) FleetUpdate(version string) {
	f.raiseAlert(context.Background(), "fleet_update", version, "opence-fleet "+version+" is out: update on the Settings page")
	f.Live.AddFeed(live.FeedItem{Kind: "info", Event: "fleet_update", Text: "opence-fleet " + version + " is out"})
}
