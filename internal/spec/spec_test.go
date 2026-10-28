package spec

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

var update = flag.Bool("update", false, "write the golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		os.WriteFile(path, []byte(got), 0o644)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (go test ./internal/spec -update writes it)", err)
	}
	if string(want) != got {
		t.Fatalf("%s differs:\n%s", name, got)
	}
}

func btb() Input {
	profile := map[string]any{"server.max_players": "16", "server.public": true, "network.text_chat": "ON",
		"network.voice_mode": "team_global", "server.password": "joinme", "server.motd": "Say \"hi\"\\ welcome",
		"network.voice_proximity": "20"}
	overrides := map[string]any{"server.max_players": 12.0, "server.name": "BTB EU #1"}
	return Input{Slug: "btb-eu-1", Name: "BTB EU #1", Generation: 3, Build: "build-211", Running: true,
		TunnelPort: 2303, WebPort: 8102, Loopback: 3, Settings: Merge(profile, overrides),
		Rotation: "bloodgulch:ctf, sidewinder:team_slayer+score=50", Region: "eu",
		InitTxt: "sv_map bloodgulch ctf\r\nsay hello\n\n", Gametypes: map[string]string{"btb_slayer": "base = \"team_slayer\"\n"},
		Password: "joinme", WebPassword: "w3bpassword-0123456789ab", PublisherSeed: strings.Repeat("ab", 32),
		PublicAddress: "203.0.113.5", LogAddresses: true, Needs: []string{"b", "a"}}
}

