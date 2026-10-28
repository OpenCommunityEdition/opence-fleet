package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/auth"
	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
	"github.com/OpenCommunityEdition/opence-fleet/internal/httpx"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// api adds /api/v1: JSON in and out, a bearer token (or the session with
// its CSRF header); the same core calls as the pages, and the same error
// codes (the server's own passed through: pinned, rejected, changed...).
func (s *Server) api(mux *http.ServeMux) {
	read := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.gate(auth.Viewer, h)) }
	act := func(pattern, role string, h http.HandlerFunc) { mux.Handle(pattern, s.gate(role, s.checkForm(h))) }

	mux.HandleFunc("GET /agent/binary/{arch}", s.agentBinary)

	read("GET /api/v1/me", s.apiMe)
	read("GET /api/v1/nodes", s.apiNodes)
	act("POST /api/v1/nodes/{name}/join-token", auth.Admin, s.apiJoinToken)
	act("DELETE /api/v1/nodes/{name}", auth.Admin, s.apiForgetNode)
	read("GET /api/v1/instances", s.apiInstances)
	read("GET /api/v1/instances/{slug}", s.apiInstance)
	act("POST /api/v1/instances", auth.Admin, s.apiCreate)
	act("DELETE /api/v1/instances/{slug}", auth.Admin, s.apiDelete)
	act("POST /api/v1/instances/{slug}/{action}", auth.Moderator, s.apiInstanceAction)
	mux.Handle("/api/v1/instances/{slug}/server/{path...}", s.gate(auth.Viewer, s.checkForm(http.HandlerFunc(s.apiPassthrough))))
	read("GET /api/v1/instances/{slug}/log", s.apiLog)
	read("GET /api/v1/instances/{slug}/events", s.apiEvents)
	read("GET /api/v1/instances/{slug}/games", s.apiGames)
	read("GET /api/v1/profiles", s.apiProfiles)
	act("POST /api/v1/profiles", auth.Admin, s.apiSaveProfile)
	act("PUT /api/v1/profiles/{id}", auth.Admin, s.apiSaveProfile)
	act("DELETE /api/v1/profiles/{id}", auth.Admin, s.apiDeleteProfile)
	read("GET /api/v1/groups", s.apiGroups)
	act("POST /api/v1/groups/{name}/{action}", auth.Moderator, s.apiGroupAction)
	read("GET /api/v1/jobs", s.apiJobs)
	read("GET /api/v1/jobs/{id}", s.apiJob)
	act("POST /api/v1/jobs", auth.Moderator, s.apiStartJob)
	act("POST /api/v1/jobs/{id}/cancel", auth.Admin, s.apiCancelJob)
	read("GET /api/v1/library", s.apiLibrary)
	act("POST /api/v1/library/folder", auth.Owner, s.apiAddFolder)
	act("DELETE /api/v1/library/{sha}", auth.Admin, s.apiRemoveItem)
	read("GET /api/v1/releases", s.apiReleases)
	act("POST /api/v1/releases/check", auth.Admin, s.apiCheckLatest)
	act("POST /api/v1/releases/fetch", auth.Admin, s.apiFetchRelease)
	act("POST /api/v1/releases/local", auth.Admin, s.apiLocalRelease)
	mux.Handle("GET /api/v1/keys", s.gate(auth.Admin, http.HandlerFunc(s.apiKeys)))
	act("POST /api/v1/keys", auth.Owner, s.apiMakeKey)
	act("POST /api/v1/keys/{id}/sign", auth.Owner, s.apiSign)
	mux.Handle("GET /api/v1/keys/{id}/me", s.gate(auth.Admin, http.HandlerFunc(s.apiKeyMe)))
	mux.Handle("GET /api/v1/bans", s.gate(auth.Moderator, http.HandlerFunc(s.apiBans)))
	act("POST /api/v1/bans", auth.Admin, s.apiBan)
	act("DELETE /api/v1/bans/{id}", auth.Admin, s.apiUnban)
	read("GET /api/v1/schedules", s.apiSchedules)
	read("GET /api/v1/alerts", s.apiAlerts)
	mux.Handle("GET /api/v1/audit", s.gate(auth.Admin, http.HandlerFunc(s.apiAudit)))
	mux.Handle("GET /api/v1/export", s.gate(auth.Owner, http.HandlerFunc(s.apiExport)))
	act("POST /api/v1/import", auth.Owner, s.apiImport)
	read("GET /api/v1/stream", s.apiStream)
}

