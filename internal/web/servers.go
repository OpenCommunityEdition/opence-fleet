package web

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/auth"
	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view/pages"
)

func (s *Server) profileOptions(r *http.Request) []pages.Option {
	profiles, _ := s.Fleet.Store.ListProfiles(r.Context())
	var out []pages.Option
	for _, p := range profiles {
		out = append(out, pages.Option{Value: strconv.FormatInt(p.ID, 10), Name: p.Name})
	}
	return out
}

func (s *Server) channelOptions(r *http.Request) []string {
	releases, _ := s.Fleet.Releases(r.Context())
	var out []string
	for _, rel := range releases {
		out = append(out, rel.Build)
	}
	return out
}

func (s *Server) groupNames(r *http.Request) []string {
	groups, _ := s.Fleet.Store.ListGroups(r.Context())
	var out []string
	for _, g := range groups {
		out = append(out, g.Name)
	}
	return out
}

func (s *Server) serversPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := s.data(ctx)
	q := r.URL.Query()
	f := pages.Filters{Node: q.Get("node"), Profile: q.Get("profile"), Group: q.Get("group"), State: q.Get("state"),
		Search: strings.TrimSpace(q.Get("q")), Groups: s.groupNames(r)}
	for _, n := range d.nodes {
		f.Nodes = append(f.Nodes, n.Name)
	}
	sort.Strings(f.Nodes)
	for _, name := range d.profiles {
		f.Profiles = append(f.Profiles, name)
	}
	sort.Strings(f.Profiles)
	var rows []view.ServerRow
	for _, i := range d.instances {
		row := d.row(s, i)
		switch {
		case f.Node != "" && row.Node != f.Node:
			continue
		case f.Profile != "" && row.Profile != f.Profile:
			continue
		case f.Group != "" && !contains(i.Groups, f.Group):
			continue
		case f.State != "" && !strings.Contains(row.State, f.State):
			continue
		case f.Search != "" && !strings.Contains(strings.ToLower(i.Name+" "+i.Slug), strings.ToLower(f.Search)):
			continue
		}
		rows = append(rows, row)
	}
	page := s.page(r, "Servers", "servers")
	page.Live = true
	s.render(w, r, http.StatusOK, pages.Servers(page, rows, f, s.profileOptions(r), s.channelOptions(r), len(d.instances)))
}

func contains(list []string, x string) bool {
	for _, item := range list {
		if item == x {
			return true
		}
	}
	return false
}

func (s *Server) newServersPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	nodes, _ := s.Fleet.Nodes(ctx)
	var names []string
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	warning := ""
	if _, err := s.Fleet.ResolveChannel(ctx, ""); err != nil {
		warning = err.Error() + ". The servers are made, and start once a release is known."
	}
	s.render(w, r, http.StatusOK, pages.NewServers(s.page(r, "New servers", "servers"), s.profileOptions(r), names,
		s.channelOptions(r), s.groupNames(r), warning))
}

func (s *Server) createServers(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	profile, _ := strconv.ParseInt(r.FormValue("profile"), 10, 64)
	count, _ := strconv.Atoi(r.FormValue("count"))
	var groups []string
	for _, g := range strings.Split(r.FormValue("groups"), ",") {
		if g = strings.TrimSpace(g); g != "" {
			groups = append(groups, g)
		}
	}
	created, warnings, err := s.Fleet.CreateInstances(r.Context(), s.actor(r), core.CreateRequest{ProfileID: profile,
		Nodes: r.Form["node"], Count: count, Pattern: r.FormValue("pattern"), Groups: groups,
		Running: r.FormValue("start") == "1", Channel: r.FormValue("channel")})
	if err != nil {
		s.fail(w, r, "/servers/new", err)
		return
	}
	notice := "Made " + view.Count(len(created), "server", "servers") + ": " + strings.Join(created, ", ") + "."
	if len(warnings) > 0 {
		notice += " " + strings.Join(warnings, " ")
	}
	if len(created) == 1 {
		s.done(w, r, "/servers/"+created[0], notice)
		return
	}
	s.done(w, r, "/servers", notice)
}

// bulk starts a job of a bulk action over the selection (or every server).
func (s *Server) bulk(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	p := principal(r)
	action := r.FormValue("action")
	if !p.Can(auth.Admin) && action != core.ActionSay && action != core.ActionNext && action != core.ActionEndGame {
		s.fail(w, r, back(r, "/servers"), core.Refuse(http.StatusForbidden, "forbidden", "that action needs the admin role"))
		return
	}
	slugs := r.Form["slug"]
	if r.FormValue("all") == "1" {
		instances, _ := s.Fleet.Instances(r.Context())
		for _, i := range instances {
			slugs = append(slugs, i.Slug)
		}
	}
	if g := r.FormValue("group"); g != "" && len(slugs) == 0 {
		slugs, _ = s.Fleet.GroupMembers(r.Context(), g)
	}
	profile, _ := strconv.ParseInt(r.FormValue("profile"), 10, 64)
	params := core.JobParams{Action: action, Slugs: slugs, Text: r.FormValue("text"), ProfileID: profile,
		Channel: r.FormValue("channel"), After: r.FormValue("after"), Notice: r.FormValue("notice"), Role: p.Role}
	if d, err := time.ParseDuration(r.FormValue("deadline")); err == nil {
		params.Deadline = int(d / time.Second)
	}
	id, err := s.Fleet.StartJob(r.Context(), s.actor(r), params)
	if err != nil {
		s.fail(w, r, back(r, "/servers"), err)
		return
	}
	if action == core.ActionSay && r.FormValue("all") == "1" {
		s.done(w, r, back(r, "/"), "Said on "+view.Count(len(slugs), "server", "servers")+".")
		return
	}
	s.done(w, r, "/jobs/"+strconv.FormatInt(id, 10), "The job runs.")
}
