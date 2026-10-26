package serverapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	adminPassword = "admin-password-1"
	ownerPassword = "owner-password-1"
)

// fakehalo builds tools/fakehalo and runs it as `halo --server` on free
// ports, with synthetic maps, the admin's and the owner's passwords and a
// pinned region: its web port's base URL.
func fakehalo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go on PATH to build tools/fakehalo")
	}
	work := t.TempDir()
	bin := filepath.Join(work, "fakehalo")
	build := exec.Command("go", "build", "-o", bin, "../../tools/fakehalo")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building fakehalo: %v\n%s", err, out)
	}
	data := filepath.Join(work, "gamedata")
	if out, err := exec.Command(bin, "--make-maps", data).CombinedOutput(); err != nil {
		t.Fatalf("making the maps: %v\n%s", err, out)
	}

	web, udp := freePorts(t)
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "HALO_") && !strings.HasPrefix(kv, "FAKEHALO_") {
			env = append(env, kv)
		}
	}
	env = append(env, "HALO_DATA_ROOT="+data, "HALO_CONFIG_FILE="+filepath.Join(work, "config", "config.toml"),
		"HALO_SERVER_WEB_PORT="+fmt.Sprint(web), "HALO_SERVER_WEB_BIND=127.0.0.1", "HALO_NET_TUNNEL_PORT="+fmt.Sprint(udp),
		"HALO_SERVER_WEB_PASSWORD="+adminPassword, "HALO_SERVER_WEB_OWNER_PASSWORD="+ownerPassword,
		"HALO_SERVER_REGION=eu", "FAKEHALO_TICK=0.05")
	if err := os.MkdirAll(filepath.Join(work, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(work, "stdout.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--server")
	cmd.Env, cmd.Stdout, cmd.Stderr = env, logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		logFile.Close()
		close(exited)
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-exited
		if t.Failed() {
			out, _ := os.ReadFile(logPath)
			t.Logf("fakehalo's output:\n%s", out)
		}
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", web)
	c := &Client{Base: base}
	ctx := context.Background()
	for deadline := time.Now().Add(15 * time.Second); ; {
		state, ok, err := c.Healthz(ctx)
		if err == nil && ok {
			if state != "lobby" && state != "loading" && state != "playing" && state != "postgame" {
				t.Errorf("/healthz's state: %q", state)
			}
			return base
		}
		select {
		case <-exited:
			out, _ := os.ReadFile(logPath)
			t.Fatalf("fakehalo exited:\n%s", out)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("/healthz: %q, %v, %v", state, ok, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// freePorts is a free TCP port and a free UDP port on loopback.
func freePorts(t *testing.T) (int, int) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	u, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	return l.Addr().(*net.TCPAddr).Port, u.LocalAddr().(*net.UDPAddr).Port
}

// refused checks an error is the server's refusal of a status and code.
func refused(t *testing.T, err error, status int, code string) *Error {
	t.Helper()
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("got %v, want a refusal %d %s", err, status, code)
	}
	if e.Status != status || e.Code != code {
		t.Fatalf("got %d %s (%s), want %d %s", e.Status, e.Code, e.Message, status, code)
	}
	return e
}

func TestFakehalo(t *testing.T) {
	base := fakehalo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin := &Client{Base: base, Password: adminPassword, By: "serverapi test"}
	owner := &Client{Base: base, Password: ownerPassword, By: "serverapi test (owner)"}

	t.Run("login", func(t *testing.T) {
		if admin.Role() != "" {
			t.Errorf("a role before the sign-in: %q", admin.Role())
		}
		token, err := admin.Token(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if token == "" || admin.Role() != "admin" {
			t.Errorf("token %d bytes, role %q", len(token), admin.Role())
		}
		// (kept while it has more than an hour)
		if again, _ := admin.Token(ctx); again != token {
			t.Error("a second token while the first is good")
		}
		if err := owner.Login(ctx); err != nil || owner.Role() != "owner" {
			t.Errorf("the owner: %q, %v", owner.Role(), err)
		}
		wrong := &Client{Base: base, Password: "a-wrong-password"}
		refused(t, wrong.Login(ctx), 401, "denied")
		_, err = wrong.Status(ctx)
		refused(t, err, 401, "denied")
	})

	t.Run("sessions renewed", func(t *testing.T) {
		// a session the server forgot: refused once, signed in again
		c := &Client{Base: base, Password: adminPassword}
		c.token, c.role, c.expires = "forgotten", "admin", time.Now().Add(12*time.Hour)
		if _, err := c.Status(ctx); err != nil {
			t.Fatal(err)
		}
		if c.token == "forgotten" || c.token == "" {
			t.Error("the forgotten session was kept")
		}
		// a session with less than an hour left: renewed before it ends
		old := c.token
		c.expires = time.Now().Add(30 * time.Minute)
		token, err := c.Token(ctx)
		if err != nil || token == old || time.Until(c.expires) < 11*time.Hour {
			t.Errorf("renewal: %v, same token %v, expires in %s", err, token == old, time.Until(c.expires))
		}
		c.Forget()
		if c.Role() != "" {
			t.Error("Forget kept the role")
		}
	})

	t.Run("status", func(t *testing.T) {
		s, err := admin.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !s.OK || s.Build != "build-900" || s.NetworkVersion != 39 || s.Name == "" {
			t.Errorf("status: ok %v, build %q, network %d, name %q", s.OK, s.Build, s.NetworkVersion, s.Name)
		}
		if s.Region != "eu" || s.WebPort == 0 || s.TunnelPort == 0 || s.PID == 0 || s.Drain != nil {
			t.Errorf("identity: region %q, web %d, tunnel %d, pid %d, drain %v", s.Region, s.WebPort, s.TunnelPort, s.PID, s.Drain)
		}
		if !s.Has("identity") || !s.Has("events") || s.Has("teleport") {
			t.Errorf("capabilities %v", s.Capabilities)
		}
		if s.Rotation.Count != 3 || s.Rotation.Next == nil || s.Rotation.Next.Map == "" {
			t.Errorf("rotation %+v", s.Rotation)
		}
		if !strings.HasPrefix(s.Listing.Invite, "halo://") {
			t.Errorf("the admin's invite: %q", s.Listing.Invite)
		}
	})

	t.Run("players", func(t *testing.T) {
		// (the script brings players within a few ticks)
		var players []Player
		for deadline := time.Now().Add(5 * time.Second); len(players) == 0; time.Sleep(50 * time.Millisecond) {
			var err error
			if players, err = admin.Players(ctx); err != nil {
				t.Fatal(err)
			}
			if time.Now().After(deadline) {
				t.Fatal("no player joined")
			}
		}
		p := players[0]
		if p.Name == "" || p.Ping == nil || p.Machine == 0 || p.Joined == 0 {
			t.Errorf("player %+v", p)
		}
		if p.Address != "" || p.HWID != "" {
			t.Errorf("the admin sees an address: %+v", p)
		}
		// (the owner does)
		list, err := owner.Players(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) > 0 && list[0].Address == "" {
			t.Errorf("the owner sees no address: %+v", list[0])
		}
	})

	t.Run("settings", func(t *testing.T) {
		settings, err := admin.Settings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		find := func(name string) SettingInfo {
			for _, s := range settings {
				if s.Name == name {
					return s
				}
			}
			t.Fatalf("no setting %s", name)
			return SettingInfo{}
		}
		region := find("server.region")
		if !region.Pinned || region.Env == nil || *region.Env != "HALO_SERVER_REGION" || region.Value != "eu" {
			t.Errorf("server.region %+v", region)
		}
		password := find("server.web_password")
		if !password.Secret || !password.Set || password.Value != "" || !password.Pinned {
			t.Errorf("server.web_password %+v", password)
		}
		if format := find("server.log_format"); !slices.Contains(format.Choices, "json") {
			t.Errorf("server.log_format's choices %v", format.Choices)
		}

		// each changed or refused on its own
		result, err := admin.PutSettings(ctx, map[string]any{"server.metrics": false, "server.region": "us",
			"server.auto_update": "check", "server.nosuch": 1})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(result.Applied, []string{"server.metrics"}) {
			t.Errorf("applied %v", result.Applied)
		}
		codes := map[string]string{}
		for _, e := range result.Errors {
			codes[e.Name] = e.Code
		}
		want := map[string]string{"server.region": "pinned", "server.auto_update": "forbidden", "server.nosuch": "schema"}
		for name, code := range want {
			if codes[name] != code {
				t.Errorf("%s: %q, want %q (%v)", name, codes[name], code, result.Errors)
			}
		}
		// (the owner's alone)
		if result, err := owner.PutSettings(ctx, map[string]any{"server.auto_update": "check"}); err != nil || len(result.Applied) != 1 {
			t.Errorf("the owner's change: %+v, %v", result, err)
		}

		// a pinned setting through a command: 409
		lines, err := admin.Command(ctx, "sv_set server.region us")
		e := refused(t, err, 409, "pinned")
		if len(lines) == 0 || !slices.Equal(e.Lines, lines) || !strings.Contains(lines[0], "HALO_SERVER_REGION") {
			t.Errorf("the pinned refusal's lines %q (%q)", lines, e.Lines)
		}
	})

	t.Run("rotation", func(t *testing.T) {
		rotation, err := admin.Rotation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(rotation.Entries) != 3 || rotation.Entries[0].Text != "bloodgulch:ctf" || rotation.Entries[0].Gametype != "ctf" {
			t.Errorf("rotation %+v", rotation.Entries)
		}
		err = admin.PutRotation(ctx, []string{"bloodgulch:slayer", "nowhere:slayer", "prisoner:nosuch"})
		e := refused(t, err, 400, "rejected")
		if len(e.Rejected) != 2 {
			t.Fatalf("rejected %+v", e.Rejected)
		}
		if r := e.Rejected[0]; r.Index != 1 || r.Entry != "nowhere:slayer" || !strings.Contains(r.Reason, "nowhere") {
			t.Errorf("rejected[0] %+v", r)
		}
		if r := e.Rejected[1]; r.Index != 2 || !strings.Contains(r.Reason, "nosuch") {
			t.Errorf("rejected[1] %+v", r)
		}
		// (nothing changed)
		if rotation, _ := admin.Rotation(ctx); len(rotation.Entries) != 3 {
			t.Errorf("the rejected rotation changed it: %+v", rotation.Entries)
		}
		if err := admin.PutRotation(ctx, []string{"chillout:slayer", "hangemhigh:team_slayer"}); err != nil {
			t.Fatal(err)
		}
		rotation, err = admin.Rotation(ctx)
		if err != nil || len(rotation.Entries) != 2 || rotation.Entries[1].Map != "hangemhigh" || rotation.Next == nil {
			t.Errorf("the new rotation %+v, %v", rotation, err)
		}
	})

	t.Run("gametypes", func(t *testing.T) {
		if err := admin.PutGametype(ctx, "mine", "base = \"slayer\"\nscore = 25\n"); err != nil {
			t.Fatal(err)
		}
		e := refused(t, admin.PutGametype(ctx, "bad", "score = 25\n"), 400, "invalid")
		if e.Line != 1 {
			t.Errorf("the invalid file's line: %d", e.Line)
		}
		gametypes, err := admin.Gametypes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(gametypes.Builtin, "slayer") || len(gametypes.Schema) == 0 {
			t.Errorf("builtin %v, schema %s", gametypes.Builtin, gametypes.Schema)
		}
		if len(gametypes.Files) != 1 || gametypes.Files[0].Name != "mine" || gametypes.Files[0].Error != nil {
			t.Errorf("files %+v", gametypes.Files)
		}
		if text, err := admin.Gametype(ctx, "mine"); err != nil || !strings.Contains(text, "score = 25") {
			t.Errorf("mine: %q, %v", text, err)
		}
		// (a rotation may name it now)
		if err := admin.PutRotation(ctx, []string{"bloodgulch:file=mine", "chillout:slayer"}); err != nil {
			t.Errorf("a rotation with the file: %v", err)
		}
		if err := admin.PutRotation(ctx, []string{"chillout:slayer"}); err != nil {
			t.Fatal(err)
		}
		if err := admin.DeleteGametype(ctx, "mine"); err != nil {
			t.Fatal(err)
		}
		_, err = admin.Gametype(ctx, "mine")
		refused(t, err, 404, "not_found")
		_, err = admin.Gametype(ctx, "Not A Name")
		refused(t, err, 400, "bad_request")
	})

	t.Run("maps", func(t *testing.T) {
		maps, err := admin.Maps(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, m := range maps {
			names = append(names, m.Name)
			if m.Level == "" || m.Kind == "" {
				t.Errorf("map %+v", m)
			}
		}
		slices.Sort(names)
		if got := strings.Join(names, ","); got != "a10,bloodgulch,chillout,dangercanyon,hangemhigh,prisoner,sidewinder" {
			t.Errorf("maps %s", got)
		}
	})

	t.Run("bans", func(t *testing.T) {
		n, err := admin.AddBan(ctx, "198.51.100.7", "", "Someone", "1h", "a test")
		if err != nil {
			t.Fatal(err)
		}
		bans, err := admin.Bans(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var found *Ban
		for i := range bans {
			if bans[i].N == n {
				found = &bans[i]
			}
		}
		if found == nil || found.IP != "198.51.100.7" || found.Players != "Someone" || found.Reason != "a test" ||
			found.Until == nil || *found.Until < time.Now().Add(50*time.Minute).Unix() {
			t.Fatalf("ban %d: %+v in %+v", n, found, bans)
		}
		_, err = admin.AddBan(ctx, "", "", "Nobody", "", "")
		refused(t, err, 400, "schema")
		if err := admin.DeleteBan(ctx, n); err != nil {
			t.Fatal(err)
		}
		bans, _ = admin.Bans(ctx)
		for _, b := range bans {
			if b.IP == "198.51.100.7" {
				t.Errorf("the ban is still there: %+v", b)
			}
		}
		refused(t, admin.DeleteBan(ctx, 999), 404, "not_found")
	})

	t.Run("command", func(t *testing.T) {
		lines, err := admin.Command(ctx, "sv_version")
		if err != nil || len(lines) != 1 || !strings.Contains(lines[0], "build-900") {
			t.Errorf("sv_version: %q, %v", lines, err)
		}
		_, err = admin.Command(ctx, "sv_nosuch")
		refused(t, err, 404, "not_found")
		_, err = admin.Command(ctx, "sv_stop now")
		refused(t, err, 403, "forbidden")
		lines, err = admin.Command(ctx, "say")
		e := refused(t, err, 400, "failed")
		if len(lines) != 1 || lines[0] != "say <text>" || !strings.Contains(e.Error(), "say <text>") {
			t.Errorf("a failed command's lines %q (%v)", lines, e)
		}
	})

	t.Run("log", func(t *testing.T) {
		lines, next, err := admin.Log(ctx, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		signedIn := false
		for _, l := range lines {
			signedIn = signedIn || strings.Contains(l.Text, "serverapi test signed in")
			if l.N <= 0 || l.Time == 0 || l.Level == "" {
				t.Errorf("line %+v", l)
			}
		}
		if !signedIn || next != lines[len(lines)-1].N {
			t.Errorf("%d lines (signed in %v), next %d", len(lines), signedIn, next)
		}
		if more, after, err := admin.Log(ctx, next+1000, 100); err != nil || len(more) != 0 || after != next+1000 {
			t.Errorf("after the end: %d lines, next %d, %v", len(more), after, err)
		}
	})

	t.Run("events", func(t *testing.T) {
		events, next, err := admin.Events(ctx, 0)
		if err != nil || len(events) == 0 {
			t.Fatalf("%d events, %v", len(events), err)
		}
		var first struct {
			Seq   int64  `json:"seq"`
			Event string `json:"event"`
		}
		json.Unmarshal(events[0], &first)
		if first.Seq != 1 || first.Event != "server_started" {
			t.Errorf("the first event %s", events[0])
		}
		if later, _, err := admin.Events(ctx, next); err != nil || len(later) > 50 {
			t.Errorf("after %d: %d events, %v", next, len(later), err)
		}
	})

	t.Run("rcon", func(t *testing.T) { testRCON(t, ctx, base, admin) })

	t.Run("stop", func(t *testing.T) {
		refused(t, admin.Stop(ctx, "game", "", false), 403, "forbidden")
		// (the last: the server stops once its game ends)
		if err := owner.Stop(ctx, "game", "maintenance", true); err != nil {
			t.Fatal(err)
		}
		s, err := owner.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if s.Drain == nil || s.Drain.After != "game" || !s.Drain.Restart {
			t.Errorf("drain %+v", s.Drain)
		}
	})
}

// next waits for an event that matches.
func next(t *testing.T, r *RCON, match func(Message) bool) Message {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-r.Events():
			if !ok {
				t.Fatalf("the RCON closed: %v", r.Err())
			}
			if match(m) {
				return m
			}
		case <-timeout:
			t.Fatal("no such event")
		}
	}
}

func testRCON(t *testing.T, ctx context.Context, base string, admin *Client) {
	r, err := DialRCON(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Hello.Protocol != 1 || r.Hello.Build != "build-900" || r.Hello.NetworkVersion != 39 || r.Hello.Server == "" {
		t.Errorf("hello %+v", r.Hello)
	}
	if !r.Hello.Has("events") || r.Hello.Has("teleport") || r.Role != "admin" {
		t.Errorf("capabilities %v, role %q", r.Hello.Capabilities, r.Role)
	}

	if err := r.Subscribe(Groups, 0); err != nil {
		t.Fatal(err)
	}
	status := next(t, r, func(m Message) bool { return m.Event == "status" })
	var answer struct {
		Status Status `json:"status"`
	}
	if err := json.Unmarshal(status.Raw, &answer); err != nil || answer.Status.Build != "build-900" || status.Replay {
		t.Errorf("the status event %s: %v", status.Raw, err)
	}

	reply, err := r.Command(ctx, "sv_version")
	if err != nil || !reply.OK || len(reply.Lines) != 1 || !strings.Contains(reply.Lines[0], "build-900") {
		t.Errorf("sv_version: %+v, %v", reply, err)
	}
	if reply, err := r.Command(ctx, "sv_nosuch"); err != nil || reply.OK {
		t.Errorf("an unknown command: %+v, %v", reply, err)
	}
	if err := r.Ping(); err != nil {
		t.Errorf("ping: %v", err)
	}

	// a live event, and the same replayed to a later subscriber
	_, seq, err := admin.Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	n, err := admin.AddBan(ctx, "198.51.100.8", "", "Replayed", "1h", "")
	if err != nil {
		t.Fatal(err)
	}
	defer admin.DeleteBan(ctx, n)
	isBan := func(m Message) bool { return m.Event == "ban" && strings.Contains(string(m.Raw), "Replayed") }
	live := next(t, r, isBan)
	if live.Replay || live.Seq <= seq || live.Time == 0 {
		t.Errorf("the live event %+v", live)
	}
	later, err := DialRCON(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer later.Close()
	if err := later.Subscribe([]string{"moderation"}, seq); err != nil {
		t.Fatal(err)
	}
	replayed := next(t, later, isBan)
	if !replayed.Replay || replayed.Seq != live.Seq {
		t.Errorf("the replayed event %+v (live %d)", replayed, live.Seq)
	}

	// closed: the events end, a command is refused
	r.Close()
	select {
	case <-r.Done():
	case <-time.After(time.Second):
		t.Fatal("Done is not closed")
	}
	for range r.Events() {
	}
	if r.Err() == nil {
		t.Error("no reason for the closed connection")
	}
	if _, err := r.Command(ctx, "sv_version"); err == nil {
		t.Error("a command on a closed RCON")
	}

	// a session the server does not know: refused, and forgotten
	stale := &Client{Base: base, Password: adminPassword}
	stale.token, stale.role, stale.expires = "unknown", "admin", time.Now().Add(12*time.Hour)
	if _, err := DialRCON(ctx, stale); err == nil {
		t.Fatal("the RCON took an unknown session")
	} else if e, ok := AsError(err); !ok || e.Code != "denied" {
		t.Errorf("the refusal: %v", err)
	}
	if stale.token != "" {
		t.Error("the refused session was kept")
	}
	// (the next dial signs in again)
	again, err := DialRCON(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
}

// The client's reading of answers that are not the server's, against
// httptest.
func TestRefusals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login":
			if r.Header.Get("Content-Type") != "application/json" {
				http.Error(w, "no JSON", 400)
				return
			}
			fmt.Fprint(w, `{"ok":true,"token":"t","role":"viewer"}`)
		case "/api/status":
			if r.Header.Get("Authorization") != "Bearer t" {
				http.Error(w, "no token", 401)
				return
			}
			http.Error(w, "upstream down", http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &Client{Base: server.URL, HTTP: server.Client()}
	_, err := c.Status(context.Background())
	e := refused(t, err, 502, "http_502")
	if e.Message != "upstream down" || e.Error() != "http_502: upstream down" {
		t.Errorf("the refusal %q", e.Error())
	}
	// (no expiry: twelve hours)
	if time.Until(c.expires) < 11*time.Hour || c.Role() != "viewer" {
		t.Errorf("expires in %s, role %q", time.Until(c.expires), c.Role())
	}
	if (&Error{Status: 500, Code: "x"}).Error() != "x (500)" {
		t.Error("a refusal without a message")
	}
	if _, ok := AsError(fmt.Errorf("wrapped: %w", &Error{Code: "x"})); !ok {
		t.Error("AsError reads no wrapped refusal")
	}
	if _, ok := AsError(errors.New("x")); ok {
		t.Error("AsError reads a refusal from another error")
	}

	noToken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer noToken.Close()
	if err := (&Client{Base: noToken.URL}).Login(context.Background()); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Errorf("a sign-in without a token: %v", err)
	}
}

func TestParseStatus(t *testing.T) {
	s, err := ParseStatus([]byte(`{"ok":true,"state":"playing","players":3,"team_scores":[1,2],"capabilities":["stop"],
		"drain":{"after":"game","restart":true,"since":5},"rotation":{"index":1,"count":2,"next":{"text":"a:b","map":"a","gametype":"b","rejected":null}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.State != "playing" || s.Players != 3 || !slices.Equal(s.TeamScores, []int{1, 2}) || !s.Has("stop") ||
		s.Drain == nil || !s.Drain.Restart || s.Rotation.Next == nil || s.Rotation.Next.Rejected != nil {
		t.Errorf("status %+v", s)
	}
	if _, err := ParseStatus([]byte("not json")); err == nil {
		t.Error("no error for garbage")
	}
}
