package web_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/auth"
	"github.com/OpenCommunityEdition/opence-fleet/internal/config"
	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
	"github.com/OpenCommunityEdition/opence-fleet/internal/crypt"
	"github.com/OpenCommunityEdition/opence-fleet/internal/httpx"
	"github.com/OpenCommunityEdition/opence-fleet/internal/live"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store"
	"github.com/OpenCommunityEdition/opence-fleet/internal/web"
	"github.com/OpenCommunityEdition/opence-fleet/static"
)

// The addresses requests come from.
const (
	loopback = "127.0.0.1:40000"
	// a client beyond loopback, over plain HTTP
	remote = "203.0.113.7:40000"
	// a trusted proxy (trustedProxies)
	proxy = "10.1.2.3:40000"
)

var trustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}

const pageCSP = "default-src 'self'; style-src 'self'; img-src 'self' data:; frame-ancestors 'none'; form-action 'self'; base-uri 'none'"

// fixture is a control plane in-process: its handler, its fleet, its
// accounts and (with an owner) the owner's session.
type fixture struct {
	t       *testing.T
	fleet   *core.Fleet
	auth    *auth.Auth
	handler http.Handler
	owner   *auth.Principal
	cookie  string
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func newFixture(t *testing.T, clients *httpx.Clients, withOwner bool) *fixture {
	t.Helper()
	work := t.TempDir()
	c := &config.ServeConfig{Name: "Test fleet", Data: filepath.Join(work, "data"), DB: filepath.Join(work, "fleet.db"),
		Image: "opence-server", ReleaseRepository: "x/y", ReleaseCheckInterval: time.Hour, EventsDays: 30, SamplesDays: 7,
		Updates: "off", Timezone: time.UTC, SessionHours: 12}
	s, err := store.Open(c.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := core.New(s, crypt.New(), c, quiet(), live.New())
	a := auth.New(s, 12)
	if clients == nil {
		clients = &httpx.Clients{}
	}
	server := web.New(f, a, clients, quiet(), "dev-test")
	web.Static = static.Files
	x := &fixture{t: t, fleet: f, auth: a, handler: server.Handler()}
	if withOwner {
		ctx := context.Background()
		code, err := a.SetupCode(ctx)
		if err != nil {
			t.Fatal(err)
		}
		x.cookie, x.owner, err = a.Setup(ctx, code, "paul", "a long password", "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	return x
}

// request makes a request from an address (host:port).
func request(method, path, from string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, path, body)
	r.RemoteAddr = from
	if body != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return r
}

func form(values url.Values) io.Reader { return strings.NewReader(values.Encode()) }

func (x *fixture) do(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	x.handler.ServeHTTP(w, r)
	return w
}

// signed adds a session's cookie.
func signed(r *http.Request, token string) *http.Request {
	r.AddCookie(&http.Cookie{Name: "fleet_session", Value: token})
	return r
}

// bearer adds an API token.
func bearer(r *http.Request, token string) *http.Request {
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

// user makes a user of a role (by an invite): its session's token.
func (x *fixture) user(role string) string {
	x.t.Helper()
	ctx := context.Background()
	code, err := x.auth.MakeInvite(ctx, x.owner, role, "")
	if err != nil {
		x.t.Fatal(err)
	}
	token, _, err := x.auth.AcceptInvite(ctx, code, role+"-user", "a long password", "127.0.0.2")
	if err != nil {
		x.t.Fatal(err)
	}
	return token
}

// apiToken makes an API token of a role.
func (x *fixture) apiToken(role string) string {
	x.t.Helper()
	token, err := x.auth.MakeToken(context.Background(), x.owner, "test "+role, role)
	if err != nil {
		x.t.Fatal(err)
	}
	return token
}

// errorCode is a JSON error's code.
func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body httpx.ErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("not a JSON error (%d): %q", w.Code, w.Body.String())
	}
	return body.Error.Code
}

func header(t *testing.T, w *httptest.ResponseRecorder, name, want string) {
	t.Helper()
	got := w.Header().Values(name)
	if len(got) != 1 || got[0] != want {
		t.Errorf("%s: %q, want %q", name, got, want)
	}
}

func TestSecurityHeaders(t *testing.T) {
	x := newFixture(t, &httpx.Clients{Proxies: trustedProxies}, true)

	t.Run("a page", func(t *testing.T) {
		w := x.do(signed(request(http.MethodGet, "/servers", loopback, nil), x.cookie))
		if w.Code != http.StatusOK {
			t.Fatalf("%d", w.Code)
		}
		header(t, w, "Content-Security-Policy", pageCSP)
		header(t, w, "X-Content-Type-Options", "nosniff")
		header(t, w, "Referrer-Policy", "same-origin")
		header(t, w, "X-Frame-Options", "DENY")
		header(t, w, "Cache-Control", "no-store")
		header(t, w, "Content-Type", "text/html; charset=utf-8")
		if hsts := w.Header().Get("Strict-Transport-Security"); hsts != "" {
			t.Errorf("HSTS over plain HTTP: %q", hsts)
		}
	})

	t.Run("the sign-in page, signed out", func(t *testing.T) {
		w := x.do(request(http.MethodGet, "/login", loopback, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%d", w.Code)
		}
		header(t, w, "Content-Security-Policy", pageCSP)
		header(t, w, "X-Frame-Options", "DENY")
	})

	t.Run("a refusal's page", func(t *testing.T) {
		w := x.do(signed(request(http.MethodPost, "/theme", loopback, form(url.Values{"csrf": {"wrong"}})), x.cookie))
		if w.Code != http.StatusForbidden {
			t.Fatalf("%d", w.Code)
		}
		header(t, w, "Content-Security-Policy", pageCSP)
		header(t, w, "X-Frame-Options", "DENY")
	})

	t.Run("the API", func(t *testing.T) {
		w := x.do(bearer(request(http.MethodGet, "/api/v1/me", loopback, nil), x.apiToken(auth.Viewer)))
		if w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		header(t, w, "Content-Type", "application/json")
		header(t, w, "Cache-Control", "no-store")
		header(t, w, "X-Content-Type-Options", "nosniff")
		header(t, w, "Referrer-Policy", "same-origin")
	})

	t.Run("the API's refusal", func(t *testing.T) {
		w := x.do(request(http.MethodGet, "/api/v1/me", loopback, nil))
		if w.Code != http.StatusUnauthorized || errorCode(t, w) != "denied" {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		header(t, w, "X-Content-Type-Options", "nosniff")
		header(t, w, "Referrer-Policy", "same-origin")
		header(t, w, "Cache-Control", "no-store")
	})

	t.Run("an SVG", func(t *testing.T) {
		w := x.do(request(http.MethodGet, "/static/favicon.svg", remote, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%d", w.Code)
		}
		header(t, w, "Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		header(t, w, "Cache-Control", "public, max-age=3600")
		header(t, w, "X-Content-Type-Options", "nosniff")
	})

	t.Run("HSTS over a trusted proxy's TLS alone", func(t *testing.T) {
		r := request(http.MethodGet, "/healthz", proxy, nil)
		r.Header.Set("X-Forwarded-Proto", "https")
		header(t, x.do(r), "Strict-Transport-Security", "max-age=31536000")
		r = request(http.MethodGet, "/healthz", remote, nil)
		r.Header.Set("X-Forwarded-Proto", "https")
		if hsts := x.do(r).Header().Get("Strict-Transport-Security"); hsts != "" {
			t.Errorf("an untrusted X-Forwarded-Proto set HSTS: %q", hsts)
		}
	})
}

func TestFormsCSRFAndOrigin(t *testing.T) {
	x := newFixture(t, &httpx.Clients{Proxies: trustedProxies}, true)
	x.fleet.PublicURL = "https://fleet.example.org"
	post := func(values url.Values, from string, set func(r *http.Request)) *httptest.ResponseRecorder {
		r := signed(request(http.MethodPost, "/theme", from, form(values)), x.cookie)
		if set != nil {
			set(r)
		}
		return x.do(r)
	}
	good := url.Values{"csrf": {x.owner.CSRF}}

	if w := post(good, loopback, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("the right token: %d %s", w.Code, w.Body)
	}
	if w := post(url.Values{"csrf": {"wrong"}}, loopback, nil); w.Code != http.StatusForbidden {
		t.Fatalf("a wrong token: %d", w.Code)
	}
	if w := post(url.Values{}, loopback, nil); w.Code != http.StatusForbidden {
		t.Fatalf("no token: %d", w.Code)
	}
	w := post(url.Values{"csrf": {"wrong"}}, loopback, func(r *http.Request) { r.Header.Set("Accept", "application/json") })
	if w.Code != http.StatusForbidden || errorCode(t, w) != "csrf" {
		t.Fatalf("a wrong token, JSON: %d %s", w.Code, w.Body)
	}
	if w := post(url.Values{}, loopback, func(r *http.Request) { r.Header.Set("X-CSRF-Token", x.owner.CSRF) }); w.Code != http.StatusSeeOther {
		t.Fatalf("the header's token: %d", w.Code)
	}
	if w := post(url.Values{}, loopback, func(r *http.Request) { r.Header.Set("X-CSRF-Token", "wrong") }); w.Code != http.StatusForbidden {
		t.Fatalf("a wrong header token: %d", w.Code)
	}

	// the Origin: another site's is refused even with the right token
	origin := func(o string) func(r *http.Request) { return func(r *http.Request) { r.Header.Set("Origin", o) } }
	w = post(good, loopback, origin("https://evil.example"))
	if w.Code != http.StatusForbidden || errorCode(t, w) != "origin" {
		t.Fatalf("a foreign origin: %d %s", w.Code, w.Body)
	}
	if w := post(good, loopback, origin("null")); w.Code != http.StatusForbidden {
		t.Fatalf("Origin: null: %d", w.Code)
	}
	// (httptest's requests are to example.com)
	if w := post(good, loopback, origin("http://example.com")); w.Code != http.StatusSeeOther {
		t.Fatalf("the request's own host: %d %s", w.Code, w.Body)
	}
	if w := post(good, loopback, origin("https://fleet.example.org")); w.Code != http.StatusSeeOther {
		t.Fatalf("the public URL's host: %d %s", w.Code, w.Body)
	}

	// X-Forwarded-Host: a trusted proxy's is believed, another's is not
	forwarded := func(r *http.Request) {
		r.Header.Set("Origin", "https://behind.example.net")
		r.Header.Set("X-Forwarded-Host", "behind.example.net")
		r.Header.Set("X-Forwarded-Proto", "https")
	}
	if w := post(good, proxy, forwarded); w.Code != http.StatusSeeOther {
		t.Fatalf("a trusted proxy's host: %d %s", w.Code, w.Body)
	}
	if w := post(good, loopback, forwarded); w.Code != http.StatusForbidden {
		t.Fatalf("an untrusted X-Forwarded-Host was believed: %d", w.Code)
	}

	// the sign-in from another site
	w = x.do(request(http.MethodPost, "/login", loopback, form(url.Values{"name": {"paul"}, "password": {"a long password"}})))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("a sign-in: %d", w.Code)
	}
	r := request(http.MethodPost, "/login", loopback, form(url.Values{"name": {"paul"}, "password": {"a long password"}}))
	r.Header.Set("Origin", "https://evil.example")
	if w := x.do(r); w.Code != http.StatusForbidden || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("a sign-in from another site: %d %v", w.Code, w.Header())
	}
}

func TestAPITokenNeedsNoCSRF(t *testing.T) {
	x := newFixture(t, nil, true)
	token := x.apiToken(auth.Admin)
	r := bearer(request(http.MethodPost, "/api/v1/nodes/home/join-token", loopback, nil), token)
	r.Header.Set("Origin", "https://evil.example")
	w := x.do(r)
	if w.Code != http.StatusOK {
		t.Fatalf("a bearer token's POST: %d %s", w.Code, w.Body)
	}
	var answer struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
	}
	if json.Unmarshal(w.Body.Bytes(), &answer) != nil || !answer.OK || answer.Token == "" {
		t.Fatalf("%s", w.Body)
	}

	// the same with a session: its CSRF token is needed
	w = x.do(signed(request(http.MethodPost, "/api/v1/nodes/home/join-token", loopback, nil), x.cookie))
	if w.Code != http.StatusForbidden || errorCode(t, w) != "csrf" {
		t.Fatalf("a session's POST without its token: %d %s", w.Code, w.Body)
	}
	r = signed(request(http.MethodPost, "/api/v1/nodes/home/join-token", loopback, nil), x.cookie)
	r.Header.Set("X-CSRF-Token", x.owner.CSRF)
	if w := x.do(r); w.Code != http.StatusOK {
		t.Fatalf("a session's POST with its token: %d %s", w.Code, w.Body)
	}

	// a token of too low a role
	w = x.do(bearer(request(http.MethodPost, "/api/v1/nodes/home/join-token", loopback, nil), x.apiToken(auth.Viewer)))
	if w.Code != http.StatusForbidden || errorCode(t, w) != "forbidden" {
		t.Fatalf("a viewer's token: %d %s", w.Code, w.Body)
	}
	// a token that is not one
	w = x.do(bearer(request(http.MethodGet, "/api/v1/me", loopback, nil), "ocf_nothing"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a made-up token: %d %s", w.Code, w.Body)
	}
}

func TestSetupLink(t *testing.T) {
	x := newFixture(t, nil, false)
	ctx := context.Background()
	code, err := x.auth.SetupCode(ctx)
	if err != nil || code == "" {
		t.Fatal(code, err)
	}
	if w := x.do(request(http.MethodGet, "/setup?code="+url.QueryEscape(code), loopback, nil)); w.Code != http.StatusOK {
		t.Fatalf("the setup page: %d", w.Code)
	}
	if w := x.do(request(http.MethodGet, "/setup", remote, nil)); w.Code != http.StatusForbidden {
		t.Fatalf("the setup page over plain HTTP from afar: %d", w.Code)
	}
	setup := func(code, name string) *httptest.ResponseRecorder {
		return x.do(request(http.MethodPost, "/setup", loopback,
			form(url.Values{"code": {code}, "name": {name}, "password": {"a long password"}, "fleet": {"Mine"}})))
	}
	if w := setup("not-the-code", "paul"); w.Code != http.StatusBadRequest {
		t.Fatalf("a wrong code: %d", w.Code)
	}
	w := setup(code, "paul")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("the setup: %d %v", w.Code, w.Header())
	}
	var session string
	for _, c := range w.Result().Cookies() {
		if c.Name == "fleet_session" {
			session = c.Value
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				t.Errorf("the session's cookie: %+v", c)
			}
		}
	}
	if p := x.auth.Session(ctx, session); p == nil || p.Role != auth.Owner || p.Name != "paul" {
		t.Fatalf("the setup's session: %+v", p)
	}
	if x.fleet.Name(ctx) != "Mine" {
		t.Errorf("the fleet's name: %q", x.fleet.Name(ctx))
	}

	// once
	if w := setup(code, "mallory"); w.Code != http.StatusBadRequest {
		t.Fatalf("the setup link worked twice: %d", w.Code)
	}
	if _, err := x.fleet.Store.GetUserByName(ctx, "mallory"); err == nil {
		t.Fatal("a second owner")
	}
	w = x.do(request(http.MethodGet, "/setup?code="+url.QueryEscape(code), loopback, nil))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("/setup with an owner: %d %v", w.Code, w.Header())
	}
	if again, err := x.auth.SetupCode(ctx); err != nil || again != "" {
		t.Fatalf("a setup code with an owner: %q %v", again, err)
	}
}

func TestLoginLockout(t *testing.T) {
	x := newFixture(t, nil, true)
	login := func(password string) *httptest.ResponseRecorder {
		return x.do(request(http.MethodPost, "/login", loopback, form(url.Values{"name": {"paul"}, "password": {password}})))
	}
	for i := 1; i <= 5; i++ {
		if w := login("wrong password"); w.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password %d: %d", i, w.Code)
		}
	}
	// the sixth is refused before its password is checked, the right one too
	w := login("a long password")
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "locked out") {
		t.Fatalf("after five wrong passwords: %d", w.Code)
	}
	if w.Header().Get("Set-Cookie") != "" {
		t.Fatal("a locked-out sign-in made a session")
	}
	if !x.auth.Locked("127.0.0.1") {
		t.Fatal("the address is not locked")
	}
	// (another address is not)
	if x.auth.Locked("127.0.0.2") {
		t.Fatal("another address is locked")
	}
	// ten minutes later
	x.auth.Now = func() time.Time { return time.Now().Add(11 * time.Minute) }
	if w := login("a long password"); w.Code != http.StatusSeeOther {
		t.Fatalf("after the lockout: %d", w.Code)
	}
}

