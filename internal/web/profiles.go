package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view/pages"
)

func (s *Server) profilesPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	profiles, _ := s.Fleet.Profiles(ctx)
	missing := s.Fleet.Missing(ctx)
	var list []pages.ProfileLine
	for _, p := range profiles {
		n, _ := s.Fleet.Store.CountProfileFollowers(ctx, &p.ID)
		list = append(list, pages.ProfileLine{Profile: p, Followers: int(n), Missing: missing[p.Name]})
	}
	s.render(w, r, http.StatusOK, pages.Profiles(s.page(r, "Profiles", "profiles"), list))
}

func (s *Server) profilePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	e := pages.ProfileEdit{New: r.PathValue("id") == ""}
	if !e.New {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		p, err := s.Fleet.Profile(ctx, id)
		if err != nil {
			s.render(w, r, http.StatusNotFound, s.errorPage(r, "No such profile", ""))
			return
		}
		e.ID, e.Profile = id, p
		followers, _ := s.Fleet.Followers(ctx, id)
		for _, f := range followers {
			e.Followers = append(e.Followers, f.Slug)
		}
		e.Missing = s.Fleet.Missing(ctx)[p.Name]
	} else {
		e.Profile.Settings = map[string]any{}
		e.Profile.Gametypes = map[string]string{}
		e.Profile.Channel = "latest"
		e.Profile.PasswordPolicy = core.PasswordNone
	}
	keys, _ := s.Fleet.Keys(ctx)
	for _, k := range keys {
		e.Keys = append(e.Keys, pages.Option{Value: k.KeyID, Name: k.Label + " (" + k.KeyID + ")"})
	}
	e.Channels = s.channelOptions(r)
	title := "New profile"
	if !e.New {
		title = e.Profile.Name
	}
	s.render(w, r, http.StatusOK, pages.Profile(s.page(r, title, "profiles"), e))
}

// profileInput reads a profile's form: its settings are the fields named
// setting.<name> that are not empty.
func profileInput(r *http.Request) core.ProfileInput {
	r.ParseForm()
	in := core.ProfileInput{Name: r.FormValue("name"), Description: r.FormValue("description"), Rotation: r.FormValue("rotation"),
		Region: r.FormValue("region"), InitTxt: r.FormValue("init_txt"), Channel: r.FormValue("channel"),
		PasswordPolicy: r.FormValue("password_policy"), Password: r.FormValue("password"), KeyID: r.FormValue("key_id"),
		ApplyMode: r.FormValue("apply_mode"), Settings: map[string]any{}}
	for key, values := range r.Form {
		if name, ok := strings.CutPrefix(key, "setting."); ok && len(values) > 0 && strings.TrimSpace(values[0]) != "" {
			in.Settings[name] = values[0]
		}
	}
	return in
}

func (s *Server) saveProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	in := profileInput(r)
	if r.PathValue("id") == "" {
		id, err := s.Fleet.CreateProfile(ctx, s.actor(r), in)
		if err != nil {
			s.fail(w, r, "/profiles/new", err)
			return
		}
		s.done(w, r, "/profiles/"+strconv.FormatInt(id, 10), "The profile is made: make servers from it on the Servers page.")
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Fleet.UpdateProfile(ctx, s.actor(r), id, in); err != nil {
		s.fail(w, r, "/profiles/"+r.PathValue("id"), err)
		return
	}
	followers, _ := s.Fleet.Followers(ctx, id)
	s.done(w, r, "/profiles/"+r.PathValue("id"), "Saved, and sent to "+strconv.Itoa(len(followers))+" servers: they take it live where they can.")
}

func (s *Server) duplicateProfile(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	copyID, err := s.Fleet.DuplicateProfile(r.Context(), s.actor(r), id)
	if err != nil {
		s.fail(w, r, "/profiles/"+r.PathValue("id"), err)
		return
	}
	s.done(w, r, "/profiles/"+strconv.FormatInt(copyID, 10), "A copy is made.")
}

func (s *Server) deleteProfile(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Fleet.DeleteProfile(r.Context(), s.actor(r), id); err != nil {
		s.fail(w, r, "/profiles/"+r.PathValue("id"), err)
		return
	}
	s.done(w, r, "/profiles", "The profile is deleted.")
}

func (s *Server) profileGametype(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	name := strings.TrimSpace(r.FormValue("name"))
	if err := s.Fleet.SetProfileGametype(r.Context(), s.actor(r), id, name, r.FormValue("text")); err != nil {
		s.fail(w, r, "/profiles/"+r.PathValue("id"), err)
		return
	}
	s.done(w, r, "/profiles/"+r.PathValue("id"), name+".toml is saved and sent to the followers.")
}

func (s *Server) deleteProfileGametype(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	name := r.PathValue("name")
	if err := s.Fleet.DeleteProfileGametype(r.Context(), s.actor(r), id, name); err != nil {
		s.fail(w, r, "/profiles/"+r.PathValue("id"), err)
		return
	}
	s.done(w, r, "/profiles/"+r.PathValue("id"), name+".toml is deleted.")
}
