package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/master"
	"github.com/OpenCommunityEdition/opence-fleet/internal/serverapi"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// DefaultRotation is the server's own rotation, played by an instance whose
// profile names none.
const DefaultRotation = "bloodgulch:ctf, sidewinder:team_slayer, prisoner:slayer"

// Overrides is what an instance sets over its profile.
type Overrides struct {
	Settings map[string]any `json:"settings,omitempty"`
	Rotation *string        `json:"rotation,omitempty"`
	InitTxt  *string        `json:"init_txt,omitempty"`
}

// Instance is an instance with its overrides read.
type Instance struct {
	db.Instance
	Overrides Overrides
	Groups    []string
}

func readInstance(i db.Instance) Instance {
	out := Instance{Instance: i}
	json.Unmarshal([]byte(i.Overrides), &out.Overrides)
	if out.Overrides.Settings == nil {
		out.Overrides.Settings = map[string]any{}
	}
	return out
}

// Instances lists every instance, with its groups.
func (f *Fleet) Instances(ctx context.Context) ([]Instance, error) {
	rows, err := f.Store.ListInstances(ctx)
	if err != nil {
		return nil, err
	}
	groups, _ := f.Store.ListInstanceGroups(ctx)
	bySlug := map[string][]string{}
	for _, g := range groups {
		bySlug[g.Slug] = append(bySlug[g.Slug], g.GroupName)
	}
	out := make([]Instance, len(rows))
	for i, r := range rows {
		out[i] = readInstance(r)
		out[i].Groups = bySlug[r.Slug]
	}
	return out, nil
}

// Instance finds an instance.
func (f *Fleet) Instance(ctx context.Context, slug string) (Instance, error) {
	row, err := f.Store.GetInstance(ctx, slug)
	if err != nil {
		return Instance{}, NotFound("server " + slug)
	}
	i := readInstance(row)
	groups, _ := f.Store.ListInstanceGroups(ctx)
	for _, g := range groups {
		if g.Slug == slug {
			i.Groups = append(i.Groups, g.GroupName)
		}
	}
	return i, nil
}

// ResolveChannel is the build a channel names now ("latest" is the release
// marked latest).
func (f *Fleet) ResolveChannel(ctx context.Context, channel string) (string, error) {
	if channel == "" {
		channel = f.Setting(ctx, "default_channel")
	}
	if channel == "latest" || channel == "" {
		r, err := f.Store.GetLatestRelease(ctx)
		if err != nil {
			return "", Refuse(http.StatusConflict, "no_release", "no release is known yet: fetch one on the Releases page")
		}
		return r.Build, nil
	}
	if _, err := f.Store.GetRelease(ctx, channel); err != nil {
		return "", Refuse(http.StatusConflict, "no_release", "release %s is not known: fetch it on the Releases page", channel)
	}
	return channel, nil
}

// Channel is the channel an instance follows (its own, its profile's, the
// fleet's default).
func (f *Fleet) Channel(ctx context.Context, i db.Instance, p *Profile) string {
	if i.Channel != "" {
		return i.Channel
	}
	if p != nil && p.Channel != "" {
		return p.Channel
	}
	return f.Setting(ctx, "default_channel")
}

