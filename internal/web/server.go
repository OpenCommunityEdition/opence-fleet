// Package web is the control plane's HTTP server: the pages (templ, from
// package view), the forms that post and redirect (no JavaScript needed),
// the live stream (/events, server-sent events of re-rendered fragments),
// the console's WebSocket, the agents' endpoints (/agent, the enrolment, the
// library's blobs, the releases' zips, agent.sh), /api/v1 for the command
// line and scripts, /metrics and /healthz. Everything that changes state
// goes through package core with the signed-in user as its actor.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/OpenCommunityEdition/opence-fleet/internal/auth"
	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
	"github.com/OpenCommunityEdition/opence-fleet/internal/httpx"
	"github.com/OpenCommunityEdition/opence-fleet/internal/limit"
	"github.com/OpenCommunityEdition/opence-fleet/internal/netaddr"
	"github.com/OpenCommunityEdition/opence-fleet/internal/update"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view"
)

// Static is the pages' files (static/, embedded by the command).
var Static fs.FS

// Server is the HTTP server.
type Server struct {
	Fleet   *core.Fleet
	Auth    *auth.Auth
	Clients *httpx.Clients
	Log     *slog.Logger
	Version string
	Updater *update.Updater
	// the agents' installer's template (agent.sh)
	AgentScript []byte

	anonymous *limit.Limiter
	signed    *limit.Limiter
}

// New makes the server.
func New(f *core.Fleet, a *auth.Auth, clients *httpx.Clients, log *slog.Logger, version string) *Server {
	return &Server{Fleet: f, Auth: a, Clients: clients, Log: log, Version: version,
		anonymous: limit.PerMinute(60, nil).Grouped(netaddr.Group), signed: limit.PerMinute(600, nil).Grouped(netaddr.Group)}
}

type contextKey int

const principalKey contextKey = 0

// principal is the request's signed-in user (nil: none).
func principal(r *http.Request) *auth.Principal {
	p, _ := r.Context().Value(principalKey).(*auth.Principal)
	return p
}

// sessionCookie is the session's cookie's name.
const sessionCookie = "fleet_session"