func (s *Server) apiError(w http.ResponseWriter, err error) {
	if e, ok := core.AsError(err); ok {
		status := e.Status
		if status == 0 {
			status = http.StatusBadRequest
		}
		httpx.Fail(w, status, e.Code, e.Message, "")
		return
	}
	httpx.Fail(w, http.StatusBadRequest, "failed", err.Error(), "")
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	writeJSON(w, map[string]any{"ok": true, "name": p.Name, "role": p.Role, "kind": p.Kind, "fleet": s.Fleet.Name(r.Context()),
		"version": s.Version})
}

// instanceJSON is an instance and its live state, as the API gives it.
func (s *Server) instanceJSON(d fleetData, i core.Instance) map[string]any {
	row := d.row(s, i)
	out := map[string]any{"slug": i.Slug, "name": i.Name, "node": row.Node, "profile": row.Profile, "state": row.State,
		"game": row.Game, "players": row.Players, "max_players": row.MaxPlayers, "build": row.Build, "uptime": row.Uptime,
		"tunnel_port": i.TunnelPort, "web_port": i.WebPort, "groups": i.Groups, "running": i.Running != 0,
		"channel": i.Channel, "overrides": i.Overrides, "restart_needed": row.RestartNeeded, "kind": i.Kind}
	if row.Status != nil {
		out["status"] = row.Status
	}
	return out
}

func (s *Server) apiInstances(w http.ResponseWriter, r *http.Request) {
	d := s.data(r.Context())
	list := []map[string]any{}
	group := r.URL.Query().Get("group")
	for _, i := range d.instances {
		if group != "" && !contains(i.Groups, group) {
			continue
		}
		list = append(list, s.instanceJSON(d, i))
	}
	writeJSON(w, map[string]any{"ok": true, "instances": list})
}

func (s *Server) apiInstance(w http.ResponseWriter, r *http.Request) {
	d := s.data(r.Context())
	for _, i := range d.instances {
		if i.Slug == r.PathValue("slug") {
			writeJSON(w, map[string]any{"ok": true, "instance": s.instanceJSON(d, i)})
			return
		}
	}
	httpx.Fail(w, http.StatusNotFound, "not_found", "no such server", "")
}

func (s *Server) apiNodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	nodes, _ := s.Fleet.Nodes(ctx)
	list := []map[string]any{}
	for _, n := range nodes {
		card, _ := s.nodeCard(ctx, n.Name)
		list = append(list, map[string]any{"name": n.Name, "online": card.Online, "settings": n.Settings, "agent_version": card.Version,
			"servers": card.Instances, "load": card.Load, "memory": card.Memory, "disk": card.Disk, "missing": card.Missing})
	}
	writeJSON(w, map[string]any{"ok": true, "nodes": list})
}

