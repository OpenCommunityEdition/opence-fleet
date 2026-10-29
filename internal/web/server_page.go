package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/auth"
	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
	"github.com/OpenCommunityEdition/opence-fleet/internal/serverapi"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view/pages"
)

// serverView gathers a server's page's data.
func (s *Server) serverView(ctx context.Context, slug string, p *auth.Principal) (pages.ServerView, error) {
	i, err := s.Fleet.Instance(ctx, slug)
	if err != nil {
		return pages.ServerView{}, err
	}
	row, _ := s.rowOf(ctx, slug)
	v := pages.ServerView{Row: row, Instance: i, Online: s.Fleet.NodeOnline(i.NodeID) || i.Kind == "external"}
	var profile *core.Profile
	if i.ProfileID != nil {
		if pr, err := s.Fleet.Profile(ctx, *i.ProfileID); err == nil {
			v.ProfileName, profile = pr.Name, &pr
		}
	}
	v.Channel = s.Fleet.Channel(ctx, i.Instance, profile)
	v.Pinned = s.Fleet.PinNote(ctx, slug)
	keyID := core.InstanceKeyID(ctx, s.Fleet, i.Instance, profile)
	v.KeyID, v.ServerID = keyID, core.ServerID(keyID, slug)
	if l := s.Fleet.Live.Instance(slug); l != nil {
		v.Applied, v.Exit, v.Tail = l.Applied, l.Exit, l.Tail
		v.Capabilities, v.ServerBuild = l.Capabilities, l.ServerBuild
		switch {
		case l.State == "up":
			v.Since = "up for " + view.Uptime(l.Since, s.Fleet.Now())
		case !l.Since.IsZero():
			v.Since = "since " + view.When(l.Since, s.Fleet.Location())
		}
		if l.Exit != nil {
			v.Since += fmt.Sprintf(" · exit %d", *l.Exit)
		}
	}
	if p.Can(auth.Admin) {
		v.JoinPassword = s.Fleet.JoinPassword(ctx, slug)
	}
	samples, _ := s.Fleet.Store.ListInstanceSamples(ctx, db.ListInstanceSamplesParams{Slug: slug,
		Time: s.Fleet.Now().Add(-24 * time.Hour).Unix()})
	if len(samples) > 1 {
		values := make([]float64, 0, len(samples))
		for _, sm := range samples {
			values = append(values, float64(sm.Players))
		}
		v.Spark = view.SparkPoints(values)
	}
	v.Feed = view.FeedOf(s.Fleet.Live.Feed(30, slug), map[string]string{slug: i.Name}, p.Can(auth.Admin), s.Fleet.Location())
	for k := range v.Feed {
		v.Feed[k].Slug = ""
	}
	v.Profiles = s.profileOptionsCtx(ctx)
	releases, _ := s.Fleet.Releases(ctx)
	for _, r := range releases {
		v.Channels = append(v.Channels, r.Build)
	}
	return v, nil
}

func (s *Server) profileOptionsCtx(ctx context.Context) []pages.Option {
	profiles, _ := s.Fleet.Store.ListProfiles(ctx)
	var out []pages.Option
	for _, p := range profiles {
		out = append(out, pages.Option{Value: strconv.FormatInt(p.ID, 10), Name: p.Name})
	}
	return out
}

// serverLive is the overview's live fragment.
func (s *Server) serverLive(ctx context.Context, slug string, p *auth.Principal) (templ.Component, bool) {
	v, err := s.serverView(ctx, slug, p)
	if err != nil {
		return nil, false
	}
	return pages.ServerLive(v, view.Page{User: p, Zone: s.Fleet.Location()}), true
}