// Handler is the server's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	page := func(pattern, role string, h http.HandlerFunc) { mux.Handle(pattern, s.gate(role, h)) }
	form := func(pattern, role string, h http.HandlerFunc) { mux.Handle(pattern, s.gate(role, s.checkForm(h))) }

	mux.Handle("GET /static/", s.static())
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /status", s.publicStatus)

	// signing in
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("GET /setup", s.setupPage)
	mux.HandleFunc("POST /setup", s.setup)
	mux.HandleFunc("GET /invite/{code}", s.invitePage)
	mux.HandleFunc("POST /invite/{code}", s.acceptInvite)
	form("POST /logout", auth.Viewer, s.logout)
	form("POST /theme", auth.Viewer, s.theme)
	page("GET /account", auth.Viewer, s.accountPage)
	form("POST /account/password", auth.Viewer, s.changePassword)
	page("GET /more", auth.Viewer, s.morePage)

	// the pages
	page("GET /{$}", auth.Viewer, s.dashboard)
	page("GET /servers", auth.Viewer, s.serversPage)
	page("GET /servers/new", auth.Admin, s.newServersPage)
	form("POST /servers", auth.Admin, s.createServers)
	form("POST /servers/bulk", auth.Moderator, s.bulk)
	page("GET /servers/{slug}", auth.Viewer, s.serverPage)
	page("GET /servers/{slug}/{tab}", auth.Viewer, s.serverPage)
	page("GET /servers/{slug}/fragment/{part}", auth.Viewer, s.serverFragment)
	form("POST /servers/{slug}/start", auth.Admin, s.serverLifecycle)
	form("POST /servers/{slug}/stop", auth.Admin, s.serverLifecycle)
	form("POST /servers/{slug}/restart", auth.Admin, s.serverLifecycle)
	form("POST /servers/{slug}/drain", auth.Admin, s.serverLifecycle)
	form("POST /servers/{slug}/delete", auth.Admin, s.deleteServer)
	form("POST /servers/{slug}/edit", auth.Admin, s.editServer)
	form("POST /servers/{slug}/setting", auth.Admin, s.serverSetting)
	form("POST /servers/{slug}/rotate-password", auth.Admin, s.rotatePassword)
	form("POST /servers/{slug}/say", auth.Moderator, s.serverSay)
	form("POST /servers/{slug}/next", auth.Moderator, s.serverNext)
	form("POST /servers/{slug}/rotation", auth.Admin, s.serverRotation)
	form("POST /servers/{slug}/play", auth.Moderator, s.serverPlay)
	form("POST /servers/{slug}/gametypes/{name}", auth.Admin, s.serverGametype)
	form("POST /servers/{slug}/players/{slot}/{action}", auth.Moderator, s.playerAction)
	form("POST /servers/{slug}/bans/{n}/delete", auth.Admin, s.serverUnban)
	form("POST /servers/{slug}/command", auth.Moderator, s.serverCommand)
	form("POST /servers/{slug}/groups", auth.Admin, s.serverGroups)
	mux.Handle("GET /ws/console/{slug}", s.gate(auth.Moderator, http.HandlerFunc(s.console)))

	page("GET /nodes", auth.Viewer, s.nodesPage)
	page("GET /nodes/add", auth.Admin, s.addNodePage)
	form("POST /nodes/add", auth.Admin, s.addNode)
	page("GET /nodes/{name}", auth.Viewer, s.nodePage)
	form("POST /nodes/{name}/settings", auth.Admin, s.nodeSettings)
	form("POST /nodes/{name}/forget", auth.Admin, s.forgetNode)
	form("POST /nodes/{name}/update", auth.Owner, s.updateAgent)

	page("GET /profiles", auth.Viewer, s.profilesPage)
	page("GET /profiles/new", auth.Admin, s.profilePage)
	form("POST /profiles", auth.Admin, s.saveProfile)
	page("GET /profiles/{id}", auth.Viewer, s.profilePage)
	form("POST /profiles/{id}", auth.Admin, s.saveProfile)
	form("POST /profiles/{id}/duplicate", auth.Admin, s.duplicateProfile)
	form("POST /profiles/{id}/delete", auth.Admin, s.deleteProfile)
	form("POST /profiles/{id}/gametypes", auth.Admin, s.profileGametype)
	form("POST /profiles/{id}/gametypes/{name}/delete", auth.Admin, s.deleteProfileGametype)

	page("GET /players", auth.Viewer, s.playersPage)
	page("GET /players/fragment", auth.Viewer, s.playersFragment)
	page("GET /bans", auth.Moderator, s.bansPage)
	form("POST /bans", auth.Admin, s.addBan)
	form("POST /bans/import", auth.Admin, s.importBans)
	form("POST /bans/{id}/delete", auth.Admin, s.unban)

	page("GET /library", auth.Viewer, s.libraryPage)
	form("POST /library/upload", auth.Admin, s.uploadItem)
	form("POST /library/folder", auth.Owner, s.addFolder)
	form("POST /library/{sha}/delete", auth.Admin, s.removeItem)
	mux.Handle("POST /library/uploads", s.gate(auth.Admin, s.checkForm(http.HandlerFunc(s.startUpload))))
	mux.Handle("PUT /library/uploads/{id}", s.gate(auth.Admin, s.checkForm(http.HandlerFunc(s.uploadChunk))))
	mux.Handle("POST /library/uploads/{id}/finish", s.gate(auth.Admin, s.checkForm(http.HandlerFunc(s.finishUpload))))

	page("GET /releases", auth.Viewer, s.releasesPage)
	form("POST /releases/check", auth.Admin, s.checkLatest)
	form("POST /releases/fetch", auth.Admin, s.fetchRelease)
	form("POST /releases/local", auth.Admin, s.localRelease)
	form("POST /releases/{build}/zip", auth.Admin, s.fetchZip)
	form("POST /releases/{build}/latest", auth.Admin, s.setLatest)
	form("POST /releases/{build}/forget", auth.Admin, s.forgetRelease)
	form("POST /releases/update", auth.Admin, s.updateServers)

	page("GET /masters", auth.Admin, s.mastersPage)
	form("POST /masters/keys", auth.Owner, s.makeKey)
	form("POST /masters/keys/import", auth.Owner, s.importKey)
	form("POST /masters/keys/{id}/default", auth.Owner, s.defaultKey)
	form("POST /masters/keys/{id}/forget", auth.Owner, s.forgetKey)
	form("POST /masters/keys/{id}/sign", auth.Owner, s.signNonce)
	form("POST /masters/keys/{id}/refresh", auth.Admin, s.refreshMe)

	page("GET /jobs", auth.Viewer, s.jobsPage)
	page("GET /jobs/{id}", auth.Viewer, s.jobPage)
	page("GET /jobs/{id}/fragment", auth.Viewer, s.jobFragment)
	form("POST /jobs/{id}/cancel", auth.Admin, s.cancelJob)
	page("GET /schedules", auth.Viewer, s.schedulesPage)
	form("POST /schedules", auth.Admin, s.saveSchedule)
	form("POST /schedules/{id}", auth.Admin, s.saveSchedule)
	form("POST /schedules/{id}/run", auth.Admin, s.runSchedule)
	form("POST /schedules/{id}/delete", auth.Admin, s.deleteSchedule)
	page("GET /alerts", auth.Viewer, s.alertsPage)
	form("POST /alerts/{id}/acknowledge", auth.Moderator, s.acknowledgeAlert)
	form("POST /alerts/{id}/resolve", auth.Admin, s.resolveAlert)
	form("POST /webhooks", auth.Admin, s.addWebhook)
	form("POST /webhooks/{id}/test", auth.Admin, s.testWebhook)
	form("POST /webhooks/{id}/delete", auth.Admin, s.deleteWebhook)
	page("GET /audit", auth.Admin, s.auditPage)
	page("GET /audit.json", auth.Admin, s.auditExport)
	page("GET /team", auth.Admin, s.teamPage)
	form("POST /team/invites", auth.Admin, s.makeInvite)
	form("POST /team/tokens", auth.Admin, s.makeToken)
	form("POST /team/tokens/{id}/delete", auth.Admin, s.deleteToken)
	form("POST /team/users/{id}/role", auth.Admin, s.setRole)
	form("POST /team/users/{id}/disable", auth.Admin, s.disableUser)
	form("POST /team/sessions/{hash}/delete", auth.Admin, s.revokeSession)
	page("GET /settings", auth.Owner, s.settingsPage)
	form("POST /settings", auth.Owner, s.saveSettings)
	form("POST /settings/update", auth.Owner, s.startUpdate)

	// the live stream
	mux.Handle("GET /events", s.gate(auth.Viewer, http.HandlerFunc(s.events)))

	// the agents
	mux.Handle("GET /agent", s.agentGate(s.Fleet.Hub))
	mux.HandleFunc("POST /agent/join", s.join)
	mux.HandleFunc("GET /agent/blobs/{sha}", s.blob)
	mux.HandleFunc("GET /agent/releases/{file}", s.releaseZip)
	mux.HandleFunc("GET /agent.sh", s.agentScript)

	// the API
	s.api(mux)

	return s.headers(s.limit(s.session(mux)))
}