func (s *Server) apiJoinToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Runtime       string `json:"runtime"`
		PublicAddress string `json:"public_address"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(r, &in); err != nil {
			s.apiError(w, err)
			return
		}
	}
	settings := spec.DefaultNode()
	settings.Runtime, settings.PublicAddress = in.Runtime, in.PublicAddress
	name := r.PathValue("name")
	token, err := s.Fleet.JoinToken(r.Context(), s.actor(r), name, settings)
	if err != nil {
		s.apiError(w, err)
		return
	}
	url := s.publicURL(r)
	writeJSON(w, map[string]any{"ok": true, "token": token, "command": "curl -fsSL " + url + "/agent.sh | sudo sh -s -- --join " +
		url + " --token " + token, "expires": s.Fleet.Now().Add(core.JoinTokenTTL).Unix()})
}

func (s *Server) apiForgetNode(w http.ResponseWriter, r *http.Request) {
	if err := s.Fleet.ForgetNode(r.Context(), s.actor(r), r.PathValue("name")); err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) apiCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Profile string   `json:"profile"`
		Nodes   []string `json:"nodes"`
		Count   int      `json:"count"`
		Pattern string   `json:"pattern"`
		Groups  []string `json:"groups"`
		Start   *bool    `json:"start"`
		Channel string   `json:"channel"`
	}
	if err := readJSON(r, &in); err != nil {
		s.apiError(w, err)
		return
	}
	id, err := strconv.ParseInt(in.Profile, 10, 64)
	if err != nil {
		p, perr := s.Fleet.Store.GetProfileByName(r.Context(), in.Profile)
		if perr != nil {
			s.apiError(w, core.NotFound("profile "+in.Profile))
			return
		}
		id = p.ID
	}
	start := in.Start == nil || *in.Start
	if in.Count == 0 {
		in.Count = 1
	}
	created, warnings, err := s.Fleet.CreateInstances(r.Context(), s.actor(r), core.CreateRequest{ProfileID: id, Nodes: in.Nodes,
		Count: in.Count, Pattern: in.Pattern, Groups: in.Groups, Running: start, Channel: in.Channel})
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "created": created, "warnings": warnings})
}

func (s *Server) apiDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.Fleet.DeleteInstance(r.Context(), s.actor(r), r.PathValue("slug")); err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) apiInstanceAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug, action := r.PathValue("slug"), r.PathValue("action")
	p := principal(r)
	var in struct {
		After    string `json:"after"`
		Notice   string `json:"notice"`
		Deadline string `json:"deadline"`
		Restart  *bool  `json:"restart"`
		Line     string `json:"line"`
		Text     string `json:"text"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(r, &in); err != nil {
			s.apiError(w, err)
			return
		}
	}
	admin := func() bool {
		if !p.Can(auth.Admin) {
			httpx.Fail(w, http.StatusForbidden, "forbidden", "needs the admin role", "")
			return false
		}
		return true
	}
	var err error
	switch action {
	case "start", "stop", "restart", "drain":
		if !admin() {
			return
		}
		switch action {
		case "start":
			err = s.Fleet.SetRunning(ctx, s.actor(r), slug, true)
		case "stop":
			err = s.Fleet.SetRunning(ctx, s.actor(r), slug, false)
		case "restart":
			err = s.Fleet.Restart(ctx, s.actor(r), slug)
		case "drain":
			deadline, perr := time.ParseDuration(in.Deadline)
			if perr != nil || deadline <= 0 {
				deadline = 30 * time.Minute
			}
			restart := in.Restart == nil || *in.Restart
			err = s.Fleet.Drain(ctx, s.actor(r), slug, in.After, in.Notice, deadline, restart)
		}
	case "command":
		var lines []string
		lines, err = s.Fleet.Command(ctx, s.actor(r), slug, in.Line, p.Role)
		if err == nil || len(lines) > 0 {
			writeJSON(w, map[string]any{"ok": err == nil, "lines": lines})
			return
		}
	case "say":
		err = s.Fleet.Call(ctx, slug, http.MethodPost, "/api/say", map[string]string{"text": in.Text}, nil)
	default:
		httpx.Fail(w, http.StatusNotFound, "not_found", "no such action", "")
		return
	}
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// apiPassthrough is the server's own API, one to one, through its agent:
// reads for viewers, moderation for moderators, the rest for admins.
func (s *Server) apiPassthrough(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	path := "/api/" + r.PathValue("path")
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	p := principal(r)
	need := auth.Viewer
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		need = auth.Admin
		if strings.HasPrefix(path, "/api/players/") || path == "/api/say" || path == "/api/rotation/next" ||
			path == "/api/rotation/play" {
			need = auth.Moderator
		}
	}
	if strings.HasPrefix(path, "/api/bans") || strings.HasPrefix(path, "/api/settings") {
		need = auth.Admin
	}
	if !p.Can(need) {
		httpx.Fail(w, http.StatusForbidden, "forbidden", "needs the "+need+" role", "")
		return
	}
	var body any
	if r.Body != nil && r.Method != http.MethodGet {
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			httpx.Fail(w, http.StatusRequestEntityTooLarge, "too_large", "64 KB at most", "")
			return
		}
		if len(data) > 0 {
			body = json.RawMessage(data)
		}
	}
	status, answer, err := s.Fleet.API(r.Context(), slug, r.Method, path, body)
	if err != nil {
		s.apiError(w, err)
		return
	}
	if r.Method != http.MethodGet {
		s.Fleet.Audit(r.Context(), s.actor(r), "instance.api", slug, r.Method+" "+path)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(answer)
}

func (s *Server) apiLog(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	p := principal(r)
	lines := []any{}
	next := after
	for _, l := range s.Fleet.Live.Logs(slug, 0) {
		if l.N > after && (!l.Private || p.Can(auth.Admin)) {
			lines = append(lines, l)
			next = l.N
		}
	}
	writeJSON(w, map[string]any{"ok": true, "lines": lines, "next": next})
}