func (s *Server) serverPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	p := principal(r)
	v, err := s.serverView(ctx, slug, p)
	if err != nil {
		s.render(w, r, http.StatusNotFound, s.errorPage(r, "No such server", "There is no server "+slug+"."))
		return
	}
	tab := r.PathValue("tab")
	if tab == "" {
		tab = "overview"
	}
	v.Tab = tab
	page := s.page(r, v.Instance.Name, "servers")
	switch tab {
	case "overview":
		page.Live = true
		if l := s.Fleet.Live.Instance(slug); l != nil && l.Has("vpn") {
			v.VPN = s.vpnView(ctx, slug)
		}
		s.render(w, r, http.StatusOK, pages.ServerOverview(page, v))
	case "players":
		players, problem := s.players(ctx, slug)
		s.render(w, r, http.StatusOK, pages.ServerPlayers(page, v, players, problem))
	case "rotation":
		s.render(w, r, http.StatusOK, pages.ServerRotation(page, v, s.rotationView(ctx, v)))
	case "gametypes":
		s.render(w, r, http.StatusOK, pages.ServerGametypes(page, v, s.gametypesView(ctx, v, r.URL.Query().Get("edit"))))
	case "bans":
		var answer struct {
			Bans []serverapi.Ban `json:"bans"`
		}
		problem := ""
		if !p.Can(auth.Admin) {
			problem = "The server's bans need the admin role."
		} else if err := s.Fleet.Call(ctx, slug, http.MethodGet, "/api/bans", nil, &answer); err != nil {
			problem = "The server did not answer: " + err.Error()
		}
		for i := range answer.Bans {
			if answer.Bans[i].DiscordID == "none" {
				answer.Bans[i].DiscordID = ""
			}
		}
		states, _ := s.Fleet.Store.ListBanStates(ctx)
		var mine []db.InstanceBanState
		for _, st := range states {
			if st.Slug == slug {
				mine = append(mine, st)
			}
		}
		s.render(w, r, http.StatusOK, pages.ServerBans(page, v, answer.Bans, problem, mine))
	case "console":
		lines := s.Fleet.Live.Logs(slug, 400)
		if !p.Can(auth.Admin) {
			var kept []agentproto.Log
			for _, l := range lines {
				if !l.Private {
					kept = append(kept, l)
				}
			}
			lines = kept
		}
		var last []string
		if text, err := r.Cookie("fleet_console"); err == nil {
			last = strings.Split(unescape(text.Value), "\n")
			http.SetCookie(w, &http.Cookie{Name: "fleet_console", Path: "/", MaxAge: -1})
		}
		page.Live = false
		s.render(w, r, http.StatusOK, pages.ServerConsole(page, v, lines, last))
	case "settings":
		settings, fromServer := s.settingLines(ctx, v)
		s.render(w, r, http.StatusOK, pages.ServerSettings(page, v, settings, fromServer))
	case "files":
		files := map[string][]string{}
		problem := ""
		for _, name := range []string{"init.txt", "config.toml", "debug.txt", "stderr.txt"} {
			reply, err := s.Fleet.Control(ctx, slug, agentproto.Control{Action: agentproto.FileTail, File: name, Limit: 500})
			if err != nil {
				problem = err.Error()
				break
			}
			files[name] = reply.Lines
		}
		if !p.Can(auth.Admin) {
			delete(files, "config.toml")
		}
		s.render(w, r, http.StatusOK, pages.ServerFiles(page, v, files, problem))
	case "history":
		games, _ := s.Fleet.Store.ListGames(ctx, db.ListGamesParams{Slug: slug, Limit: 200})
		s.render(w, r, http.StatusOK, pages.ServerHistory(page, v, games))
	default:
		s.render(w, r, http.StatusNotFound, s.errorPage(r, "No such tab", ""))
	}
}

// vpnView is the VPN card's: the server's answer (a few seconds at most:
// the page does not wait on a node) and the refusals of the last hour.
func (s *Server) vpnView(ctx context.Context, slug string) *pages.VPNView {
	now := s.Fleet.Now()
	v := &pages.VPNView{Now: now, RefusedHour: s.Fleet.RefusedSince(ctx, slug, now.Add(-time.Hour))}
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	status, err := s.Fleet.VPN(call, slug)
	if err != nil {
		v.Problem = "The server did not answer (" + err.Error() + "): its VPN lists show once it does."
		return v
	}
	v.Status = status
	return v
}

// serverVPNCheck asks the server whether it refuses an address as a VPN's.
func (s *Server) serverVPNCheck(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	ip := strings.TrimSpace(r.FormValue("ip"))
	answer, err := s.Fleet.CheckVPN(r.Context(), slug, ip)
	if err != nil {
		s.fail(w, r, "/servers/"+slug, err)
		return
	}
	notice := ip + " is not refused as a VPN's."
	switch {
	case answer.Listed:
		notice = ip + " is refused as a VPN's (" + answer.By + ")."
	case answer.By == "vpn_allow.txt":
		notice = ip + " is never refused (vpn_allow.txt)."
	}
	s.done(w, r, "/servers/"+slug, notice)
}