func TestRateLimit(t *testing.T) {
	x := newFixture(t, nil, false)
	for i := 1; i <= 60; i++ {
		if w := x.do(request(http.MethodGet, "/login", loopback, nil)); w.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	w := x.do(request(http.MethodGet, "/login", loopback, nil))
	if w.Code != http.StatusTooManyRequests || errorCode(t, w) != "rate_limited" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("the 61st request: %d %v", w.Code, w.Header())
	}
	// the sign-in is counted with the rest: refused before its password
	w = x.do(request(http.MethodPost, "/login", loopback, form(url.Values{"name": {"paul"}, "password": {"x"}})))
	if w.Code != http.StatusTooManyRequests || errorCode(t, w) != "rate_limited" {
		t.Fatalf("a sign-in past the limit: %d", w.Code)
	}
	// what is not counted
	for _, path := range []string{"/healthz", "/static/app.css"} {
		if w := x.do(request(http.MethodGet, path, loopback, nil)); w.Code != http.StatusOK {
			t.Errorf("%s past the limit: %d", path, w.Code)
		}
	}
	// another address has its own
	if w := x.do(request(http.MethodGet, "/status", "127.0.0.9:1", nil)); w.Code == http.StatusTooManyRequests {
		t.Fatal("another address was limited")
	}
}