func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if before == 0 {
		before = 1 << 62
	}
	rows, err := s.Fleet.Store.ListEventsBySlug(r.Context(), db.ListEventsBySlugParams{Slug: r.PathValue("slug"), ID: before, Limit: 500})
	if err != nil {
		s.apiError(w, err)
		return
	}
	events := []map[string]any{}
	admin, moderator := principal(r).Can(auth.Admin), principal(r).Can(auth.Moderator)
	for _, e := range rows {
		var data map[string]any
		json.Unmarshal([]byte(e.Data), &data)
		if !admin {
			delete(data, "address")
			delete(data, "hwid")
			if e.Type == "chat" {
				continue
			}
		}
		// (a player's Discord account, as the pages show it: to moderators)
		if !moderator {
			delete(data, "discord_id")
			delete(data, "discord_name")
		}
		events = append(events, map[string]any{"id": e.ID, "time": e.Time, "type": e.Type, "data": data})
	}
	writeJSON(w, map[string]any{"ok": true, "events": events})
}

func (s *Server) apiGames(w http.ResponseWriter, r *http.Request) {
	games, err := s.Fleet.Store.ListGames(r.Context(), db.ListGamesParams{Slug: r.PathValue("slug"), Limit: 500})
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "games": games})
}

func (s *Server) apiProfiles(w http.ResponseWriter, r *http.Request) {
	profiles, err := s.Fleet.Profiles(r.Context())
	if err != nil {
		s.apiError(w, err)
		return
	}
	list := []map[string]any{}
	for _, p := range profiles {
		list = append(list, map[string]any{"id": p.ID, "name": p.Name, "description": p.Description, "settings": p.Settings,
			"rotation": p.Rotation, "region": p.Region, "init_txt": p.InitTxt, "channel": p.Channel,
			"password_policy": p.PasswordPolicy, "key_id": p.KeyID, "gametypes": p.Gametypes})
	}
	writeJSON(w, map[string]any{"ok": true, "profiles": list})
}

type profileJSON struct {
	Name           string            `json:"name"`
	Description    string            `json:"description"`
	Settings       map[string]any    `json:"settings"`
	Rotation       string            `json:"rotation"`
	Region         string            `json:"region"`
	InitTxt        string            `json:"init_txt"`
	Channel        string            `json:"channel"`
	PasswordPolicy string            `json:"password_policy"`
	Password       string            `json:"password"`
	KeyID          string            `json:"key_id"`
	Gametypes      map[string]string `json:"gametypes"`
}

func (p profileJSON) input() core.ProfileInput {
	return core.ProfileInput{Name: p.Name, Description: p.Description, Settings: p.Settings, Rotation: p.Rotation,
		Region: p.Region, InitTxt: p.InitTxt, Channel: p.Channel, PasswordPolicy: p.PasswordPolicy, Password: p.Password,
		KeyID: p.KeyID}
}

func (s *Server) apiSaveProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in profileJSON
	if err := readJSON(r, &in); err != nil {
		s.apiError(w, err)
		return
	}
	var id int64
	var err error
	if r.PathValue("id") == "" {
		id, err = s.Fleet.CreateProfile(ctx, s.actor(r), in.input())
	} else {
		id, _ = strconv.ParseInt(r.PathValue("id"), 10, 64)
		err = s.Fleet.UpdateProfile(ctx, s.actor(r), id, in.input())
	}
	if err != nil {
		s.apiError(w, err)
		return
	}
	for name, text := range in.Gametypes {
		if err := s.Fleet.SetProfileGametype(ctx, s.actor(r), id, name, text); err != nil {
			s.apiError(w, err)
			return
		}
	}
	writeJSON(w, map[string]any{"ok": true, "id": id})
}

func (s *Server) apiDeleteProfile(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Fleet.DeleteProfile(r.Context(), s.actor(r), id); err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) apiGroups(w http.ResponseWriter, r *http.Request) {
	groups, members, err := s.Fleet.Groups(r.Context())
	if err != nil {
		s.apiError(w, err)
		return
	}
	list := []map[string]any{}
	for _, g := range groups {
		list = append(list, map[string]any{"name": g.Name, "description": g.Description, "servers": members[g.Name]})
	}
	writeJSON(w, map[string]any{"ok": true, "groups": list})
}