func unescape(text string) string {
	out, err := urlUnescape(text)
	if err != nil {
		return text
	}
	return out
}

func (s *Server) players(ctx context.Context, slug string) ([]serverapi.Player, string) {
	players, err := s.Fleet.Players(ctx, slug)
	if err != nil {
		return nil, "The server did not answer: " + err.Error()
	}
	return players, ""
}

func (s *Server) serverFragment(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	switch r.PathValue("part") {
	case "players":
		players, problem := s.players(r.Context(), slug)
		page := s.page(r, "", "servers")
		s.render(w, r, http.StatusOK, pages.PlayersTable(page, slug, players, problem))
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) rotationView(ctx context.Context, v pages.ServerView) pages.RotationView {
	rv := pages.RotationView{Index: -1}
	if v.Instance.Overrides.Rotation != nil {
		rv.Text = *v.Instance.Overrides.Rotation
	}
	var rotation serverapi.RotationAnswer
	if err := s.Fleet.Call(ctx, v.Instance.Slug, http.MethodGet, "/api/rotation", nil, &rotation); err != nil {
		rv.Problem = "The server did not answer (" + err.Error() + "): its rotation shows once it is up."
		return rv
	}
	rv.Entries, rv.Index = rotation.Entries, rotation.Index
	var maps struct {
		Maps []serverapi.MapInfo `json:"maps"`
	}
	s.Fleet.Call(ctx, v.Instance.Slug, http.MethodGet, "/api/maps", nil, &maps)
	rv.Maps = maps.Maps
	var gametypes serverapi.Gametypes
	s.Fleet.Call(ctx, v.Instance.Slug, http.MethodGet, "/api/gametypes", nil, &gametypes)
	rv.Gametypes = gametypes.Builtin
	for _, f := range gametypes.Files {
		rv.Gametypes = append(rv.Gametypes, f.Name)
	}
	return rv
}

func (s *Server) gametypesView(ctx context.Context, v pages.ServerView, editing string) pages.GametypesView {
	g := pages.GametypesView{Editing: editing, Profile: map[string]string{}}
	if v.Instance.ProfileID != nil {
		if p, err := s.Fleet.Profile(ctx, *v.Instance.ProfileID); err == nil {
			g.Profile, g.Profiled = p.Gametypes, true
		}
	}
	var answer serverapi.Gametypes
	if err := s.Fleet.Call(ctx, v.Instance.Slug, http.MethodGet, "/api/gametypes", nil, &answer); err != nil {
		g.Problem = "The server did not answer (" + err.Error() + "): its profile's files are below."
		for name := range g.Profile {
			g.Files = append(g.Files, serverapi.GametypeFile{Name: name})
		}
	} else {
		g.Files, g.Builtin = answer.Files, answer.Builtin
	}
	if editing != "" {
		var file struct {
			Text string `json:"text"`
		}
		if err := s.Fleet.Call(ctx, v.Instance.Slug, http.MethodGet, "/api/gametypes/"+editing, nil, &file); err == nil {
			g.Text = file.Text
		} else {
			g.Text = g.Profile[editing]
		}
	}
	return g
}

// settingLines is the Settings tab's table: the server's served table when
// it is up, else the fleet's, with each value's source.
func (s *Server) settingLines(ctx context.Context, v pages.ServerView) ([]pages.SettingLine, bool) {
	profileSettings := map[string]any{}
	if v.Instance.ProfileID != nil {
		if p, err := s.Fleet.Profile(ctx, *v.Instance.ProfileID); err == nil {
			profileSettings = p.Settings
		}
	}
	var answer struct {
		Settings []serverapi.SettingInfo `json:"settings"`
	}
	if err := s.Fleet.Call(ctx, v.Instance.Slug, http.MethodGet, "/api/settings", nil, &answer); err != nil {
		answer.Settings = nil
	}
	return settingLinesOf(answer.Settings, profileSettings, v.Instance.Overrides.Settings)
}

// settingLinesOf is the Settings tab's table from what the server serves
// (none: it is not up), its profile's settings and its own. A setting the
// fleet sets shows the fleet's value, pending while the server still serves
// another (the spec reaches the node a moment after a change, and a
// setting that is not live waits for a restart), with the served one beside
// it. The lines are in the table's groups, the server's own last.
func settingLinesOf(served []serverapi.SettingInfo, profileSettings, overrides map[string]any) ([]pages.SettingLine, bool) {
	source := func(name string) string {
		if _, ok := overrides[name]; ok {
			return "this server"
		}
		if _, ok := profileSettings[name]; ok {
			return "its profile"
		}
		return "the default"
	}
	merged := spec.Merge(profileSettings, overrides)
	var out []pages.SettingLine
	for _, st := range served {
		line := pages.SettingLine{Name: st.Name, Type: st.Type, Value: spec.SettingText(st.Value),
			Default: spec.SettingText(st.Default), Comment: st.Comment, Choices: st.Choices, Pinned: st.Pinned,
			Secret: st.Secret, Live: true, Group: pages.OwnSettings}
		if st.Env != nil {
			line.Env = *st.Env
		}
		known, managed := spec.Lookup(st.Name)
		if managed {
			line.Live, line.Group = known.Live, known.Group
		}
		switch {
		case st.Pinned:
			line.Source = "the environment (the fleet's)"
		case managed:
			line.Source = source(st.Name)
			if want, ok := merged[st.Name]; ok && !st.Secret {
				line.Value = spec.SettingText(want)
				if served, err := spec.Normalize(st.Name, st.Value); err != nil || !spec.Equal(served, want) {
					line.Pending, line.Served = true, spec.SettingText(st.Value)
				}
			}
		default:
			// (the server's own: shown, not changed here)
			line.Source = "the server"
			line.Pinned = true
		}
		_, line.Override = overrides[st.Name]
		out = append(out, line)
	}
	fromServer := len(out) > 0
	if !fromServer {
		for _, st := range spec.Settings {
			value := st.Default
			if m, ok := merged[st.Name]; ok {
				value = m
			}
			line := pages.SettingLine{Name: st.Name, Type: st.Type, Value: spec.SettingText(value), Default: spec.SettingText(st.Default),
				Comment: st.Comment, Choices: st.Choices, Secret: st.Secret, Live: st.Live, Env: st.Env, Source: source(st.Name),
				Group: st.Group}
			_, line.Override = overrides[st.Name]
			out = append(out, line)
		}
	}
	order := map[string]int{}
	for k, g := range spec.Groups() {
		order[g] = k + 1
	}
	rank := func(group string) int {
		if order[group] == 0 {
			return len(order) + 1
		}
		return order[group]
	}
	sort.SliceStable(out, func(a, b int) bool {
		if ra, rb := rank(out[a].Group), rank(out[b].Group); ra != rb {
			return ra < rb
		}
		return out[a].Name < out[b].Name
	})
	return out, fromServer
}

func (s *Server) serverLifecycle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	base := "/servers/" + slug
	action := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
	var err error
	notice := ""
	switch action {
	case "start":
		err, notice = s.Fleet.SetRunning(ctx, s.actor(r), slug, true), "Starting."
	case "stop":
		err, notice = s.Fleet.SetRunning(ctx, s.actor(r), slug, false), "Stopping: the game goes to its postgame and the listings are withdrawn."
	case "restart":
		err, notice = s.Fleet.Restart(ctx, s.actor(r), slug), "Restarting."
	case "drain":
		after := r.FormValue("after")
		deadline, _ := time.ParseDuration(s.Fleet.Setting(ctx, "drain_deadline"))
		if deadline <= 0 {
			deadline = 30 * time.Minute
		}
		err = s.Fleet.Drain(ctx, s.actor(r), slug, after, r.FormValue("notice"), deadline, r.FormValue("restart") == "1")
		notice = "Draining: it stops once " + map[string]string{"game": "its game ends", "empty": "it is empty"}[after] + "."
	}
	if err != nil {
		s.fail(w, r, base, err)
		return
	}
	s.done(w, r, base, notice)
}

