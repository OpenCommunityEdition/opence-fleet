package web_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/OpenCommunityEdition/opence-fleet/internal/auth"
	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
	"github.com/OpenCommunityEdition/opence-fleet/internal/serverapi"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view/pages"
	"github.com/OpenCommunityEdition/opence-fleet/internal/web"
)

// The Settings tab's form saves the settings that changed, all at once:
// one left alone stays its profile's, a reset gives one back with the
// others saved, and a wrong value saves none.
func TestServerSettingsForm(t *testing.T) {
	x := newFixture(t, nil, true)
	slug := x.instance()
	ctx := context.Background()
	i, err := x.fleet.Instance(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.fleet.UpdateProfile(ctx, "test", *i.ProfileID, core.ProfileInput{Name: "BTB",
		Settings: map[string]any{"server.motd": "welcome"}}); err != nil {
		t.Fatal(err)
	}
	post := func(token, csrf string, values url.Values) int {
		values.Set("csrf", csrf)
		return x.do(signed(request(http.MethodPost, "/servers/"+slug+"/settings", loopback, form(values)), token)).Code
	}
	overrides := func() map[string]any {
		i, err := x.fleet.Instance(ctx, slug)
		if err != nil {
			t.Fatal(err)
		}
		return i.Overrides.Settings
	}

	// the page is the form, its fields shown with their values
	page := x.do(signed(request(http.MethodGet, "/servers/"+slug+"/settings", loopback, nil), x.cookie)).Body.String()
	for _, want := range []string{`action="/servers/` + slug + `/settings"`, `name="value.server.max_players"`,
		`name="shown.server.motd" value="welcome"`, "Save changes"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the page has no %s", want)
		}
	}

	if code := post(x.cookie, x.owner.CSRF, url.Values{
		"value.server.max_players": {"12"}, "shown.server.max_players": {"16"},
		"value.server.vpn_fail": {"closed"}, "shown.server.vpn_fail": {"open"},
		"value.server.motd": {"welcome"}, "shown.server.motd": {"welcome"},
		"value.server.name": {"OpenCE Server"}, "shown.server.name": {"OpenCE Server"},
	}); code != http.StatusSeeOther {
		t.Fatalf("save: %d", code)
	}
	got := overrides()
	if len(got) != 2 || got["server.max_players"] != int64(12) && got["server.max_players"] != float64(12) ||
		got["server.vpn_fail"] != "closed" {
		t.Fatalf("the overrides: %v", got)
	}

	// a reset, with another change saved beside it
	if code := post(x.cookie, x.owner.CSRF, url.Values{
		"value.server.max_players": {"12"}, "shown.server.max_players": {"12"},
		"value.server.motd": {"hello there"}, "shown.server.motd": {"welcome"},
		"reset": {"server.vpn_fail"},
	}); code != http.StatusSeeOther {
		t.Fatalf("reset: %d", code)
	}
	got = overrides()
	if _, ok := got["server.vpn_fail"]; ok || got["server.motd"] != "hello there" || len(got) != 2 {
		t.Fatalf("after the reset: %v", got)
	}

	// a wrong value saves nothing, not even the right ones beside it
	post(x.cookie, x.owner.CSRF, url.Values{
		"value.server.max_players": {"999"}, "shown.server.max_players": {"12"},
		"value.server.motd": {"changed"}, "shown.server.motd": {"hello there"},
	})
	if got := overrides(); got["server.motd"] != "hello there" {
		t.Fatalf("a refused form saved: %v", got)
	}

	// a moderator may not
	moderator := x.user(auth.Moderator)
	p := x.auth.Session(ctx, moderator)
	if code := post(moderator, p.CSRF, url.Values{"value.server.motd": {"mine"}}); code != http.StatusForbidden {
		t.Fatalf("a moderator: %d", code)
	}
}

// A setting the fleet sets shows the fleet's value, pending (with the
// served one) until the server serves it; a setting that is not live says
// so.
func TestSettingLinesPending(t *testing.T) {
	served := []serverapi.SettingInfo{
		{Name: "server.max_players", Type: "integer", Value: float64(16), Default: float64(16)},
		{Name: "server.motd", Type: "string", Value: "welcome", Default: ""},
		{Name: "server.name", Type: "string", Value: "Alpha", Default: "OpenCE Server"},
		{Name: "server.vpn_list", Type: "string", Value: "", Default: ""},
		{Name: "server.something_new", Type: "string", Value: "x"},
	}
	profile := map[string]any{"server.motd": "welcome", "server.vpn_list": "https://example.org/list.txt"}
	overrides := map[string]any{"server.max_players": int64(12)}
	lines, fromServer := web.SettingLinesOf(served, profile, overrides)
	if !fromServer {
		t.Fatal("not from the server")
	}
	byName := map[string]pages.SettingLine{}
	for _, l := range lines {
		byName[l.Name] = l
	}
	if l := byName["server.max_players"]; l.Value != "12" || !l.Pending || l.Served != "16" || !l.Override || !l.Live {
		t.Fatalf("an override not served yet: %+v", l)
	}
	if l := byName["server.motd"]; l.Value != "welcome" || l.Pending || l.Source != "its profile" {
		t.Fatalf("a profile's value served: %+v", l)
	}
	if l := byName["server.name"]; l.Value != "Alpha" || l.Pending || l.Source != "the default" {
		t.Fatalf("a setting the fleet leaves: %+v", l)
	}
	if l := byName["server.vpn_list"]; !l.Pending || l.Live || l.Value != "https://example.org/list.txt" {
		t.Fatalf("a restart setting: %+v", l)
	}
	if l := byName["server.something_new"]; l.Source != "the server" || l.Group != pages.OwnSettings || !l.Live || !l.Pinned {
		t.Fatalf("the server's own: %+v", l)
	}
	// (the server's own are last)
	if lines[len(lines)-1].Name != "server.something_new" {
		t.Fatalf("the order: %v", lines)
	}
}