// apiGroupAction runs a bulk action on a group (a job).
func (s *Server) apiGroupAction(w http.ResponseWriter, r *http.Request) {
	slugs, _ := s.Fleet.GroupMembers(r.Context(), r.PathValue("name"))
	var params core.JobParams
	if r.ContentLength > 0 {
		if err := readJSON(r, &params); err != nil {
			s.apiError(w, err)
			return
		}
	}
	params.Action, params.Slugs, params.Role = r.PathValue("action"), slugs, principal(r).Role
	s.startJobJSON(w, r, params)
}

func (s *Server) startJobJSON(w http.ResponseWriter, r *http.Request, params core.JobParams) {
	p := principal(r)
	if !p.Can(auth.Admin) && params.Action != core.ActionSay && params.Action != core.ActionNext && params.Action != core.ActionEndGame {
		httpx.Fail(w, http.StatusForbidden, "forbidden", "that action needs the admin role", "")
		return
	}
	id, err := s.Fleet.StartJob(r.Context(), s.actor(r), params)
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "job": id})
}

func (s *Server) apiStartJob(w http.ResponseWriter, r *http.Request) {
	var params core.JobParams
	if err := readJSON(r, &params); err != nil {
		s.apiError(w, err)
		return
	}
	params.Role = principal(r).Role
	s.startJobJSON(w, r, params)
}

func (s *Server) apiJobs(w http.ResponseWriter, r *http.Request) {
	jobs, _ := s.Fleet.Store.ListJobs(r.Context(), 200)
	writeJSON(w, map[string]any{"ok": true, "jobs": jobs})
}

func (s *Server) apiJob(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	job, items, err := s.Fleet.Job(r.Context(), id)
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "job": job, "items": items})
}

func (s *Server) apiCancelJob(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Fleet.CancelJob(r.Context(), s.actor(r), id); err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) apiLibrary(w http.ResponseWriter, r *http.Request) {
	items, _ := s.Fleet.Library(r.Context())
	list := []map[string]any{}
	for _, it := range items {
		list = append(list, map[string]any{"sha256": it.Sha256, "kind": it.Kind, "name": it.Name, "size": it.Size})
	}
	writeJSON(w, map[string]any{"ok": true, "items": list, "missing": s.Fleet.Missing(r.Context())})
}

func (s *Server) apiAddFolder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &in); err != nil {
		s.apiError(w, err)
		return
	}
	added, skipped, err := s.Fleet.AddFolder(r.Context(), s.actor(r), in.Path)
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "added": added, "skipped": skipped})
}

func (s *Server) apiRemoveItem(w http.ResponseWriter, r *http.Request) {
	if err := s.Fleet.RemoveItem(r.Context(), s.actor(r), r.PathValue("sha")); err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) apiReleases(w http.ResponseWriter, r *http.Request) {
	releases, _ := s.Fleet.Releases(r.Context())
	writeJSON(w, map[string]any{"ok": true, "releases": releases})
}

func (s *Server) apiCheckLatest(w http.ResponseWriter, r *http.Request) {
	release, moved, err := s.Fleet.CheckLatest(r.Context(), s.actor(r))
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "latest": release.Build, "digest": release.Digest, "moved": moved})
}

func (s *Server) apiFetchRelease(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Tag string `json:"tag"`
		Zip bool   `json:"zip"`
	}
	if err := readJSON(r, &in); err != nil {
		s.apiError(w, err)
		return
	}
	if err := s.Fleet.FetchRelease(r.Context(), s.actor(r), in.Tag); err != nil {
		s.apiError(w, err)
		return
	}
	if in.Zip {
		if err := s.Fleet.FetchZip(r.Context(), s.actor(r), in.Tag); err != nil {
			s.apiError(w, err)
			return
		}
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) apiLocalRelease(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Build  string `json:"build"`
		Image  string `json:"image"`
		Latest bool   `json:"latest"`
	}
	if err := readJSON(r, &in); err != nil {
		s.apiError(w, err)
		return
	}
	if err := s.Fleet.AddLocalRelease(r.Context(), s.actor(r), in.Build, in.Image, in.Latest); err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) apiKeys(w http.ResponseWriter, r *http.Request) {
	keys, _ := s.Fleet.Keys(r.Context())
	list := []map[string]any{}
	for _, k := range keys {
		list = append(list, map[string]any{"key_id": k.KeyID, "public_key": k.PublicKey, "label": k.Label,
			"default": k.IsDefault != 0, "masters": s.Fleet.MeAnswers(k)})
	}
	writeJSON(w, map[string]any{"ok": true, "keys": list})
}