func (s *Server) deleteServer(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if err := s.Fleet.DeleteInstance(r.Context(), s.actor(r), slug); err != nil {
		s.fail(w, r, "/servers/"+slug, err)
		return
	}
	s.done(w, r, "/servers", slug+" is deleted.")
}

func (s *Server) editServer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	i, err := s.Fleet.Instance(ctx, slug)
	if err != nil {
		s.fail(w, r, "/servers", err)
		return
	}
	var profile *int64
	if id, err := strconv.ParseInt(r.FormValue("profile"), 10, 64); err == nil {
		profile = &id
	}
	channel := r.FormValue("channel")
	if err := s.Fleet.UpdateInstance(ctx, s.actor(r), slug, core.InstanceUpdate{Name: r.FormValue("name"), ProfileID: profile,
		Overrides: i.Overrides, KeyID: i.KeyID, Channel: channel, Notes: r.FormValue("notes")}); err != nil {
		s.fail(w, r, "/servers/"+slug, err)
		return
	}
	s.done(w, r, "/servers/"+slug, "Saved: the server takes what it can now; the rest at its next restart.")
}

func (s *Server) serverSetting(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	name := r.FormValue("name")
	var value any = r.FormValue("value")
	if r.FormValue("reset") == "1" {
		value = nil
	}
	if _, ok := spec.Lookup(name); !ok {
		// (a setting the fleet does not manage: the server's own sv_set)
		lines, err := s.Fleet.Command(r.Context(), s.actor(r), slug, "sv_set "+name+" "+strconv.Quote(r.FormValue("value")), principal(r).Role)
		if err != nil {
			s.fail(w, r, "/servers/"+slug+"/settings", err)
			return
		}
		s.done(w, r, "/servers/"+slug+"/settings", strings.Join(lines, " "))
		return
	}
	if err := s.Fleet.SetOverride(r.Context(), s.actor(r), slug, name, value); err != nil {
		s.fail(w, r, "/servers/"+slug+"/settings", err)
		return
	}
	if value == nil {
		s.done(w, r, "/servers/"+slug+"/settings", name+" is back to its profile's.")
		return
	}
	s.done(w, r, "/servers/"+slug+"/settings", name+" is set for this server.")
}