func TestForwardedHeaders(t *testing.T) {
	t.Run("without a trusted proxy", func(t *testing.T) {
		x := newFixture(t, nil, true)
		// plain HTTP from afar: refused, whatever it says
		for _, path := range []string{"/login", "/servers", "/agent"} {
			r := request(http.MethodGet, path, remote, nil)
			r.Header.Set("X-Forwarded-Proto", "https")
			r.Header.Set("X-Forwarded-For", "127.0.0.1")
			signed(r, x.cookie)
			w := x.do(r)
			if w.Code != http.StatusForbidden || errorCode(t, w) != "tls_required" {
				t.Errorf("%s from afar: %d %s", path, w.Code, w.Body)
			}
		}
		r := request(http.MethodPost, "/agent/join", remote, strings.NewReader(`{"token":"x"}`))
		if w := x.do(r); w.Code != http.StatusForbidden || errorCode(t, w) != "tls_required" {
			t.Errorf("the enrolment from afar: %d %s", w.Code, w.Body)
		}
		// loopback directly: through (the agent's endpoint wants its token)
		if w := x.do(request(http.MethodGet, "/agent", loopback, nil)); w.Code != http.StatusUnauthorized {
			t.Errorf("/agent from loopback: %d %s", w.Code, w.Body)
		}
		if w := x.do(signed(request(http.MethodGet, "/servers", loopback, nil), x.cookie)); w.Code != http.StatusOK {
			t.Errorf("/servers from loopback: %d", w.Code)
		}
		// X-Forwarded-For from loopback, not a trusted proxy, is not
		// believed: the requests count as loopback's
		for i := 1; i <= 60; i++ {
			r := request(http.MethodGet, "/login", loopback, nil)
			r.Header.Set("X-Forwarded-For", netip.AddrFrom4([4]byte{198, 51, 100, byte(i)}).String())
			if w := x.do(r); w.Code != http.StatusOK {
				t.Fatalf("request %d: %d", i, w.Code)
			}
		}
		r = request(http.MethodGet, "/login", loopback, nil)
		r.Header.Set("X-Forwarded-For", "198.51.100.200")
		if w := x.do(r); w.Code != http.StatusTooManyRequests {
			t.Fatalf("an untrusted X-Forwarded-For was believed: %d", w.Code)
		}
	})

	t.Run("with a trusted proxy", func(t *testing.T) {
		x := newFixture(t, &httpx.Clients{Proxies: trustedProxies}, true)
		through := func(method, path, proto, forwardedFor string) *httptest.ResponseRecorder {
			r := request(method, path, proxy, nil)
			if proto != "" {
				r.Header.Set("X-Forwarded-Proto", proto)
			}
			if forwardedFor != "" {
				r.Header.Set("X-Forwarded-For", forwardedFor)
			}
			return x.do(signed(r, x.cookie))
		}
		// the proxy's word for TLS
		if w := through(http.MethodGet, "/servers", "https", "198.51.100.1"); w.Code != http.StatusOK {
			t.Errorf("/servers over the proxy's TLS: %d", w.Code)
		}
		if w := through(http.MethodGet, "/agent", "https", "198.51.100.1"); w.Code != http.StatusUnauthorized {
			t.Errorf("/agent over the proxy's TLS: %d %s", w.Code, w.Body)
		}
		for _, proto := range []string{"", "http", "https, http"} {
			for _, path := range []string{"/servers", "/agent", "/login"} {
				if w := through(http.MethodGet, path, proto, "198.51.100.1"); w.Code != http.StatusForbidden || errorCode(t, w) != "tls_required" {
					t.Errorf("%s over the proxy's plain HTTP (%q): %d", path, proto, w.Code)
				}
			}
		}
		// (a proxy is not loopback: a trusted loopback proxy's plain HTTP is refused)
		y := newFixture(t, &httpx.Clients{Proxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}, false)
		if w := y.do(request(http.MethodGet, "/login", loopback, nil)); w.Code != http.StatusForbidden {
			t.Errorf("a loopback proxy's plain HTTP: %d", w.Code)
		}
		// X-Forwarded-For is believed: each client has its own count (signed
		// out: the anonymous limit)
		anonymous := func(forwardedFor string) int {
			r := request(http.MethodGet, "/login", proxy, nil)
			r.Header.Set("X-Forwarded-Proto", "https")
			r.Header.Set("X-Forwarded-For", forwardedFor)
			return x.do(r).Code
		}
		for i := 1; i <= 60; i++ {
			if code := anonymous("198.51.100.1"); code != http.StatusOK {
				t.Fatalf("request %d: %d", i, code)
			}
		}
		if code := anonymous("198.51.100.1"); code != http.StatusTooManyRequests {
			t.Fatalf("the 61st of one client: %d", code)
		}
		// (the rightmost address that is not a trusted proxy's)
		if code := anonymous("198.51.100.1, 198.51.100.2, 10.9.9.9"); code != http.StatusOK {
			t.Fatalf("another client: %d", code)
		}
		// (the leftmost entries are the client's own word, not believed)
		if code := anonymous("198.51.100.2, 198.51.100.1"); code != http.StatusTooManyRequests {
			t.Fatalf("a spoofed first entry escaped the limit: %d", code)
		}
	})
}