// RenderInstance makes an instance's desired state.
func (f *Fleet) RenderInstance(ctx context.Context, row db.Instance, n Node, library []db.Library) (spec.Instance, error) {
	i := readInstance(row)
	var p *Profile
	if row.ProfileID != nil {
		profile, err := f.Profile(ctx, *row.ProfileID)
		if err == nil {
			p = &profile
		}
	}
	profileSettings := map[string]any{}
	rotation, region, initTxt := "", "", ""
	gametypes := map[string]string{}
	if p != nil {
		profileSettings = p.Settings
		rotation, region, initTxt = p.Rotation, p.Region, p.InitTxt
		for name, text := range p.Gametypes {
			gametypes[name] = text
		}
	}
	if i.Overrides.Rotation != nil {
		rotation = *i.Overrides.Rotation
	}
	if i.Overrides.InitTxt != nil {
		initTxt = *i.Overrides.InitTxt
	}
	settings := spec.Merge(profileSettings, i.Overrides.Settings)
	build, err := f.ResolveChannel(ctx, f.Channel(ctx, row, p))
	if err != nil {
		return spec.Instance{}, err
	}
	web, err := f.Key.OpenString(row.WebPasswordEnc, "instance:"+row.Slug+":web_password")
	if err != nil {
		return spec.Instance{}, err
	}
	owner, _ := f.Key.OpenString(row.OwnerPasswordEnc, "instance:"+row.Slug+":owner_password")
	password := ""
	if p != nil {
		switch p.PasswordPolicy {
		case "fixed":
			password, _ = f.Key.OpenString(p.PasswordEnc, fmt.Sprintf("profile:%d:password", p.ID))
		case "per_instance":
			password, _ = f.Key.OpenString(row.PasswordEnc, "instance:"+row.Slug+":password")
		}
	}
	if v, ok := settings["server.password"]; ok && spec.SettingText(v) != "" {
		password = spec.SettingText(v)
	}
	seed := ""
	if key, err := f.instanceKey(ctx, row, p); err == nil && key != nil {
		seed = key.seed
	}
	effective := rotation
	if strings.TrimSpace(effective) == "" {
		effective = DefaultRotation
	}
	needs, extra, saved := needs(effective, library)
	for name, text := range extra {
		if _, ok := gametypes[name]; !ok {
			gametypes[name] = text
		}
	}
	_ = saved
	in := spec.Input{Slug: row.Slug, Name: row.Name, Generation: row.Generation, Build: build, Running: row.Running != 0,
		TunnelPort: int(row.TunnelPort), WebPort: int(row.WebPort), Loopback: n.Settings.LoopbackBase + int(row.LoopbackIndex),
		Settings: settings, Rotation: rotation, Region: region, InitTxt: initTxt, Gametypes: gametypes,
		Password: password, WebPassword: web, OwnerPassword: owner, PublisherSeed: seed,
		PublicAddress: n.Settings.PublicAddress, LogAddresses: f.Setting(ctx, "log_addresses") == "true",
		WebUI: n.Settings.WebUI, Needs: needs}
	return spec.Render(in), nil
}