// serverSettings saves the Settings tab's form: each setting as
// "value.<name>", beside the value the page showed as "shown.<name>". Only
// the settings whose value changed are set (one that was left alone stays
// its profile's), all at once; a "reset" button names a setting to give
// back to its profile, the other changes saved with it. A setting the fleet
// does not manage is the server's own sv_set.
func (s *Server) serverSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	back := "/servers/" + slug + "/settings"
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, back, core.Refuse(http.StatusBadRequest, "form", "the form could not be read"))
		return
	}
	changes := map[string]any{}
	var own []string
	for key, values := range r.PostForm {
		name, ok := strings.CutPrefix(key, "value.")
		if !ok || len(values) == 0 {
			continue
		}
		shown, ok := r.PostForm["shown."+name]
		if ok && len(shown) > 0 && shown[0] == values[0] {
			continue
		}
		if _, managed := spec.Lookup(name); managed {
			changes[name] = values[0]
		} else {
			own = append(own, name)
		}
	}
	reset := r.PostForm.Get("reset")
	if reset != "" {
		if _, managed := spec.Lookup(reset); managed {
			changes[reset] = nil
		}
	}
	if len(changes) == 0 && len(own) == 0 {
		s.done(w, r, back, "Nothing changed.")
		return
	}
	if len(changes) > 0 {
		if err := s.Fleet.SetOverrides(ctx, s.actor(r), slug, changes); err != nil {
			s.fail(w, r, back, err)
			return
		}
	}
	sort.Strings(own)
	var answers []string
	for _, name := range own {
		lines, err := s.Fleet.Command(ctx, s.actor(r), slug, "sv_set "+name+" "+strconv.Quote(r.PostForm.Get("value."+name)), principal(r).Role)
		if err != nil {
			s.fail(w, r, back, err)
			return
		}
		answers = append(answers, lines...)
	}
	var notice []string
	set := len(changes)
	if _, ok := changes[reset]; ok && reset != "" {
		notice = append(notice, reset+" is back to its profile's.")
		set--
	}
	switch {
	case set == 1:
		notice = append(notice, "1 setting saved.")
	case set > 1:
		notice = append(notice, fmt.Sprintf("%d settings saved.", set))
	}
	if len(changes) > 0 {
		notice = append(notice, "The server takes them in a moment (one that is not live, at its next restart).")
	}
	notice = append(notice, answers...)
	s.done(w, r, back, strings.Join(notice, " "))
}