func TestConsoleRoleGate(t *testing.T) {
	x := newFixture(t, nil, true)
	console := func(token string) *httptest.ResponseRecorder {
		r := request(http.MethodGet, "/ws/console/nowhere", loopback, nil)
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Sec-WebSocket-Version", "13")
		r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		if token != "" {
			signed(r, token)
		}
		return x.do(r)
	}
	if w := console(""); w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
		t.Fatalf("no session: %d %v", w.Code, w.Header())
	}
	if w := console(x.user(auth.Viewer)); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "moderator") {
		t.Fatalf("a viewer: %d", w.Code)
	}
	// a moderator is through the gate: the console answers (no such server)
	if w := console(x.user(auth.Moderator)); w.Code != http.StatusNotFound {
		t.Fatalf("a moderator: %d %s", w.Code, w.Body)
	}
	if w := console(x.cookie); w.Code != http.StatusNotFound {
		t.Fatalf("the owner: %d %s", w.Code, w.Body)
	}
	// plain HTTP from afar, even a moderator's
	r := request(http.MethodGet, "/ws/console/nowhere", remote, nil)
	if w := x.do(signed(r, x.cookie)); w.Code != http.StatusForbidden || errorCode(t, w) != "tls_required" {
		t.Fatalf("from afar: %d", w.Code)
	}
}

