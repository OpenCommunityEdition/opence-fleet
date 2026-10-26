package web

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/OpenCommunityEdition/opence-fleet/internal/auth"
	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view/pages"
)

func (s *Server) jobsPage(w http.ResponseWriter, r *http.Request) {
	jobs, _ := s.Fleet.Store.ListJobs(r.Context(), 200)
	s.render(w, r, http.StatusOK, pages.Jobs(s.page(r, "Jobs", "jobs"), jobs))
}

func (s *Server) jobPage(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	job, items, err := s.Fleet.Job(r.Context(), id)
	if err != nil {
		s.render(w, r, http.StatusNotFound, s.errorPage(r, "No such job", ""))
		return
	}
	page := s.page(r, "Job "+r.PathValue("id"), "jobs")
	page.Live = true
	s.render(w, r, http.StatusOK, pages.Job(page, job, items))
}

// jobFragment is a job's live fragment alone: the page asks it again when
// its live stream opens (a job that ended before it did).
func (s *Server) jobFragment(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	job, items, err := s.Fleet.Job(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, http.StatusOK, pages.JobTable(job, items, s.Fleet.Location()))
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Fleet.CancelJob(r.Context(), s.actor(r), id); err != nil {
		s.fail(w, r, "/jobs/"+r.PathValue("id"), err)
		return
	}
	s.done(w, r, "/jobs/"+r.PathValue("id"), "Cancelled: what runs finishes, the next waves do not start.")
}

func (s *Server) schedulesPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, _ := s.Fleet.Store.ListSchedules(ctx)
	var lines []pages.ScheduleLine
	for _, sc := range list {
		t, p := core.ReadSchedule(sc)
		lines = append(lines, pages.ScheduleLine{Schedule: sc, Targets: t, Params: p, Summary: core.ScheduleSummary(sc)})
	}
	s.render(w, r, http.StatusOK, pages.Schedules(s.page(r, "Schedules", "schedules"), lines, s.groupNames(r),
		s.Fleet.Location().String()))
}

func splitList(text string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == ' ' }) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (s *Server) saveSchedule(w http.ResponseWriter, r *http.Request) {
	in := core.ScheduleInput{Name: r.FormValue("name"), Cron: r.FormValue("cron"), Action: r.FormValue("action"),
		Targets: core.Targets{Slugs: splitList(r.FormValue("slugs")), Groups: splitList(r.FormValue("groups"))},
		Params: core.JobParams{Text: r.FormValue("text"), After: r.FormValue("after"), Channel: r.FormValue("channel"),
			Notice: r.FormValue("notice")},
		Enabled: r.FormValue("enabled") == "1"}
	if id, err := strconv.ParseInt(r.FormValue("profile"), 10, 64); err == nil {
		in.Params.ProfileID = id
	}
	var err error
	if id := r.PathValue("id"); id != "" {
		n, _ := strconv.ParseInt(id, 10, 64)
		err = s.Fleet.UpdateSchedule(r.Context(), s.actor(r), n, in)
	} else {
		_, err = s.Fleet.CreateSchedule(r.Context(), s.actor(r), in)
	}
	if err != nil {
		s.fail(w, r, "/schedules", err)
		return
	}
	s.done(w, r, "/schedules", "The schedule is saved.")
}

func (s *Server) runSchedule(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	job, err := s.Fleet.RunSchedule(r.Context(), id)
	if err != nil {
		s.fail(w, r, "/schedules", err)
		return
	}
	if job > 0 {
		s.done(w, r, "/jobs/"+strconv.FormatInt(job, 10), "The schedule runs.")
		return
	}
	s.done(w, r, "/schedules", "Done.")
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Fleet.DeleteSchedule(r.Context(), s.actor(r), id); err != nil {
		s.fail(w, r, "/schedules", err)
		return
	}
	s.done(w, r, "/schedules", "The schedule is deleted.")
}