func TestRender(t *testing.T) {
	in := btb()
	instance := Render(in)
	golden(t, "btb.config.toml", instance.ConfigTOML)
	env, _ := json.MarshalIndent(instance.Env, "", "  ")
	golden(t, "btb.env.json", string(env)+"\n")
	golden(t, "btb.init.txt", instance.InitTxt)
	// the file is TOML the game reads, with the values typed
	var parsed map[string]map[string]any
	if _, err := toml.Decode(instance.ConfigTOML, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["server"]["max_players"] != int64(12) || parsed["server"]["motd"] != `Say "hi"\ welcome` ||
		parsed["network"]["text_chat"] != "on" || parsed["network"]["voice_proximity"] != 20.0 ||
		parsed["server"]["rotation"] != in.Rotation {
		t.Fatalf("%v", parsed)
	}
	// secrets are never in the file
	for _, secret := range []string{in.Password, in.WebPassword, in.PublisherSeed} {
		if strings.Contains(instance.ConfigTOML, secret) {
			t.Fatalf("%q in config.toml", secret)
		}
	}
	if instance.Env["HALO_NET_PUBLIC_ADDRESS"] != "203.0.113.5:2303" || instance.Env["HALO_SERVER_PASSWORD"] != "joinme" {
		t.Fatal(instance.Env)
	}
	if instance.Needs[0] != "a" {
		t.Fatal("needs are not sorted")
	}
	// a live change keeps the restart key; an infrastructure one changes it
	changed := in
	changed.Settings = Merge(in.Settings, map[string]any{"server.max_players": 8})
	if Render(changed).RestartKey() != instance.RestartKey() {
		t.Fatal("a live setting changed the restart key")
	}
	changed.WebPassword = "another-password-0123"
	if Render(changed).RestartKey() == instance.RestartKey() {
		t.Fatal("the web password (pinned by the environment) did not change the restart key")
	}
	changed.WebPassword = in.WebPassword
	changed.TunnelPort = 2304
	if Render(changed).RestartKey() == instance.RestartKey() {
		t.Fatal("a port did not change the restart key")
	}
}

func TestNormalize(t *testing.T) {
	for _, c := range []struct {
		name  string
		value any
		want  any
		err   string
	}{
		{"server.max_players", "16", int64(16), ""},
		{"server.max_players", "1", nil, "server.max_players takes a whole number from 2 to 127"},
		{"server.empty_seconds", "-1", nil, "server.empty_seconds takes a whole number from 0"},
		{"server.team_start", "BOTH", "both", ""},
		{"server.team_start", "all", nil, "server.team_start is one of one|both|skip"},
		{"server.public", "on", true, ""},
		{"server.public", "maybe", nil, "server.public is true or false"},
		{"server.name", "A very long server name", "A very long ser", ""},
		{"server.motd", "tab\there", nil, "server.motd takes printable ASCII"},
		{"network.voice_proximity", "12.5", 12.5, ""},
		{"server.web_port", "8080", nil, "server.web_port is not a setting a profile sets"},
		// (the Safety group's VPN policy and its linked Discord accounts)
		{"server.vpn_policy", "Block", "block", ""},
		{"server.vpn_policy", "deny", nil, "server.vpn_policy is one of off|link|block"},
		{"server.vpn_fail", "closed", "closed", ""},
		{"server.vpn_fail", "shut", nil, "server.vpn_fail is one of open|closed"},
		{"server.vpn_list", "/srv/netlist.bin", "/srv/netlist.bin", ""},
		{"server.vpn_check_lan", "yes", true, ""},
		{"server.link_min_age_days", "0", int64(0), ""},
		{"server.link_min_age_days", "3651", nil, "server.link_min_age_days takes a whole number from 0 to 3650"},
		{"server.link_one_seat", "false", false, ""},
	} {
		got, err := Normalize(c.name, c.value)
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Errorf("%s=%v: %v, want %q", c.name, c.value, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s=%v: %#v %v", c.name, c.value, got, err)
		}
	}
}

func TestRotation(t *testing.T) {
	entries := RotationEntries("# events\nbloodgulch:ctf, sidewinder:team_slayer+score=50;\ncustom_maps/hugeass:slayer\n# x, y\nprisoner")
	want := []string{"bloodgulch:ctf", "sidewinder:team_slayer+score=50", "custom_maps/hugeass:slayer", "prisoner"}
	if strings.Join(entries, "|") != strings.Join(want, "|") {
		t.Fatal(entries)
	}
	if RotationMap(entries[2]) != `custom_maps\hugeass` || RotationGametype(entries[1]) != "team_slayer" ||
		RotationGametype(entries[3]) != "slayer" {
		t.Fatal()
	}
}

func TestNames(t *testing.T) {
	if Slug("BTB EU #1") != "btb-eu-1" || Slug("  Co-op Night!! ") != "co-op-night" {
		t.Fatal(Slug("BTB EU #1"))
	}
	if Pattern("BTB EU #{n}", 3) != "BTB EU #3" || Pattern("Slayer", 2) != "Slayer 2" {
		t.Fatal()
	}
	if PublicAddress("2001:db8::1", 2302) != "[2001:db8::1]:2302" || PublicAddress("host:2400", 2302) != "host:2400" {
		t.Fatal()
	}
	r, err := ParsePortRange("2302-2399")
	if err != nil || !r.Contains(2302) || r.Contains(2400) {
		t.Fatal(r, err)
	}
	if _, err := ParsePortRange("80-90"); err == nil {
		t.Fatal("a privileged range parsed")
	}
	n := DefaultNode()
	if err := n.Check(); err != nil {
		t.Fatal(err)
	}
	n.WebPorts = PortRange{2350, 2360}
	if err := n.Check(); err == nil {
		t.Fatal("overlapping ranges passed")
	}
	img := Image{Build: "build-211", Ref: "ghcr.io/opencommunityedition/opence-server:build-211", Digest: "sha256:ab"}
	if img.Pull() != "ghcr.io/opencommunityedition/opence-server@sha256:ab" {
		t.Fatal(img.Pull())
	}
	img = Image{Ref: "localhost:5000/opence-server:dev"}
	if img.Pull() != "localhost:5000/opence-server:dev" {
		t.Fatal(img.Pull())
	}
	img.Digest = "sha256:cd"
	if img.Pull() != "localhost:5000/opence-server@sha256:cd" {
		t.Fatal(img.Pull())
	}
}

// The VPN policy's settings are the Safety group's, live but for the list
// file, which the server reads at its start and by sv_reload alone.
func TestVPNSettings(t *testing.T) {
	defaults := map[string]any{"server.vpn_policy": "link", "server.vpn_fail": "open", "server.vpn_list": "",
		"server.vpn_check_lan": false, "server.link_min_age_days": 365, "server.link_one_seat": true}
	for name, value := range defaults {
		s, ok := Lookup(name)
		if !ok {
			t.Errorf("%s is not in the table", name)
			continue
		}
		if s.Group != "Safety" || s.Default != value || s.Live != (name != "server.vpn_list") || s.Comment == "" ||
			s.Env != "HALO_"+strings.ToUpper(strings.ReplaceAll(name, ".", "_")) {
			t.Errorf("%+v", s)
		}
	}
}
