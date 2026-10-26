// Package fleet_test runs a whole fleet in one test: a control plane (the
// core and its pages on loopback), a node's agent with the process runtime,
// and tools/fakehalo as the release's server (OPENCE_FLEET_PLAN.md 9.2).
// Nothing leaves loopback.
package fleet_test

import (
	"archive/zip"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	opencefleet "github.com/OpenCommunityEdition/opence-fleet"
	"github.com/OpenCommunityEdition/opence-fleet/internal/agent"
	"github.com/OpenCommunityEdition/opence-fleet/internal/agent/runtime"
	"github.com/OpenCommunityEdition/opence-fleet/internal/agent/runtime/process"
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

type harness struct {
	t      *testing.T
	fleet  *core.Fleet
	auth   *auth.Auth
	server *httptest.Server
	owner  *auth.Principal
	cookie string
	work   string
	// stops the docker node's agent (TestDockerRuntime)
	stopAgent   func()
	dockerAgent *agent.Agent
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func logger(t *testing.T) *slog.Logger {
	if os.Getenv("FLEET_TEST_VERBOSE") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return quiet()
}

// release builds tools/fakehalo and zips it as a release: halo is a script
// that runs the fake quickly (FAKEHALO_TICK), with a 32-bit C runtime's
// worth of nothing.
func release(t *testing.T, work string) string {
	t.Helper()
	bin := filepath.Join(work, "fakehalo")
	build := exec.Command("go", "build", "-o", bin, "../../tools/fakehalo")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(work, "release.zip")
	f, _ := os.Create(path)
	z := zip.NewWriter(f)
	add := func(name string, mode os.FileMode, data []byte) {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(mode)
		w, _ := z.CreateHeader(h)
		w.Write(data)
	}
	fake, _ := os.ReadFile(bin)
	add("fakehalo", 0o755, fake)
	add("halo", 0o755, []byte("#!/bin/sh\nexport FAKEHALO_TICK=${FAKEHALO_TICK:-0.05}\nexec \"$(dirname \"$0\")/fakehalo\" \"$@\"\n"))
	add("libSDL3.so.0", 0o644, []byte("not a library"))
	z.Close()
	f.Close()
	maps := exec.Command(bin, "--make-maps", filepath.Join(work, "gamedata"))
	if out, err := maps.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	return path
}

func start(t *testing.T) *harness {
	t.Helper()
	work := t.TempDir()
	zipPath := release(t, work)
	c := &config.ServeConfig{Name: "Test fleet", Data: filepath.Join(work, "data"), DB: filepath.Join(work, "fleet.db"),
		Image: "opence-server", ReleaseRepository: "x/y", ReleaseCheckInterval: time.Hour, EventsDays: 30, SamplesDays: 7,
		LogAddresses: true, Metrics: true, Updates: "off", Timezone: time.UTC, SessionHours: 12}
	s, err := store.Open(c.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	l := live.New()
	f := core.New(s, crypt.New(), c, logger(t), l)
	a := auth.New(s, 12)
	server := web.New(f, a, &httpx.Clients{}, quiet(), "dev-test")
	server.AgentScript = opencefleet.AgentScript
	web.Static = static.Files
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	f.PublicURL = ts.URL
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.Run(ctx)
	code, _ := a.SetupCode(ctx)
	token, owner, err := a.Setup(ctx, code, "paul", "a long password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.AddZip(ctx, "test", "build-900", zipPath); err != nil {
		t.Fatal(err)
	}
	if err := f.SetLatest(ctx, "test", "build-900"); err != nil {
		t.Fatal(err)
	}
	if added, skipped, err := f.AddFolder(ctx, "test", filepath.Join(work, "gamedata")); err != nil || len(skipped) > 0 {
		t.Fatal(added, skipped, err)
	}
	return &harness{t: t, fleet: f, auth: a, server: ts, owner: owner, cookie: token, work: work}
}

// node enrols and runs an agent with the process runtime.
func (h *harness) node(name string, base int) (*agent.Agent, context.CancelFunc) {
	h.t.Helper()
	settings := spec.DefaultNode()
	settings.Runtime = "process"
	settings.TunnelPorts = spec.PortRange{First: base, Last: base + 9}
	settings.WebPorts = spec.PortRange{First: base + 100, Last: base + 109}
	settings.LoopbackBase = 20
	ctx := context.Background()
	token, err := h.fleet.JoinToken(ctx, "test", name, settings)
	if err != nil {
		h.t.Fatal(err)
	}
	answer, err := agent.Join(ctx, http.DefaultClient, h.server.URL, token, name, "process")
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := agent.Join(ctx, http.DefaultClient, h.server.URL, token, name, "process"); err == nil {
		h.t.Fatal("a join token worked twice")
	}
	c := &config.AgentConfig{URL: h.server.URL, Token: answer.Token, Dir: filepath.Join(h.work, "node-"+name), Name: name,
		Runtime: "process"}
	var a *agent.Agent
	rt := process.New(filepath.Join(c.Dir, "releases"), func(ctx context.Context, url, path, sha string, p runtime.Progress) error {
		return a.Download(ctx, url, path, sha, p)
	}, quiet())
	a = agent.New(c, "dev-test", logger(h.t), rt, http.DefaultClient)
	a.ReleaseKey = ed25519.PublicKey(make([]byte, 32))
	run, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Run(run)
		close(done)
	}()
	h.t.Cleanup(func() {
		cancel()
		<-done
	})
	return a, func() {
		cancel()
		<-done
	}
}

func (h *harness) until(what string, timeout time.Duration, ok func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			h.t.Fatalf("waited %s for %s", timeout, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (h *harness) up(slug string) bool {
	l := h.fleet.Live.Instance(slug)
	return l != nil && l.State == "up" && l.Status != nil
}

// page gets a page as a user (a session of the role's).
func (h *harness) page(token, path string) (int, string) {
	request, _ := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
	request.AddCookie(&http.Cookie{Name: "fleet_session", Value: token})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		h.t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

func TestFleet(t *testing.T) {
	if testing.Short() {
		t.Skip("the whole fleet: not in -short")
	}
	h := start(t)
	ctx := context.Background()
	h.node("home", 42300)
	h.until("the node", 10*time.Second, func() bool {
		n, err := h.fleet.NodeByName(ctx, "home")
		return err == nil && h.fleet.NodeOnline(n.ID)
	})
	profile, err := h.fleet.CreateProfile(ctx, "user:paul", core.ProfileInput{Name: "BTB", Rotation: "bloodgulch:ctf, sidewinder:slayer",
		Region: "eu", Settings: map[string]any{"server.max_players": "16", "server.lobby_seconds": "2"}})
	if err != nil {
		t.Fatal(err)
	}
	slugs, _, err := h.fleet.CreateInstances(ctx, "user:paul", core.CreateRequest{ProfileID: profile, Nodes: []string{"home"},
		Count: 2, Pattern: "BTB #{n}", Groups: []string{"eu"}, Running: true})
	if err != nil || len(slugs) != 2 || slugs[0] != "btb-1" {
		t.Fatal(slugs, err)
	}
	for _, slug := range slugs {
		h.until(slug+" up", 30*time.Second, func() bool { return h.up(slug) })
	}
	first, _ := h.fleet.Instance(ctx, "btb-1")
	second, _ := h.fleet.Instance(ctx, "btb-2")
	if first.TunnelPort == second.TunnelPort || first.WebPort == second.WebPort || first.LoopbackIndex == second.LoopbackIndex {
		t.Fatal("two servers share a port or a loopback address")
	}
	if l := h.fleet.Live.Instance("btb-1"); l.Status.MaxPlayers != 16 || !l.Has("owner_web") {
		t.Fatalf("%+v %v", l.Status, l.Capabilities)
	}

	t.Run("a profile's change reaches its servers live", func(t *testing.T) {
		in := core.ProfileInput{Name: "BTB", Rotation: "bloodgulch:ctf, sidewinder:slayer", Region: "eu",
			Settings: map[string]any{"server.max_players": "12", "server.lobby_seconds": "2", "server.motd": "hello"}}
		if err := h.fleet.UpdateProfile(ctx, "user:paul", profile, in); err != nil {
			t.Fatal(err)
		}
		for _, slug := range slugs {
			h.until(slug+"'s max_players", 15*time.Second, func() bool {
				l := h.fleet.Live.Instance(slug)
				return l.Status != nil && l.Status.MaxPlayers == 12
			})
			if l := h.fleet.Live.Instance(slug); l.RestartNeeded {
				t.Fatal("a live change needs a restart")
			}
		}
	})

	t.Run("a rotation a server refuses is set on none", func(t *testing.T) {
		in := core.ProfileInput{Name: "BTB", Rotation: "bloodgulch:ctf, nowhere:slayer", Region: "eu",
			Settings: map[string]any{"server.max_players": "12", "server.lobby_seconds": "2"}}
		if err := h.fleet.UpdateProfile(ctx, "user:paul", profile, in); err != nil {
			t.Fatal(err)
		}
		h.until("the refusal", 15*time.Second, func() bool {
			l := h.fleet.Live.Instance("btb-1")
			if l.Applied == nil {
				return false
			}
			for _, e := range l.Applied.Errors {
				if e.Name == "server.rotation" && strings.Contains(e.Message, "nowhere") {
					return true
				}
			}
			return false
		})
		in.Rotation = "bloodgulch:ctf, sidewinder:slayer"
		h.fleet.UpdateProfile(ctx, "user:paul", profile, in)
	})

	t.Run("moderation and a fleet ban", func(t *testing.T) {
		var players []struct {
			Slot    int
			Name    string
			Address string
			HWID    string
		}
		h.until("players", 20*time.Second, func() bool {
			list, err := h.fleet.Players(ctx, "btb-1")
			if err != nil || len(list) == 0 {
				return false
			}
			players = nil
			for _, p := range list {
				players = append(players, struct {
					Slot    int
					Name    string
					Address string
					HWID    string
				}{p.Slot, p.Name, p.Address, p.HWID})
			}
			return true
		})
		target := players[0]
		if target.Address == "" {
			t.Fatal("no address: server.log_addresses is the fleet's")
		}
		if err := h.fleet.BanPlayer(ctx, "user:paul", "btb-1", target.Slot, "someone else", 0, "x", true); err == nil {
			t.Fatal("a ban of another name passed")
		}
		if err := h.fleet.BanPlayer(ctx, "user:paul", "btb-1", target.Slot, target.Name, time.Hour, "team killing", true); err != nil {
			t.Fatal(err)
		}
		h.until("the fleet ban on btb-2", 15*time.Second, func() bool {
			var answer struct {
				Bans []struct{ IP, HWID string } `json:"bans"`
			}
			if h.fleet.Call(ctx, "btb-2", http.MethodGet, "/api/bans", nil, &answer) != nil {
				return false
			}
			for _, b := range answer.Bans {
				if b.IP == target.Address {
					return true
				}
			}
			return false
		})
		bans, _ := h.fleet.Bans(ctx, false)
		if len(bans) != 1 || bans[0].SourceSlug != "btb-1" {
			t.Fatalf("%+v", bans)
		}
		if err := h.fleet.Unban(ctx, "user:paul", bans[0].ID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a drained restart is a job", func(t *testing.T) {
		before := h.fleet.Live.Instance("btb-2").Since
		id, err := h.fleet.StartJob(ctx, "user:paul", core.JobParams{Action: core.ActionDrainRestart, Slugs: []string{"btb-2"},
			After: "now"})
		if err != nil {
			t.Fatal(err)
		}
		h.until("the job", 60*time.Second, func() bool {
			job, _, _ := h.fleet.Job(ctx, id)
			return job.State == "done" || job.State == "failed"
		})
		job, items, _ := h.fleet.Job(ctx, id)
		if job.State != "done" || items[0].State != "done" {
			t.Fatalf("%+v %+v", job, items)
		}
		if !h.fleet.Live.Instance("btb-2").Since.After(before) {
			t.Fatal("not restarted")
		}
	})

	t.Run("a bad configuration blocks a server", func(t *testing.T) {
		bad, err := h.fleet.CreateProfile(ctx, "user:paul", core.ProfileInput{Name: "Bad", Rotation: "nowhere:ctf"})
		if err != nil {
			t.Fatal(err)
		}
		created, _, err := h.fleet.CreateInstances(ctx, "user:paul", core.CreateRequest{ProfileID: bad, Nodes: []string{"home"},
			Count: 1, Running: true})
		if err != nil {
			t.Fatal(err)
		}
		h.until("blocked", 20*time.Second, func() bool {
			l := h.fleet.Live.Instance(created[0])
			return l != nil && l.State == "blocked"
		})
		if l := h.fleet.Live.Instance(created[0]); *l.Exit != 3 || !strings.Contains(l.Reason, "configuration") {
			t.Fatalf("%+v", l)
		}
		open, _ := h.fleet.Store.ListOpenAlerts(ctx)
		found := false
		for _, a := range open {
			found = found || (a.Rule == "instance_blocked" && a.Target == created[0])
		}
		if !found {
			t.Fatal("no alert")
		}
		if err := h.fleet.DeleteInstance(ctx, "user:paul", created[0]); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("the agent reconnects, and the servers go on", func(t *testing.T) {
		n, _ := h.fleet.NodeByName(ctx, "home")
		pid := h.fleet.Live.Instance("btb-1").Status.PID
		h.fleet.Hub.Disconnect(n.ID, "a test")
		h.until("offline", 5*time.Second, func() bool { return !h.fleet.NodeOnline(n.ID) })
		h.until("online again", 20*time.Second, func() bool { return h.fleet.NodeOnline(n.ID) && h.up("btb-1") })
		if h.fleet.Live.Instance("btb-1").Status.PID != pid {
			t.Fatal("the server was restarted by a reconnect")
		}
	})

	t.Run("every page, for every role", func(t *testing.T) {
		viewerToken := h.user(t, auth.Viewer)
		for _, path := range []string{"/", "/servers", "/servers/new", "/servers/btb-1", "/servers/btb-1/players",
			"/servers/btb-1/rotation", "/servers/btb-1/gametypes", "/servers/btb-1/bans", "/servers/btb-1/console",
			"/servers/btb-1/settings", "/servers/btb-1/files", "/servers/btb-1/history", "/nodes", "/nodes/home",
			"/nodes/add", "/profiles", "/profiles/1", "/players", "/bans", "/library", "/releases", "/masters", "/jobs",
			"/jobs/1", "/schedules", "/alerts", "/audit", "/team", "/settings", "/account", "/more", "/metrics"} {
			status, body := h.page(h.cookie, path)
			if status != http.StatusOK || strings.Contains(body, "the page failed") {
				t.Errorf("owner %s: %d", path, status)
			}
			status, body = h.page(viewerToken, path)
			if status >= 500 {
				t.Errorf("viewer %s: %d", path, status)
			}
			if strings.Contains(body, "203.0.113.") {
				t.Errorf("viewer %s sees an address", path)
			}
		}
		if status, _ := h.page("", "/servers"); status != http.StatusSeeOther {
			t.Fatalf("no session: %d", status)
		}
	})

	t.Run("a form without its CSRF token is refused", func(t *testing.T) {
		jar, _ := cookiejar.New(nil)
		u, _ := url.Parse(h.server.URL)
		jar.SetCookies(u, []*http.Cookie{{Name: "fleet_session", Value: h.cookie}})
		client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		response, err := client.PostForm(h.server.URL+"/servers/btb-1/stop", url.Values{"csrf": {"wrong"}})
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("%d", response.StatusCode)
		}
		response, _ = client.PostForm(h.server.URL+"/servers/btb-1/say", url.Values{"csrf": {h.owner.CSRF}, "text": {"hello"}})
		response.Body.Close()
		if response.StatusCode != http.StatusSeeOther {
			t.Fatalf("%d", response.StatusCode)
		}
	})

	t.Run("the API", func(t *testing.T) {
		token, err := h.auth.MakeToken(ctx, h.owner, "ci", auth.Admin)
		if err != nil {
			t.Fatal(err)
		}
		get := func(path string, out any) int {
			request, _ := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
			request.Header.Set("Authorization", "Bearer "+token)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			json.NewDecoder(response.Body).Decode(out)
			return response.StatusCode
		}
		var instances struct {
			Instances []struct {
				Slug  string `json:"slug"`
				State string `json:"state"`
			} `json:"instances"`
		}
		if status := get("/api/v1/instances?group=eu", &instances); status != 200 || len(instances.Instances) != 2 {
			t.Fatalf("%d %+v", status, instances)
		}
		var status struct {
			OK      bool `json:"ok"`
			Players int  `json:"players"`
		}
		if code := get("/api/v1/instances/btb-1/server/status", &status); code != 200 || !status.OK {
			t.Fatalf("%d %+v", code, status)
		}
		var export map[string]any
		if code := get("/api/v1/export", &export); code != http.StatusForbidden {
			t.Fatalf("an admin token exported: %d", code)
		}
	})

	t.Run("no secret in the audit log or the pages", func(t *testing.T) {
		rows, _ := h.fleet.Store.ListAudit(ctx, dbAuditAll())
		var all strings.Builder
		for _, r := range rows {
			fmt.Fprintln(&all, r.Actor, r.Action, r.Target, r.Detail)
		}
		instances, _ := h.fleet.Store.ListInstances(ctx)
		for _, i := range instances {
			password, _ := h.fleet.Key.OpenString(i.WebPasswordEnc, "instance:"+i.Slug+":web_password")
			if password == "" || strings.Contains(all.String(), password) {
				t.Fatal("a web password in the audit log")
			}
			for _, path := range []string{"/servers/" + i.Slug, "/servers/" + i.Slug + "/settings", "/servers/" + i.Slug + "/files"} {
				if _, body := h.page(h.cookie, path); strings.Contains(body, password) {
					t.Fatalf("a web password on %s", path)
				}
			}
		}
	})
}

// user makes a user of a role, and their session's token.
func (h *harness) user(t *testing.T, role string) string {
	ctx := context.Background()
	code, err := h.auth.MakeInvite(ctx, h.owner, role, "")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := h.auth.AcceptInvite(ctx, code, role+"-user", "a long password", "127.0.0.2")
	if err != nil {
		t.Fatal(err)
	}
	return token
}