// headers adds what every answer has.
func (s *Server) headers(next http.Handler) http.Handler {
	return s.Clients.HSTS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	}))
}

// limit counts each address's requests: 60 a minute without a session, 600
// with one (the agents' and the static files' are not counted).
func (s *Server) limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasPrefix(path, "/static/") || strings.HasPrefix(path, "/agent") || path == "/healthz" || path == "/events" ||
			strings.HasPrefix(path, "/library/uploads") {
			next.ServeHTTP(w, r)
			return
		}
		key := s.Clients.Of(r).Key()
		limiter := s.anonymous
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			limiter = s.signed
		} else if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ocf_") {
			limiter = s.signed
		}
		if ok, wait := limiter.Allow(key); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
			httpx.Fail(w, http.StatusTooManyRequests, "rate_limited", "too many requests: wait a moment", "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// session finds the request's principal: the session's cookie, or an API
// token (Authorization: Bearer ocf_…).
func (s *Server) session(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p *auth.Principal
		if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && strings.HasPrefix(bearer, "ocf_") {
			p = s.Auth.Token(r.Context(), strings.TrimSpace(bearer))
		} else if c, err := r.Cookie(sessionCookie); err == nil {
			p = s.Auth.Session(r.Context(), c.Value)
		}
		if p != nil {
			r = r.WithContext(context.WithValue(r.Context(), principalKey, p))
		}
		next.ServeHTTP(w, r)
	})
}