func (s *Server) alertsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	open, _ := s.Fleet.Store.ListOpenAlerts(ctx)
	all, _ := s.Fleet.Store.ListAlerts(ctx, 100)
	var o, resolved []pages.AlertRow
	for _, a := range open {
		o = append(o, pages.AlertRow{Alert: a, RuleName: core.RuleName(a.Rule)})
	}
	for _, a := range all {
		if a.ResolvedAt != nil {
			resolved = append(resolved, pages.AlertRow{Alert: a, RuleName: core.RuleName(a.Rule)})
		}
	}
	var hooks []pages.WebhookRow
	if principal(r).Can(auth.Admin) {
		list, _ := s.Fleet.Webhooks(ctx)
		for _, h := range list {
			hooks = append(hooks, pages.WebhookRow{ID: h.ID, Label: h.Label, Host: core.WebhookHost(h.URL), Kind: h.Kind,
				Status: h.LastStatus, Rules: h.Rules})
		}
	}
	s.render(w, r, http.StatusOK, pages.Alerts(s.page(r, "Alerts", "alerts"), o, resolved, hooks))
}

func (s *Server) acknowledgeAlert(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Fleet.AcknowledgeAlert(r.Context(), s.actor(r), id); err != nil {
		s.fail(w, r, "/alerts", err)
		return
	}
	s.done(w, r, back(r, "/alerts"), "Marked seen.")
}

func (s *Server) resolveAlert(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Fleet.ResolveAlertByID(r.Context(), s.actor(r), id); err != nil {
		s.fail(w, r, "/alerts", err)
		return
	}
	s.done(w, r, "/alerts", "Resolved (it opens again if its condition holds).")
}

func (s *Server) addWebhook(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	rules := r.Form["rule"]
	for _, rule := range rules {
		if rule == "*" {
			rules = []string{"*"}
		}
	}
	if _, err := s.Fleet.AddWebhook(r.Context(), s.actor(r), r.FormValue("label"), r.FormValue("url"), r.FormValue("kind"), rules); err != nil {
		s.fail(w, r, "/alerts", err)
		return
	}
	s.done(w, r, "/alerts", "The webhook is added: test it.")
}

func (s *Server) testWebhook(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Fleet.TestWebhook(r.Context(), s.actor(r), id); err != nil {
		s.fail(w, r, "/alerts", err)
		return
	}
	s.done(w, r, "/alerts", "The test was sent.")
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Fleet.DeleteWebhook(r.Context(), s.actor(r), id); err != nil {
		s.fail(w, r, "/alerts", err)
		return
	}
	s.done(w, r, "/alerts", "The webhook is deleted.")
}

func (s *Server) audit(r *http.Request, limit int64) ([]db.Audit, int64) {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	rows, _ := s.Fleet.Store.ListAudit(r.Context(), db.ListAuditParams{Actor: q.Get("actor"), Action: q.Get("action"),
		Target: q.Get("target"), Before: before, Lim: limit})
	next := int64(0)
	if int64(len(rows)) == limit {
		next = rows[len(rows)-1].ID
	}
	return rows, next
}

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request) {
	rows, next := s.audit(r, 200)
	q := r.URL.Query()
	s.render(w, r, http.StatusOK, pages.Audit(s.page(r, "Audit", "audit"), rows, q.Get("actor"), q.Get("action"), q.Get("target"), next))
}

func (s *Server) auditExport(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.audit(r, 100000)
	w.Header().Set("Content-Disposition", `attachment; filename="fleet-audit.json"`)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rows)
}

func (s *Server) teamView(r *http.Request) pages.TeamView {
	ctx := r.Context()
	var t pages.TeamView
	t.Users, _ = s.Fleet.Store.ListUsers(ctx)
	t.Sessions, _ = s.Fleet.Store.ListSessions(ctx)
	t.Invites, _ = s.Fleet.Store.ListInvites(ctx, s.Fleet.Now().Unix())
	t.Tokens, _ = s.Fleet.Store.ListTokens(ctx)
	return t
}