func (s *Server) rotatePassword(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if err := s.Fleet.RotatePassword(r.Context(), s.actor(r), slug); err != nil {
		s.fail(w, r, "/servers/"+slug, err)
		return
	}
	s.done(w, r, "/servers/"+slug, "A new web password is made: restart the server for it to take it.")
}

func (s *Server) serverSay(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	text := strings.TrimSpace(r.FormValue("text"))
	if err := s.Fleet.Call(r.Context(), slug, http.MethodPost, "/api/say", map[string]string{"text": text}, nil); err != nil {
		s.fail(w, r, back(r, "/servers/"+slug), err)
		return
	}
	s.Fleet.Audit(r.Context(), s.actor(r), "instance.say", slug, text)
	s.done(w, r, back(r, "/servers/"+slug), "Said.")
}

func (s *Server) serverNext(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if err := s.Fleet.Call(r.Context(), slug, http.MethodPost, "/api/rotation/next", map[string]any{}, nil); err != nil {
		s.fail(w, r, back(r, "/servers/"+slug), err)
		return
	}
	s.Fleet.Audit(r.Context(), s.actor(r), "instance.next", slug, nil)
	s.done(w, r, back(r, "/servers/"+slug), "The next map comes.")
}

func (s *Server) serverRotation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	i, err := s.Fleet.Instance(ctx, slug)
	if err != nil {
		s.fail(w, r, "/servers", err)
		return
	}
	text := strings.TrimSpace(r.FormValue("rotation"))
	if r.FormValue("reset") == "1" || text == "" {
		i.Overrides.Rotation = nil
	} else {
		// (the server's own check first: all or nothing)
		entries := spec.RotationEntries(text)
		if l := s.Fleet.Live.Instance(slug); l != nil && l.State == "up" {
			var before serverapi.RotationAnswer
			s.Fleet.Call(ctx, slug, http.MethodGet, "/api/rotation", nil, &before)
			if err := s.Fleet.Call(ctx, slug, http.MethodPut, "/api/rotation", map[string]any{"entries": entries}, nil); err != nil {
				s.fail(w, r, "/servers/"+slug+"/rotation", err)
				return
			}
		}
		i.Overrides.Rotation = &text
	}
	if err := s.Fleet.UpdateInstance(ctx, s.actor(r), slug, core.InstanceUpdate{Name: i.Name, ProfileID: i.ProfileID,
		Overrides: i.Overrides, KeyID: i.KeyID, Channel: i.Channel, Notes: i.Notes}); err != nil {
		s.fail(w, r, "/servers/"+slug+"/rotation", err)
		return
	}
	s.done(w, r, "/servers/"+slug+"/rotation", "The rotation is set.")
}

func (s *Server) serverPlay(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	body := map[string]string{"map": r.FormValue("map"), "gametype": r.FormValue("gametype")}
	var answer struct {
		Lines []string `json:"lines"`
	}
	if err := s.Fleet.Call(r.Context(), slug, http.MethodPost, "/api/rotation/play", body, &answer); err != nil {
		s.fail(w, r, "/servers/"+slug+"/rotation", err)
		return
	}
	s.Fleet.Audit(r.Context(), s.actor(r), "instance.play", slug, body)
	s.done(w, r, "/servers/"+slug+"/rotation", strings.Join(answer.Lines, " "))
}

func (s *Server) serverGametype(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	name := strings.TrimSpace(r.FormValue("name"))
	base := "/servers/" + slug + "/gametypes"
	i, err := s.Fleet.Instance(ctx, slug)
	if err != nil {
		s.fail(w, r, "/servers", err)
		return
	}
	if r.FormValue("delete") == "1" {
		if err := s.Fleet.Call(ctx, slug, http.MethodDelete, "/api/gametypes/"+name, nil, nil); err != nil {
			s.fail(w, r, base, err)
			return
		}
		if i.ProfileID != nil {
			s.Fleet.DeleteProfileGametype(ctx, s.actor(r), *i.ProfileID, name)
		}
		s.done(w, r, base, name+" is deleted.")
		return
	}
	text := r.FormValue("text")
	if err := core.CheckGametype(name, text); err != nil {
		s.fail(w, r, base+"?edit="+name, err)
		return
	}
	if l := s.Fleet.Live.Instance(slug); l != nil && l.State == "up" {
		if err := s.Fleet.Call(ctx, slug, http.MethodPut, "/api/gametypes/"+name, map[string]string{"text": text}, nil); err != nil {
			s.fail(w, r, base+"?edit="+name, err)
			return
		}
	}
	if i.ProfileID != nil {
		if err := s.Fleet.SetProfileGametype(ctx, s.actor(r), *i.ProfileID, name, text); err != nil {
			s.fail(w, r, base+"?edit="+name, err)
			return
		}
	}
	s.done(w, r, base+"?edit="+name, name+" is saved.")
}