// protected tells whether a request may carry secrets (TLS, or loopback).
func (s *Server) protected(r *http.Request) bool { return s.Clients.Of(r).Protected() }

// gate lets a request through for a role: a page sends one without a
// session to the sign-in; plain HTTP from beyond loopback is refused.
func (s *Server) gate(role string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.protected(r) {
			s.plainHTTP(w, r)
			return
		}
		p := principal(r)
		if p == nil {
			if wantsJSON(r) || r.Header.Get("HX-Request") != "" || r.URL.Path == "/events" {
				httpx.Fail(w, http.StatusUnauthorized, "denied", "sign in first", "")
				return
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		if !p.Can(role) {
			if wantsJSON(r) {
				httpx.Fail(w, http.StatusForbidden, "forbidden", "needs the "+role+" role", "")
				return
			}
			s.render(w, r, http.StatusForbidden, s.errorPage(r, "Not for your role", "This page needs the "+role+" role; yours is "+p.Role+"."))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkForm checks a state-changing request of a session: its CSRF token
// (the form's csrf, or the X-CSRF-Token header) and its Origin. An API
// token needs neither (no browser sends it by itself).
func (s *Server) checkForm(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principal(r)
		if p != nil && p.Kind == "session" {
			if origin := r.Header.Get("Origin"); origin != "" && !s.sameOrigin(r, origin) {
				httpx.Fail(w, http.StatusForbidden, "origin", "the request came from another site", "")
				return
			}
			token := r.Header.Get("X-CSRF-Token")
			if token == "" {
				// (a multipart form's field is read with its body)
				if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
					r.ParseMultipartForm(64 << 20)
				}
				token = r.FormValue("csrf")
			}
			if token == "" || token != p.CSRF {
				if wantsJSON(r) {
					httpx.Fail(w, http.StatusForbidden, "csrf", "the form is stale: reload the page", "")
					return
				}
				s.render(w, r, http.StatusForbidden, s.errorPage(r, "The form is stale", "Reload the page and send it again."))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin tells whether an Origin is the fleet's own.
func (s *Server) sameOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if s.Fleet.PublicURL != "" {
		if p, err := url.Parse(s.Fleet.PublicURL); err == nil && strings.EqualFold(p.Host, u.Host) {
			return true
		}
	}
	return strings.EqualFold(u.Host, s.Clients.Host(r))
}

func (s *Server) plainHTTP(w http.ResponseWriter, r *http.Request) {
	httpx.Fail(w, http.StatusForbidden, "tls_required", "the fleet takes sign-ins and changes over HTTPS, or from this "+
		"machine's loopback: put it behind a TLS proxy (FLEET_TRUSTED_PROXIES) or set FLEET_TLS_CERT", "")
}

func wantsJSON(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/api/") || strings.Contains(r.Header.Get("Accept"), "application/json") ||
		strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
}

func (s *Server) static() http.Handler {
	files := http.FileServerFS(Static)
	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// (the files change with a release, whose binary embeds them: an
		// hour's cache, revalidated)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		if strings.HasSuffix(r.URL.Path, ".svg") {
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		}
		files.ServeHTTP(w, r)
	}))
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.Fleet.Store.DB.PingContext(ctx); err != nil {
		httpx.JSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "the database does not answer"})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.Version})
}