func (s *Server) teamPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, pages.Team(s.page(r, "Team", "team"), s.teamView(r)))
}

func (s *Server) makeInvite(w http.ResponseWriter, r *http.Request) {
	code, err := s.Auth.MakeInvite(r.Context(), principal(r), r.FormValue("role"), r.FormValue("label"))
	if err != nil {
		s.fail(w, r, "/team", err)
		return
	}
	s.Fleet.Audit(r.Context(), s.actor(r), "team.invite", r.FormValue("role"), r.FormValue("label"))
	t := s.teamView(r)
	t.InviteLink = s.publicURL(r) + "/invite/" + code
	s.render(w, r, http.StatusOK, pages.Team(s.page(r, "Team", "team"), t))
}

func (s *Server) makeToken(w http.ResponseWriter, r *http.Request) {
	token, err := s.Auth.MakeToken(r.Context(), principal(r), r.FormValue("label"), r.FormValue("role"))
	if err != nil {
		s.fail(w, r, "/team", err)
		return
	}
	s.Fleet.Audit(r.Context(), s.actor(r), "team.token", r.FormValue("label"), r.FormValue("role"))
	if wantsJSON(r) {
		writeJSON(w, map[string]any{"ok": true, "token": token})
		return
	}
	t := s.teamView(r)
	t.Token = token
	s.render(w, r, http.StatusOK, pages.Team(s.page(r, "Team", "team"), t))
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Fleet.Store.DeleteToken(r.Context(), id); err != nil {
		s.fail(w, r, "/team", err)
		return
	}
	s.Fleet.Audit(r.Context(), s.actor(r), "team.token_revoke", r.PathValue("id"), nil)
	s.done(w, r, "/team", "The token is revoked.")
}

func (s *Server) setRole(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	role := r.FormValue("role")
	if err := s.Auth.SetRole(r.Context(), principal(r), id, role); err != nil {
		s.fail(w, r, "/team", err)
		return
	}
	s.Fleet.Audit(r.Context(), s.actor(r), "team.role", r.PathValue("id"), role)
	s.done(w, r, "/team", "The role is set: their sessions ended.")
}

func (s *Server) disableUser(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	enable := r.FormValue("enable") == "1"
	if err := s.Auth.SetDisabled(r.Context(), principal(r), id, !enable); err != nil {
		s.fail(w, r, "/team", err)
		return
	}
	action := "team.disable"
	if enable {
		action = "team.enable"
	}
	s.Fleet.Audit(r.Context(), s.actor(r), action, r.PathValue("id"), nil)
	s.done(w, r, "/team", "Done.")
}