func (s *Server) playerAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	slot, _ := strconv.Atoi(r.PathValue("slot"))
	action := r.PathValue("action")
	name := r.FormValue("name")
	base := back(r, "/servers/"+slug+"/players")
	p := principal(r)
	var err error
	notice := ""
	switch action {
	case "kick":
		err = s.Fleet.Call(ctx, slug, http.MethodPost, fmt.Sprintf("/api/players/%d/kick", slot), map[string]string{"name": name}, nil)
		notice = name + " is kicked."
	case "ban":
		duration, _ := time.ParseDuration(r.FormValue("duration"))
		if !p.Can(auth.Admin) && (duration <= 0 || duration > 24*time.Hour) {
			err = core.Refuse(http.StatusForbidden, "forbidden", "a moderator's ban is a day at most")
			break
		}
		err = s.Fleet.BanPlayer(ctx, s.actor(r), slug, slot, name, duration, r.FormValue("reason"), r.FormValue("fleet") == "1")
		notice = name + " is banned."
	case "mute":
		err = s.Fleet.Call(ctx, slug, http.MethodPost, fmt.Sprintf("/api/players/%d/mute", slot), map[string]any{"name": name, "minutes": 10}, nil)
		notice = name + " is muted for 10 minutes."
	case "unmute":
		err = s.Fleet.Call(ctx, slug, http.MethodPost, fmt.Sprintf("/api/players/%d/unmute", slot), map[string]string{"name": name}, nil)
		notice = name + " is unmuted."
	case "tell":
		err = s.Fleet.Call(ctx, slug, http.MethodPost, fmt.Sprintf("/api/players/%d/tell", slot),
			map[string]string{"name": name, "text": r.FormValue("text")}, nil)
		notice = "Told " + name + "."
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, base, err)
		return
	}
	if action != "ban" {
		s.Fleet.Audit(ctx, s.actor(r), "player."+action, slug, map[string]string{"name": name})
	}
	s.done(w, r, base, notice)
}

func (s *Server) serverUnban(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	n := r.PathValue("n")
	if err := s.Fleet.Call(r.Context(), slug, http.MethodDelete, "/api/bans/"+n, nil, nil); err != nil {
		s.fail(w, r, "/servers/"+slug+"/bans", err)
		return
	}
	s.Fleet.Audit(r.Context(), s.actor(r), "instance.unban", slug, n)
	s.done(w, r, "/servers/"+slug+"/bans", "Unbanned.")
}

func (s *Server) serverCommand(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	p := principal(r)
	lines, err := s.Fleet.Command(r.Context(), s.actor(r), slug, r.FormValue("line"), p.Role)
	if err != nil && len(lines) == 0 {
		s.fail(w, r, "/servers/"+slug+"/console", err)
		return
	}
	if wantsJSON(r) {
		writeJSON(w, map[string]any{"ok": err == nil, "lines": lines})
		return
	}
	text := "> " + r.FormValue("line") + "\n" + strings.Join(lines, "\n")
	http.SetCookie(w, &http.Cookie{Name: "fleet_console", Value: urlEscape(text), Path: "/", MaxAge: 30, HttpOnly: true,
		SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/servers/"+slug+"/console", http.StatusSeeOther)
}

func (s *Server) serverGroups(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if err := s.Fleet.SetGroups(r.Context(), s.actor(r), []string{slug}, r.FormValue("group"), r.FormValue("op") != "remove"); err != nil {
		s.fail(w, r, "/servers/"+slug, err)
		return
	}
	s.done(w, r, "/servers/"+slug, "The groups are changed.")
}