// needs is the library items a rotation plays: its maps (ui.map always;
// the resource maps with a Custom Edition map), its gametype files' texts,
// and the saved gametypes it names.
func needs(rotation string, library []db.Library) ([]string, map[string]string, []string) {
	byName := map[string]db.Library{}
	for _, item := range library {
		byName[item.Kind+":"+strings.ToLower(item.Name)] = item
	}
	set := map[string]bool{}
	add := func(kind, name string) bool {
		if item, ok := byName[kind+":"+strings.ToLower(name)]; ok {
			set[item.Sha256] = true
			return true
		}
		return false
	}
	add("map", "ui")
	custom := false
	gametypes := map[string]string{}
	var saved []string
	for _, entry := range spec.RotationEntries(rotation) {
		m := spec.RotationMap(entry)
		switch {
		case strings.HasPrefix(strings.ToLower(m), `custom_maps\`):
			if add("custom_map", m[len(`custom_maps\`):]) {
				custom = true
			}
		case strings.Contains(m, `\`):
			add("map", m[strings.LastIndexByte(m, '\\')+1:])
		default:
			if !add("map", m) && add("custom_map", m) {
				custom = true
			}
		}
		g := spec.RotationGametype(entry)
		if name, ok := strings.CutPrefix(g, "file="); ok {
			if add("saved_gametype", name) {
				saved = append(saved, name)
			}
		} else if item, ok := byName["gametype:"+strings.ToLower(g)]; ok {
			if text, ok := item.Meta, true; ok {
				var meta struct {
					Text string `json:"text"`
				}
				json.Unmarshal([]byte(text), &meta)
				if meta.Text != "" {
					gametypes[item.Name] = meta.Text
				}
			}
		}
	}
	if custom {
		for _, r := range []string{"bitmaps", "sounds", "loc"} {
			add("resource", r)
		}
	}
	out := make([]string, 0, len(set))
	for sha := range set {
		out = append(out, sha)
	}
	sort.Strings(out)
	return out, gametypes, saved
}

// CreateRequest is the New servers form: profile × nodes × count.
type CreateRequest struct {
	ProfileID int64
	// the nodes to spread the servers over (one: all there)
	Nodes   []string
	Count   int
	Pattern string
	Groups  []string
	Running bool
	Channel string
}

// CreateInstances makes servers from a profile: names from the pattern,
// slugs from the names, ports and loopback addresses from each node's
// ranges. The warnings are what does not stop them (a key's quota).
func (f *Fleet) CreateInstances(ctx context.Context, actor string, r CreateRequest) ([]string, []string, error) {
	if r.Count < 1 || r.Count > 100 {
		return nil, nil, Refuse(http.StatusBadRequest, "schema", "1 to 100 servers at once")
	}
	profile, err := f.Profile(ctx, r.ProfileID)
	if err != nil {
		return nil, nil, err
	}
	if len(r.Nodes) == 0 {
		return nil, nil, Refuse(http.StatusBadRequest, "schema", "choose a node")
	}
	var nodes []Node
	for _, name := range r.Nodes {
		n, err := f.NodeByName(ctx, name)
		if err != nil {
			return nil, nil, err
		}
		nodes = append(nodes, n)
	}
	pattern := strings.TrimSpace(r.Pattern)
	if pattern == "" {
		pattern = profile.Name + " #{n}"
	}
	if r.Channel != "" {
		if _, err := f.ResolveChannel(ctx, r.Channel); err != nil {
			return nil, nil, err
		}
	}
	var created, warnings []string
	err = f.Store.Tx(ctx, func(q *db.Queries) error {
		existing, err := q.ListInstances(ctx)
		if err != nil {
			return err
		}
		taken := map[string]bool{}
		load := map[int64]int{}
		used := map[int64]map[string]map[int64]bool{}
		for _, i := range existing {
			taken[i.Slug] = true
			load[i.NodeID]++
			if used[i.NodeID] == nil {
				used[i.NodeID] = map[string]map[int64]bool{"tunnel": {}, "web": {}, "loopback": {}}
			}
			used[i.NodeID]["tunnel"][i.TunnelPort] = true
			used[i.NodeID]["web"][i.WebPort] = true
			used[i.NodeID]["loopback"][i.LoopbackIndex] = true
		}
		number := 1
		for k := 0; k < r.Count; k++ {
			name, slug := "", ""
			for {
				name = spec.Pattern(pattern, number)
				slug = spec.Slug(name)
				number++
				if slug != "" && !taken[slug] {
					break
				}
				if number > 10000 {
					return Refuse(http.StatusConflict, "taken", "no free name for the pattern %q", pattern)
				}
			}
			if !master.ValidInstance(slug) {
				return Refuse(http.StatusBadRequest, "schema", "%q makes no instance name (a-z, 0-9, '-' and '_')", name)
			}
			// (the least loaded of the chosen nodes)
			sort.SliceStable(nodes, func(a, b int) bool { return load[nodes[a].ID] < load[nodes[b].ID] })
			n := nodes[0]
			if used[n.ID] == nil {
				used[n.ID] = map[string]map[int64]bool{"tunnel": {}, "web": {}, "loopback": {}}
			}
			tunnel := free(n.Settings.TunnelPorts, used[n.ID]["tunnel"])
			web := free(n.Settings.WebPorts, used[n.ID]["web"])
			loopback := free(spec.PortRange{First: 0, Last: 250 - n.Settings.LoopbackBase}, used[n.ID]["loopback"])
			if tunnel < 0 || web < 0 || loopback < 0 {
				return Refuse(http.StatusConflict, "full", "node %s has no free port left in its ranges (tunnel %s, web %s)",
					n.Name, n.Settings.TunnelPorts, n.Settings.WebPorts)
			}
			used[n.ID]["tunnel"][tunnel], used[n.ID]["web"][web], used[n.ID]["loopback"][loopback] = true, true, true
			load[n.ID]++
			taken[slug] = true
			params := db.CreateInstanceParams{Slug: slug, Name: name, NodeID: n.ID, ProfileID: &profile.ID, Overrides: "{}",
				TunnelPort: tunnel, WebPort: web, LoopbackIndex: loopback,
				WebPasswordEnc:   f.Key.SealString(Password(24), "instance:"+slug+":web_password"),
				OwnerPasswordEnc: f.Key.SealString(Password(24), "instance:"+slug+":owner_password"),
				Channel:          r.Channel, Running: boolInt(r.Running), Kind: "managed", CreatedBy: actor, CreatedAt: f.Now().Unix()}
			if profile.PasswordPolicy == "per_instance" {
				params.PasswordEnc = f.Key.SealString(Password(10), "instance:"+slug+":password")
			}
			if err := q.CreateInstance(ctx, params); err != nil {
				return err
			}
			for _, g := range r.Groups {
				g = groupName(g)
				if g == "" {
					continue
				}
				q.UpsertGroup(ctx, db.UpsertGroupParams{Name: g})
				q.AddInstanceGroup(ctx, db.AddInstanceGroupParams{Slug: slug, GroupName: g})
			}
			created = append(created, slug)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	for _, slug := range created {
		f.Audit(ctx, actor, "instance.create", slug, map[string]any{"profile": profile.Name})
	}
	pushed := map[int64]bool{}
	for _, n := range nodes {
		if !pushed[n.ID] {
			pushed[n.ID] = true
			f.Push(n.ID)
		}
	}
	if w := f.quotaWarning(ctx, profile.KeyID); w != "" {
		warnings = append(warnings, w)
	}
	if _, err := f.ResolveChannel(ctx, r.Channel); err != nil {
		warnings = append(warnings, err.Error())
	}
	f.Live.Notify(liveChange("tiles", ""))
	return created, warnings, nil
}

func free(r spec.PortRange, used map[int64]bool) int64 {
	for p := r.First; p <= r.Last; p++ {
		if !used[int64(p)] {
			return int64(p)
		}
	}
	return -1
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func groupName(text string) string {
	return spec.Slug(text)
}

// UpdateInstance changes an instance's name, profile, overrides, key,
// channel and notes, and pushes its node's spec (the server takes what it
// can live; the rest waits for a restart).
type InstanceUpdate struct {
	Name      string
	ProfileID *int64
	Overrides Overrides
	KeyID     *string
	Channel   string
	Notes     string
}

// UpdateInstance applies an InstanceUpdate.
func (f *Fleet) UpdateInstance(ctx context.Context, actor, slug string, u InstanceUpdate) error {
	i, err := f.Instance(ctx, slug)
	if err != nil {
		return err
	}
	for name, value := range u.Overrides.Settings {
		v, err := spec.Normalize(name, value)
		if err != nil {
			return Refuse(http.StatusBadRequest, "schema", "%v", err)
		}
		u.Overrides.Settings[name] = v
	}
	if u.Channel != "" {
		if _, err := f.ResolveChannel(ctx, u.Channel); err != nil {
			return err
		}
	}
	if strings.TrimSpace(u.Name) == "" {
		u.Name = i.Name
	}
	data, _ := json.Marshal(u.Overrides)
	if err := f.Store.UpdateInstance(ctx, db.UpdateInstanceParams{Name: strings.TrimSpace(u.Name), ProfileID: u.ProfileID,
		Overrides: string(data), KeyID: u.KeyID, Channel: u.Channel, Notes: u.Notes, Slug: slug}); err != nil {
		return err
	}
	f.Audit(ctx, actor, "instance.update", slug, map[string]any{"name": u.Name, "overrides": redactSettings(u.Overrides.Settings),
		"channel": u.Channel})
	f.Push(i.NodeID)
	f.Live.Notify(liveChange("instance", slug))
	return nil
}

// redactSettings is settings for the audit log: a password as "(changed)".
func redactSettings(settings map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range settings {
		if s, ok := spec.Lookup(k); ok && s.Secret {
			v = "(changed)"
		}
		out[k] = v
	}
	return out
}

// SetOverride sets (or, with value nil, resets to the profile) one setting
// of an instance.
func (f *Fleet) SetOverride(ctx context.Context, actor, slug, name string, value any) error {
	i, err := f.Instance(ctx, slug)
	if err != nil {
		return err
	}
	if value == nil {
		delete(i.Overrides.Settings, name)
	} else {
		v, err := spec.Normalize(name, value)
		if err != nil {
			return Refuse(http.StatusBadRequest, "schema", "%v", err)
		}
		i.Overrides.Settings[name] = v
	}
	return f.UpdateInstance(ctx, actor, slug, InstanceUpdate{Name: i.Name, ProfileID: i.ProfileID, Overrides: i.Overrides,
		KeyID: i.KeyID, Channel: i.Channel, Notes: i.Notes})
}

// SetRunning starts or stops an instance (its spec's running).
func (f *Fleet) SetRunning(ctx context.Context, actor, slug string, running bool) error {
	i, err := f.Instance(ctx, slug)
	if err != nil {
		return err
	}
	if err := f.Store.SetInstanceRunning(ctx, db.SetInstanceRunningParams{Running: boolInt(running), Slug: slug}); err != nil {
		return err
	}
	action := "instance.stop"
	if running {
		action = "instance.start"
	}
	f.Audit(ctx, actor, action, slug, nil)
	if err := f.PushNow(ctx, i.NodeID); err != nil {
		f.Log.Warn("a spec was not sent", "err", err)
	}
	if running {
		// (a server held stopped by its exit, 2, 3 or an operator's quit, is
		// started again)
		f.control(ctx, i.NodeID, agentproto.Control{Slug: slug, Action: agentproto.Start})
	}
	return nil
}

// Restart restarts an instance (the container made afresh from its spec).
func (f *Fleet) Restart(ctx context.Context, actor, slug string) error {
	i, err := f.Instance(ctx, slug)
	if err != nil {
		return err
	}
	if i.Running == 0 {
		return f.SetRunning(ctx, actor, slug, true)
	}
	f.Audit(ctx, actor, "instance.restart", slug, nil)
	return f.control(ctx, i.NodeID, agentproto.Control{Slug: slug, Action: agentproto.Restart})
}

// Drain stops (then, restart, starts again) an instance once its game ends
// or it is empty, with a notice, within a deadline.
func (f *Fleet) Drain(ctx context.Context, actor, slug, after, notice string, deadline time.Duration, restart bool) error {
	i, err := f.Instance(ctx, slug)
	if err != nil {
		return err
	}
	then := "stop"
	if restart {
		then = "restart"
	}
	f.Audit(ctx, actor, "instance.drain", slug, map[string]any{"after": after, "then": then, "notice": notice})
	if !restart {
		// (once stopped it stays stopped)
		defer func() {
			go func() {
				f.waitState(context.Background(), slug, deadline+time.Minute, "stopped", "crashed", "blocked")
				f.Store.SetInstanceRunning(context.Background(), db.SetInstanceRunningParams{Running: 0, Slug: slug})
				f.Push(i.NodeID)
			}()
		}()
	}
	return f.control(ctx, i.NodeID, agentproto.Control{Slug: slug, Action: agentproto.Drain, After: after, Notice: notice,
		Deadline: int(deadline / time.Second), Then: then})
}

// waitState waits for an instance to reach one of the states.
func (f *Fleet) waitState(ctx context.Context, slug string, timeout time.Duration, states ...string) string {
	deadline := time.After(timeout)
	changes, cancel := f.Live.Subscribe()
	defer cancel()
	for {
		if l := f.Live.Instance(slug); l != nil {
			for _, s := range states {
				if l.State == s {
					return s
				}
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

func (f *Fleet) control(ctx context.Context, node int64, c agentproto.Control) error {
	call, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	reply, err := f.Hub.Request(call, node, agentproto.TControl, c)
	if err != nil {
		return Refuse(http.StatusServiceUnavailable, "offline", "the node did not take it: %v", err)
	}
	if !reply.OK && reply.Error != nil {
		return Refuse(http.StatusBadGateway, reply.Error.Code, "%s", reply.Error.Message)
	}
	return nil
}

// Control sends an instance's agent a control request and returns its
// reply.
func (f *Fleet) Control(ctx context.Context, slug string, c agentproto.Control) (agentproto.Reply, error) {
	i, err := f.Instance(ctx, slug)
	if err != nil {
		return agentproto.Reply{}, err
	}
	c.Slug = slug
	call, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	reply, err := f.Hub.Request(call, i.NodeID, agentproto.TControl, c)
	if err != nil {
		return reply, Refuse(http.StatusServiceUnavailable, "offline", "the node did not answer: %v", err)
	}
	return reply, nil
}

// DeleteInstance removes an instance: its server stopped, its container
// removed, its folder kept aside on its node for 7 days.
func (f *Fleet) DeleteInstance(ctx context.Context, actor, slug string) error {
	i, err := f.Instance(ctx, slug)
	if err != nil {
		return err
	}
	if err := f.Store.DeleteInstance(ctx, slug); err != nil {
		return err
	}
	f.Hub.Send(i.NodeID, agentproto.TForget, agentproto.Forget{Slug: slug})
	f.Live.Forget(slug)
	f.forgetOwners()
	f.Audit(ctx, actor, "instance.delete", slug, nil)
	f.Push(i.NodeID)
	return nil
}

// RotatePassword makes the instance a new web password (the agent's
// alone); the server takes it at its next start, which it needs.
func (f *Fleet) RotatePassword(ctx context.Context, actor, slug string) error {
	i, err := f.Instance(ctx, slug)
	if err != nil {
		return err
	}
	if err := f.Store.SetInstanceWebPassword(ctx, db.SetInstanceWebPasswordParams{
		WebPasswordEnc: f.Key.SealString(Password(24), "instance:"+slug+":web_password"), Slug: slug}); err != nil {
		return err
	}
	f.Audit(ctx, actor, "instance.rotate_password", slug, "changed")
	f.Push(i.NodeID)
	return nil
}

// JoinPassword is an instance's join password (the profile's policy), for
// the admins' invite.
func (f *Fleet) JoinPassword(ctx context.Context, slug string) string {
	i, err := f.Instance(ctx, slug)
	if err != nil {
		return ""
	}
	if v, ok := i.Overrides.Settings["server.password"]; ok {
		return spec.SettingText(v)
	}
	if i.ProfileID == nil {
		return ""
	}
	p, err := f.Profile(ctx, *i.ProfileID)
	if err != nil {
		return ""
	}
	switch p.PasswordPolicy {
	case "fixed":
		pw, _ := f.Key.OpenString(p.PasswordEnc, fmt.Sprintf("profile:%d:password", p.ID))
		return pw
	case "per_instance":
		pw, _ := f.Key.OpenString(i.PasswordEnc, "instance:"+slug+":password")
		return pw
	}
	return ""
}

// API sends a request to an instance's API through its agent: the answer's
// status and body as the server gave them.
func (f *Fleet) API(ctx context.Context, slug, method, path string, body any) (int, json.RawMessage, error) {
	i, err := f.Instance(ctx, slug)
	if err != nil {
		return 0, nil, err
	}
	var raw json.RawMessage
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	if i.Kind == "external" {
		return f.externalAPI(ctx, i, method, path, raw)
	}
	call, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	reply, err := f.Hub.Request(call, i.NodeID, agentproto.TAPI, agentproto.API{Slug: slug, Method: method, Path: path, Body: raw})
	if err != nil {
		return 0, nil, Refuse(http.StatusServiceUnavailable, "offline", "%s's node is offline", slug)
	}
	if reply.Error != nil && reply.Status == 0 {
		return 0, nil, Refuse(http.StatusServiceUnavailable, reply.Error.Code, "%s", reply.Error.Message)
	}
	return reply.Status, reply.Body, nil
}

func (f *Fleet) externalAPI(ctx context.Context, i Instance, method, path string, body json.RawMessage) (int, json.RawMessage, error) {
	password, _ := f.Key.OpenString(i.ExternalPasswordEnc, "instance:"+i.Slug+":external_password")
	client := &serverapi.Client{Base: strings.TrimRight(i.ExternalUrl, "/"), Password: password, By: "opence-fleet", HTTP: f.HTTP}
	status, answer, err := client.Do(ctx, method, path, body)
	if err != nil {
		return 0, nil, Refuse(http.StatusBadGateway, "unreachable", "%s did not answer: %v", i.ExternalUrl, err)
	}
	return status, answer, nil
}

// Call is API with the answer decoded (a 2xx) or the server's refusal as
// an *Error with its code (pinned, rejected, changed...).
func (f *Fleet) Call(ctx context.Context, slug, method, path string, body, out any) error {
	status, answer, err := f.API(ctx, slug, method, path, body)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		var e struct {
			Error struct {
				Code     string                    `json:"code"`
				Message  string                    `json:"message"`
				Rejected []serverapi.RejectedEntry `json:"rejected"`
				Name     string                    `json:"name"`
			} `json:"error"`
			Lines []string `json:"lines"`
		}
		json.Unmarshal(answer, &e)
		message := e.Error.Message
		if len(e.Error.Rejected) > 0 {
			var parts []string
			for _, r := range e.Error.Rejected {
				parts = append(parts, r.Entry+": "+r.Reason)
			}
			message += " (" + strings.Join(parts, "; ") + ")"
		}
		if len(e.Lines) > 0 && e.Error.Code == "failed" {
			message = strings.Join(e.Lines, " ")
		}
		if e.Error.Code == "changed" && e.Error.Name != "" {
			message = "the slot holds " + e.Error.Name + " now: nothing done"
		}
		if message == "" {
			message = "the server answered " + strconv.Itoa(status)
		}
		return &Error{Status: status, Code: e.Error.Code, Message: message}
	}
	if out != nil {
		return json.Unmarshal(answer, out)
	}
	return nil
}

// Command runs a server command for a fleet role: the owner's on the
// server's standard input (its owner role), the others through the API.
func (f *Fleet) Command(ctx context.Context, actor, slug, line, role string) ([]string, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, Refuse(http.StatusBadRequest, "schema", "an empty command")
	}
	if actor != "auto" {
		f.Audit(ctx, actor, "instance.command", slug, redactCommand(line))
	}
	if role == "owner" {
		i, err := f.Instance(ctx, slug)
		if err != nil {
			return nil, err
		}
		call, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		reply, err := f.Hub.Request(call, i.NodeID, agentproto.TConsole, agentproto.Console{Slug: slug, Line: line})
		if err != nil {
			return nil, Refuse(http.StatusServiceUnavailable, "offline", "%s's node is offline", slug)
		}
		if reply.Error != nil {
			return reply.Lines, Refuse(http.StatusBadGateway, reply.Error.Code, "%s", reply.Error.Message)
		}
		return reply.Lines, nil
	}
	var answer struct {
		Lines []string `json:"lines"`
	}
	err := f.Call(ctx, slug, http.MethodPost, "/api/command", map[string]string{"line": line}, &answer)
	if e, ok := AsError(err); ok && e.Code == "failed" {
		return strings.Split(e.Message, "\n"), nil
	}
	return answer.Lines, err
}

// redactCommand is a command for the audit log: a password's not shown.
func redactCommand(line string) string {
	lower := strings.ToLower(line)
	if strings.Contains(lower, "password") {
		word, _, _ := strings.Cut(line, " ")
		return word + " (a password's command)"
	}
	return line
}

// quotaWarning is the words when a key's servers pass its quota on a
// master (from the masters' last answer).
func (f *Fleet) quotaWarning(ctx context.Context, keyID *string) string {
	key, err := f.keyRow(ctx, deref(keyID))
	if err != nil {
		return ""
	}
	count, _ := f.Store.CountKeyInstances(ctx, &key.KeyID)
	if keyID == nil {
		// (the default key's servers: those that name none)
		all, _ := f.Store.CountInstances(ctx)
		count = all
	}
	answers := f.MeAnswers(key)
	for url, me := range answers {
		if me.Publisher != nil && int64(me.Publisher.Quota) < count {
			return fmt.Sprintf("%d servers use key %s, past its quota of %d on %s: ask the master's approvers for more, "+
				"or add a key", count, key.KeyID, me.Publisher.Quota, url)
		}
	}
	return ""
}

// InstanceNode is an instance's node's name.
func (f *Fleet) InstanceNode(ctx context.Context, i db.Instance) string {
	n, err := f.Store.GetNode(ctx, i.NodeID)
	if err != nil {
		return ""
	}
	return n.Name
}

// AddExternal adopts a server the fleet does not run (phase 3): reached by
// its URL and admin password, for everything its API allows.
func (f *Fleet) AddExternal(ctx context.Context, actor, name, url, password, nodeName string) (string, error) {
	slug := spec.Slug(name)
	if slug == "" || !master.ValidInstance(slug) {
		return "", Refuse(http.StatusBadRequest, "schema", "a name of letters and digits")
	}
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://127.") && !strings.HasPrefix(url, "http://localhost") {
		return "", Refuse(http.StatusBadRequest, "schema", "an external server is reached over https:// (or on this machine's loopback)")
	}
	n, err := f.NodeByName(ctx, nodeName)
	if err != nil {
		return "", err
	}
	if _, err := f.Store.GetInstance(ctx, slug); err == nil {
		return "", Refuse(http.StatusConflict, "taken", "%s is taken", slug)
	} else if !store.NotFound(err) {
		return "", err
	}
	client := &serverapi.Client{Base: strings.TrimRight(url, "/"), Password: password, By: "opence-fleet", HTTP: f.HTTP}
	if err := client.Login(ctx); err != nil {
		return "", Refuse(http.StatusBadGateway, "denied", "the server refused the sign-in: %v", err)
	}
	// (ports of its own, outside the node's ranges' use: 0 and negative)
	count, _ := f.Store.CountInstances(ctx)
	err = f.Store.CreateInstance(ctx, db.CreateInstanceParams{Slug: slug, Name: name, NodeID: n.ID, Overrides: "{}",
		TunnelPort: -1 - count, WebPort: -1 - count, LoopbackIndex: -1 - count,
		WebPasswordEnc: f.Key.SealString(Password(24), "instance:"+slug+":web_password"), Kind: "external",
		ExternalUrl: url, ExternalPasswordEnc: f.Key.SealString(password, "instance:"+slug+":external_password"),
		Running: 1, CreatedBy: actor, CreatedAt: f.Now().Unix()})
	if err != nil {
		return "", err
	}
	f.Audit(ctx, actor, "instance.adopt", slug, map[string]string{"url": url})
	go f.watchExternal(context.Background(), slug)
	return slug, nil
}