func (s *Server) revokeSession(w http.ResponseWriter, r *http.Request) {
	hash, err := hex.DecodeString(r.PathValue("hash"))
	if err != nil {
		s.fail(w, r, "/team", err)
		return
	}
	s.Fleet.Store.DeleteSession(r.Context(), hash)
	s.Fleet.Audit(r.Context(), s.actor(r), "team.session_end", "", nil)
	s.done(w, r, "/team", "The session is ended.")
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f := s.Fleet
	row := func(name, label, hint string, choices ...string) pages.SettingRow {
		env := map[string]string{"name": "FLEET_NAME", "events_days": "FLEET_EVENTS_DAYS", "log_addresses": "FLEET_LOG_ADDRESSES",
			"public_status": "FLEET_PUBLIC_STATUS"}[name]
		return pages.SettingRow{Name: name, Label: label, Hint: hint, Value: f.Setting(ctx, name), Pinned: core.Pinned(name),
			Env: env, Choices: choices}
	}
	rows := []pages.SettingRow{
		row("name", "The fleet's name", "On its pages, and in alerts."),
		row("default_channel", "The default release", "latest, or a build: for profiles that name none."),
		row("rolling_per_node", "Rolling waves", "Servers of a node restarted at once by a rolling job."),
		row("drain_deadline", "A drain's deadline", "How long a drain waits for a game's end (30m)."),
		row("events_days", "Events kept", "Days of events and games."),
		row("log_addresses", "Players' addresses", "The servers log them (HALO_SERVER_LOG_ADDRESSES): fleet bans need them.", "true", "false"),
		row("public_status", "A public status page", "/status: each server's name, map and players.", "false", "true"),
	}
	var u pages.UpdateView
	if s.Updater != nil {
		st := s.Updater.Status()
		u = pages.UpdateView{Current: st.Current, Mode: st.Mode, Error: st.Error, Available: st.Available, Updating: st.Updating,
			Checked: view.When(st.Checked, f.Location())}
		if st.Latest != nil {
			u.Latest = st.Latest.Version
		}
	} else {
		u = pages.UpdateView{Current: s.Version, Mode: "off"}
	}
	s.render(w, r, http.StatusOK, pages.Settings(s.page(r, "Settings", "settings"), rows, u))
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	for _, name := range []string{"name", "default_channel", "rolling_per_node", "drain_deadline", "events_days", "log_addresses",
		"public_status"} {
		values, ok := r.Form[name]
		if !ok || core.Pinned(name) {
			continue
		}
		value := strings.TrimSpace(values[0])
		if value == s.Fleet.Setting(r.Context(), name) {
			continue
		}
		if err := s.Fleet.SetSetting(r.Context(), s.actor(r), name, value); err != nil {
			s.fail(w, r, "/settings", err)
			return
		}
	}
	s.done(w, r, "/settings", "Saved.")
}

func (s *Server) startUpdate(w http.ResponseWriter, r *http.Request) {
	if s.Updater == nil {
		s.fail(w, r, "/settings", core.Refuse(http.StatusConflict, "off", "updates are off"))
		return
	}
	if err := s.Updater.Start(s.actor(r)); err != nil {
		s.fail(w, r, "/settings", err)
		return
	}
	s.Fleet.Audit(r.Context(), s.actor(r), "fleet.update", "", nil)
	s.done(w, r, "/settings", "Updating: the fleet restarts into the new release in a moment.")
}

func (s *Server) checkUpdate(w http.ResponseWriter, r *http.Request) {
	if s.Updater == nil {
		s.fail(w, r, "/settings", core.Refuse(http.StatusConflict, "off", "updates are off"))
		return
	}
	release, newer, err := s.Updater.CheckNow(r.Context(), s.actor(r))
	if err != nil {
		s.fail(w, r, "/settings", core.Refuse(http.StatusBadGateway, "unchecked",
			"The releases were not checked (%v): try again in a moment.", err))
		return
	}
	notice := "opence-fleet " + release.Version + " is the latest release: this fleet runs it."
	switch {
	case newer && s.Updater.Status().Updating:
		notice = "opence-fleet " + release.Version + " is out: the fleet updates to it now (FLEET_UPDATES=auto)."
	case newer:
		notice = "opence-fleet " + release.Version + " is out: update to it below."
	}
	s.done(w, r, "/settings", notice)
}

func (s *Server) publicStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.Fleet.Setting(ctx, "public_status") != "true" && !s.Fleet.Config.PublicStatus {
		http.NotFound(w, r)
		return
	}
	instances, _ := s.Fleet.Instances(ctx)
	var rows []pages.StatusRow
	for _, i := range instances {
		l := s.Fleet.Live.Instance(i.Slug)
		if l == nil || l.Status == nil {
			continue
		}
		st := l.Status
		listed := false
		for _, m := range st.Masters {
			listed = listed || m.Listed
		}
		rows = append(rows, pages.StatusRow{Name: st.Name, Map: st.Map, Gametype: firstNonEmpty(st.Variant, st.Gametype),
			Region: st.Region, State: st.State, Players: st.Players, MaxPlayers: st.MaxPlayers, Listed: listed})
	}
	page := s.page(r, "Status", "")
	s.render(w, r, http.StatusOK, pages.PublicStatus(page, rows))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
