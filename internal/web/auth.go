package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/OpenCommunityEdition/opence-fleet/internal/auth"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view/pages"
)

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, token string, remember bool) {
	cookie := &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: s.Clients.Of(r).Secure}
	if remember {
		cookie.MaxAge = int((30 * 24 * time.Hour).Seconds())
	}
	http.SetCookie(w, cookie)
}

func safeNext(next string) string {
	if strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") && !strings.Contains(next, "\\") &&
		!strings.HasPrefix(next, "/login") {
		return next
	}
	return "/"
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.protected(r) {
		s.plainHTTP(w, r)
		return
	}
	if principal(r) != nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, pages.Login(s.page(r, "Sign in", ""), r.URL.Query().Get("next"), "", s.Auth.SetupOpen(r.Context())))
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.protected(r) {
		s.plainHTTP(w, r)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !s.sameOrigin(r, origin) {
		http.Error(w, "the sign-in came from another site", http.StatusForbidden)
		return
	}
	client := s.Clients.Of(r)
	name := strings.TrimSpace(r.FormValue("name"))
	token, p, err := s.Auth.Login(r.Context(), name, r.FormValue("password"), client.Key(), r.FormValue("remember") == "1")
	if err != nil {
		s.Fleet.Audit(r.Context(), "anonymous", "user.login_failed", name, map[string]string{"address": client.IP.String()})
		page := s.page(r, "Sign in", "")
		page.Error = err.Error()
		status := http.StatusUnauthorized
		if err == auth.ErrLocked {
			status = http.StatusTooManyRequests
		}
		s.render(w, r, status, pages.Login(page, r.FormValue("next"), name, false))
		return
	}
	s.setSession(w, r, token, r.FormValue("remember") == "1")
	s.Fleet.Audit(r.Context(), p.Actor(), "user.login", p.Name, nil)
	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.Auth.Logout(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if !s.protected(r) {
		s.plainHTTP(w, r)
		return
	}
	if !s.Auth.SetupOpen(r.Context()) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, pages.Setup(s.page(r, "Set up", ""), r.URL.Query().Get("code")))
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	if !s.protected(r) {
		s.plainHTTP(w, r)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !s.sameOrigin(r, origin) {
		http.Error(w, "the setup came from another site", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	token, p, err := s.Auth.Setup(ctx, r.FormValue("code"), strings.TrimSpace(r.FormValue("name")), r.FormValue("password"),
		s.Clients.Of(r).Key())
	if err != nil {
		page := s.page(r, "Set up", "")
		page.Error = err.Error()
		s.render(w, r, http.StatusBadRequest, pages.Setup(page, r.FormValue("code")))
		return
	}
	if name := strings.TrimSpace(r.FormValue("fleet")); name != "" && len(name) <= 64 {
		s.Fleet.SetSetting(ctx, p.Actor(), "name", name)
	}
	s.Fleet.Audit(ctx, p.Actor(), "user.setup", p.Name, nil)
	s.setSession(w, r, token, false)
	s.setFlash(w, r, "fleet_notice", "Welcome. Add a node to run servers on, then a profile and servers from it.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) invitePage(w http.ResponseWriter, r *http.Request) {
	if !s.protected(r) {
		s.plainHTTP(w, r)
		return
	}
	invite := s.Auth.Invite(r.Context(), r.PathValue("code"))
	if invite == nil {
		s.render(w, r, http.StatusNotFound, pages.Problem(s.page(r, "Invite", ""), "This invite does not work",
			"It was used, or is more than 7 days old: ask for another."))
		return
	}
	s.render(w, r, http.StatusOK, pages.Invite(s.page(r, "Invite", ""), r.PathValue("code"), invite.Role))
}

func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	if !s.protected(r) {
		s.plainHTTP(w, r)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !s.sameOrigin(r, origin) {
		http.Error(w, "the form came from another site", http.StatusForbidden)
		return
	}
	code := r.PathValue("code")
	token, p, err := s.Auth.AcceptInvite(r.Context(), code, strings.TrimSpace(r.FormValue("name")), r.FormValue("password"),
		s.Clients.Of(r).Key())
	if err != nil {
		page := s.page(r, "Invite", "")
		page.Error = err.Error()
		role := ""
		if invite := s.Auth.Invite(r.Context(), code); invite != nil {
			role = invite.Role
		}
		s.render(w, r, http.StatusBadRequest, pages.Invite(page, code, role))
		return
	}
	s.Fleet.Audit(r.Context(), p.Actor(), "user.join", p.Name, map[string]string{"role": p.Role})
	s.setSession(w, r, token, false)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) theme(w http.ResponseWriter, r *http.Request) {
	next := "light"
	if c, err := r.Cookie("fleet_theme"); err == nil && c.Value == "light" {
		next = "dark"
	} else if err != nil && r.FormValue("current") == "light" {
		next = "dark"
	}
	http.SetCookie(w, &http.Cookie{Name: "fleet_theme", Value: next, Path: "/", MaxAge: 365 * 24 * 3600,
		SameSite: http.SameSiteLaxMode, Secure: s.Clients.Of(r).Secure})
	http.Redirect(w, r, back(r, "/"), http.StatusSeeOther)
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, pages.Account(s.page(r, "Your account", "")))
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p.Kind != "session" {
		s.fail(w, r, "/account", errorText("an API token cannot change a password"))
		return
	}
	if err := s.Auth.ChangePassword(r.Context(), p, r.FormValue("old"), r.FormValue("password")); err != nil {
		s.fail(w, r, "/account", err)
		return
	}
	s.Fleet.Audit(r.Context(), p.Actor(), "user.password", p.Name, "changed")
	s.done(w, r, "/account", "Your password is changed; your other sessions are ended.")
}

func (s *Server) morePage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var sections [][3]string
	for _, n := range []struct{ name, path, icon, role string }{
		{"Nodes", "/nodes", "hard-drive", auth.Viewer}, {"Profiles", "/profiles", "layers", auth.Viewer},
		{"Library", "/library", "library", auth.Viewer}, {"Releases", "/releases", "rocket", auth.Viewer},
		{"Masters", "/masters", "radio-tower", auth.Admin}, {"Bans", "/bans", "ban", auth.Moderator},
		{"Jobs", "/jobs", "list-checks", auth.Viewer}, {"Schedules", "/schedules", "calendar-clock", auth.Viewer},
		{"Audit", "/audit", "scroll-text", auth.Admin}, {"Team", "/team", "users-round", auth.Admin},
		{"Settings", "/settings", "settings", auth.Owner},
	} {
		if p.Can(n.role) {
			sections = append(sections, [3]string{n.name, n.path, n.icon})
		}
	}
	s.render(w, r, http.StatusOK, pages.More(s.page(r, "More", "more"), sections))
}

// errorPage is a page that says what went wrong.
func (s *Server) errorPage(r *http.Request, title, words string) templ.Component {
	return pages.Problem(s.page(r, title, ""), title, words)
}

type errorText string

func (e errorText) Error() string { return string(e) }