// instance makes a node and a server on it: the server's slug.
func (x *fixture) instance() string {
	x.t.Helper()
	ctx := context.Background()
	joinToken, err := x.fleet.JoinToken(ctx, "test", "home", spec.DefaultNode())
	if err != nil {
		x.t.Fatal(err)
	}
	if _, _, err := x.fleet.Join(ctx, core.JoinRequest{Token: joinToken, Name: "home", Hostname: "home", Runtime: "docker",
		OS: "linux", Arch: "amd64"}); err != nil {
		x.t.Fatal(err)
	}
	profile, err := x.fleet.CreateProfile(ctx, "test", core.ProfileInput{Name: "BTB"})
	if err != nil {
		x.t.Fatal(err)
	}
	slugs, _, err := x.fleet.CreateInstances(ctx, "test", core.CreateRequest{ProfileID: profile, Nodes: []string{"home"},
		Count: 1, Pattern: "Alpha"})
	if err != nil || len(slugs) != 1 {
		x.t.Fatal(slugs, err)
	}
	return slugs[0]
}

func TestEventsStream(t *testing.T) {
	x := newFixture(t, nil, true)
	slug := x.instance()
	viewer := x.user(auth.Viewer)
	server := httptest.NewServer(x.handler)
	t.Cleanup(server.Close)

	// signed out: refused
	response, err := http.Get(server.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("signed out: %d", response.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/events", nil)
	r.AddCookie(&http.Cookie{Name: "fleet_session", Value: viewer})
	response, err = http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %v", response.StatusCode, response.Header)
	}
	lines := bufio.NewScanner(response.Body)
	lines.Buffer(make([]byte, 1<<20), 1<<20)
	next := func() string {
		if !lines.Scan() {
			t.Fatalf("the stream ended: %v", lines.Err())
		}
		return lines.Text()
	}
	// (subscribed once the retry is sent)
	if line := next(); line != "retry: 3000" {
		t.Fatalf("the first line: %q", line)
	}
	x.fleet.Live.SetInstance(0, agentproto.Instance{Slug: slug, State: agentproto.Crashed, Reason: "a test's crash"})
	want := "event: instance:" + slug
	for {
		line := next()
		if line != want {
			continue
		}
		var data strings.Builder
		for {
			line := next()
			if line == "" {
				break
			}
			text, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				t.Fatalf("not a data line: %q", line)
			}
			data.WriteString(text + "\n")
		}
		html := data.String()
		if !strings.Contains(html, `id="row-`+slug+`"`) || !strings.Contains(html, `sse-swap="instance:`+slug+`"`) {
			t.Fatalf("the fragment is not the server's row: %s", html)
		}
		if !strings.Contains(html, "crashed") || !strings.Contains(html, "a test&#39;s crash") {
			t.Fatalf("the row does not show the change: %s", html)
		}
		return
	}
}