func (s *Server) apiMakeKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Label string `json:"label"`
		Seed  string `json:"seed"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(r, &in); err != nil {
			s.apiError(w, err)
			return
		}
	}
	var id string
	var err error
	if in.Seed != "" {
		id, err = s.Fleet.ImportKey(r.Context(), s.actor(r), in.Label, in.Seed)
	} else {
		id, err = s.Fleet.MakeKey(r.Context(), s.actor(r), in.Label)
	}
	if err != nil {
		s.apiError(w, err)
		return
	}
	key, _ := s.Fleet.Store.GetKey(r.Context(), id)
	writeJSON(w, map[string]any{"ok": true, "key_id": id, "public_key": key.PublicKey})
}

func (s *Server) apiSign(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Nonce string `json:"nonce"`
	}
	if err := readJSON(r, &in); err != nil {
		s.apiError(w, err)
		return
	}
	proof, err := s.Fleet.SignNonce(r.Context(), s.actor(r), r.PathValue("id"), in.Nonce)
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "proof": proof})
}

func (s *Server) apiKeyMe(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.URL.Query().Get("refresh") == "1" {
		if err := s.Fleet.RefreshMe(r.Context(), id); err != nil {
			s.apiError(w, err)
			return
		}
	}
	key, err := s.Fleet.Store.GetKey(r.Context(), id)
	if err != nil {
		s.apiError(w, core.NotFound("key "+id))
		return
	}
	writeJSON(w, map[string]any{"ok": true, "masters": s.Fleet.MeAnswers(key)})
}

func (s *Server) apiBans(w http.ResponseWriter, r *http.Request) {
	bans, err := s.Fleet.Bans(r.Context(), r.URL.Query().Get("all") == "1")
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "bans": bans})
}

func (s *Server) apiBan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Address   string `json:"address"`
		HWID      string `json:"hwid"`
		DiscordID string `json:"discord_id"`
		Names     string `json:"names"`
		Reason    string `json:"reason"`
		Duration  string `json:"duration"`
	}
	if err := readJSON(r, &in); err != nil {
		s.apiError(w, err)
		return
	}
	duration, _ := time.ParseDuration(in.Duration)
	id, err := s.Fleet.CreateBan(r.Context(), s.actor(r), core.BanInput{Address: in.Address, HWID: in.HWID, DiscordID: in.DiscordID,
		Names: in.Names, Reason: in.Reason, Duration: duration, Source: "api"})
	if err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id})
}

func (s *Server) apiUnban(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Fleet.Unban(r.Context(), s.actor(r), id); err != nil {
		s.apiError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) apiSchedules(w http.ResponseWriter, r *http.Request) {
	list, _ := s.Fleet.Store.ListSchedules(r.Context())
	writeJSON(w, map[string]any{"ok": true, "schedules": list})
}

func (s *Server) apiAlerts(w http.ResponseWriter, r *http.Request) {
	open, _ := s.Fleet.Store.ListOpenAlerts(r.Context())
	writeJSON(w, map[string]any{"ok": true, "alerts": open})
}

func (s *Server) apiAudit(w http.ResponseWriter, r *http.Request) {
	rows, next := s.audit(r, 500)
	writeJSON(w, map[string]any{"ok": true, "audit": rows, "before": next})
}

// Export is what moves between fleets: profiles (with their gametypes),
// groups, bans, schedules, and, asked for, the publisher keys' seeds (the
// owner's secret: only over this signed-in API).
type Export struct {
	Version   string              `json:"version"`
	Profiles  []profileJSON       `json:"profiles"`
	Groups    []db.Group          `json:"groups"`
	Bans      []db.FleetBan       `json:"bans"`
	Schedules []db.Schedule       `json:"schedules"`
	Keys      []map[string]string `json:"keys,omitempty"`
	Settings  map[string]string   `json:"settings"`
}

func (s *Server) apiExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := Export{Version: s.Version, Settings: map[string]string{}}
	profiles, _ := s.Fleet.Profiles(ctx)
	for _, p := range profiles {
		out.Profiles = append(out.Profiles, profileJSON{Name: p.Name, Description: p.Description, Settings: p.Settings,
			Rotation: p.Rotation, Region: p.Region, InitTxt: p.InitTxt, Channel: p.Channel, PasswordPolicy: p.PasswordPolicy,
			KeyID: deref(p.KeyID), Gametypes: p.Gametypes})
	}
	out.Groups, _ = s.Fleet.Store.ListGroups(ctx)
	out.Bans, _ = s.Fleet.Bans(ctx, false)
	out.Schedules, _ = s.Fleet.Store.ListSchedules(ctx)
	for _, name := range []string{"name", "default_channel", "rolling_per_node", "drain_deadline"} {
		out.Settings[name] = s.Fleet.Setting(ctx, name)
	}
	if r.URL.Query().Get("keys") == "1" {
		keys, _ := s.Fleet.Keys(ctx)
		for _, k := range keys {
			seed, err := s.Fleet.Key.OpenString(k.SeedEnc, "key:"+k.KeyID)
			if err == nil {
				out.Keys = append(out.Keys, map[string]string{"label": k.Label, "seed": seed})
			}
		}
		s.Fleet.Audit(ctx, s.actor(r), "export.keys", "", fmt.Sprintf("%d keys", len(out.Keys)))
	}
	s.Fleet.Audit(ctx, s.actor(r), "export", "", nil)
	w.Header().Set("Content-Disposition", `attachment; filename="fleet-export.json"`)
	writeJSON(w, out)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (s *Server) apiImport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in Export
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	if err := decoder.Decode(&in); err != nil {
		s.apiError(w, core.Refuse(http.StatusBadRequest, "schema", "not an export: %v", err))
		return
	}
	actor := s.actor(r)
	var done, skipped []string
	for _, k := range in.Keys {
		if _, err := s.Fleet.ImportKey(ctx, actor, k["label"], k["seed"]); err != nil {
			skipped = append(skipped, "key "+k["label"]+": "+err.Error())
		} else {
			done = append(done, "key "+k["label"])
		}
	}
	for _, p := range in.Profiles {
		id, err := s.Fleet.CreateProfile(ctx, actor, p.input())
		if err != nil {
			skipped = append(skipped, "profile "+p.Name+": "+err.Error())
			continue
		}
		for name, text := range p.Gametypes {
			s.Fleet.SetProfileGametype(ctx, actor, id, name, text)
		}
		done = append(done, "profile "+p.Name)
	}
	for _, g := range in.Groups {
		s.Fleet.Store.UpsertGroup(ctx, db.UpsertGroupParams{Name: g.Name, Colour: g.Colour, Description: g.Description})
	}
	for _, b := range in.Bans {
		var d time.Duration
		if b.Until != nil {
			d = time.Until(time.Unix(*b.Until, 0))
			if d <= 0 {
				continue
			}
		}
		if _, err := s.Fleet.CreateBan(ctx, actor, core.BanInput{Address: b.Address, HWID: b.Hwid, DiscordID: b.DiscordID,
			Names: b.Names, Reason: b.Reason, Duration: d, Source: "import"}); err == nil {
			done = append(done, "a ban")
		}
	}
	for _, sc := range in.Schedules {
		t, params := core.ReadSchedule(sc)
		if _, err := s.Fleet.CreateSchedule(ctx, actor, core.ScheduleInput{Name: sc.Name, Cron: sc.Cron, Action: sc.Action,
			Targets: t, Params: params, Enabled: false}); err != nil {
			skipped = append(skipped, "schedule "+sc.Name+": "+err.Error())
		} else {
			done = append(done, "schedule "+sc.Name+" (disabled)")
		}
	}
	writeJSON(w, map[string]any{"ok": true, "imported": done, "skipped": skipped})
}

// apiStream is the live stream as JSON lines of server-sent events, for
// scripts: each instance's status and state as it changes.
func (s *Server) apiStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	changes, cancel := s.Fleet.Live.Subscribe()
	defer cancel()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case c, ok := <-changes:
			if !ok {
				return
			}
			var data any
			switch c.Kind {
			case "instance":
				l := s.Fleet.Live.Instance(c.Key)
				if l == nil {
					data = map[string]any{"slug": c.Key, "removed": true}
				} else {
					data = map[string]any{"slug": c.Key, "state": l.State, "reason": l.Reason, "build": l.Build, "status": l.Status}
				}
			case "feed":
				if c.Feed == nil || (c.Feed.Private && !principal(r).Can(auth.Admin)) {
					continue
				}
				data = c.Feed
			case "node", "alert", "job":
				data = map[string]any{"key": c.Key}
			default:
				continue
			}
			payload, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", c.Kind, payload)
			flusher.Flush()
		}
	}
}