// page makes a page's common fields.
func (s *Server) page(r *http.Request, title, section string) view.Page {
	ctx := r.Context()
	p := principal(r)
	page := view.Page{Title: title, Section: section, Fleet: s.Fleet.Name(ctx), Version: s.Version, User: p,
		Path: r.URL.Path, Now: s.Fleet.Now(), Zone: s.Fleet.Location()}
	if p != nil {
		page.CSRF = p.CSRF
	}
	if c, err := r.Cookie("fleet_theme"); err == nil && (c.Value == "light" || c.Value == "dark") {
		page.Theme = c.Value
	}
	if n, err := s.Fleet.Store.CountInstances(ctx); err == nil {
		page.Servers = int(n)
	}
	page.Players = s.Fleet.Live.Players()
	if open, err := s.Fleet.Store.ListOpenAlerts(ctx); err == nil {
		page.Alerts = len(open)
	}
	if s.Updater != nil && p.Can(auth.Owner) {
		if st := s.Updater.Status(); st.Available && st.Latest != nil {
			page.UpdateBanner = st.Latest.Version
		}
	}
	page.Notice, page.Error = s.flash(r)
	return page
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	// (the flash a page showed is shown once)
	for _, name := range []string{"fleet_notice", "fleet_error"} {
		if _, err := r.Cookie(name); err == nil {
			http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1})
		}
	}
	view.Render(w, r, status, c)
}

// flash reads (and clears) an action's outcome.
func (s *Server) flash(r *http.Request) (string, string) {
	notice, err := "", ""
	if c, e := r.Cookie("fleet_notice"); e == nil {
		notice, _ = url.QueryUnescape(c.Value)
	}
	if c, e := r.Cookie("fleet_error"); e == nil {
		err, _ = url.QueryUnescape(c.Value)
	}
	return notice, err
}

func (s *Server) setFlash(w http.ResponseWriter, r *http.Request, name, text string) {
	if len(text) > 1000 {
		text = text[:1000]
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: url.QueryEscape(text), Path: "/", MaxAge: 20, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.Clients.Of(r).Secure})
}

// clearFlash is the page's clearing of the flash it showed.
func clearFlash(w http.ResponseWriter) {
	for _, name := range []string{"fleet_notice", "fleet_error"} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1})
	}
}

// done answers a form: a redirect with its notice, or JSON.
func (s *Server) done(w http.ResponseWriter, r *http.Request, back, notice string) {
	if wantsJSON(r) {
		httpx.JSON(w, http.StatusOK, map[string]any{"ok": true, "message": notice})
		return
	}
	if notice != "" {
		s.setFlash(w, r, "fleet_notice", notice)
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// fail answers a form's refusal: back with its words, or JSON.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, back string, err error) {
	status, code := http.StatusBadRequest, "failed"
	if e, ok := core.AsError(err); ok {
		status, code = e.Status, e.Code
		if status == 0 {
			status = http.StatusBadRequest
		}
	} else {
		var e *core.Error
		if !errors.As(err, &e) {
			s.Log.Warn("a request failed", "path", r.URL.Path, "err", err)
		}
	}
	if wantsJSON(r) {
		httpx.Fail(w, status, code, err.Error(), "")
		return
	}
	s.setFlash(w, r, "fleet_error", err.Error())
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// back is where a form returns: its "back" field when it is the fleet's
// own path, else the fallback.
func back(r *http.Request, fallback string) string {
	b := r.FormValue("back")
	if strings.HasPrefix(b, "/") && !strings.HasPrefix(b, "//") && !strings.Contains(b, "\\") {
		return b
	}
	return fallback
}

func (s *Server) actor(r *http.Request) string { return principal(r).Actor() }

// readJSON decodes a JSON body (1 MB at most).
func readJSON(r *http.Request, out any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := decoder.Decode(out); err != nil {
		return core.Refuse(http.StatusBadRequest, "schema", "the body is not the JSON expected: %v", err)
	}
	return nil
}
