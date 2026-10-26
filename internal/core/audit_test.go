package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

// Every change writes its audit row with its actor; no secret the fleet
// made or was given is in the audit or the log.
func TestAuditKeepsNoSecret(t *testing.T) {
	f, logs := fleet(t)
	ctx := context.Background()
	stop(f, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	var secrets []string

	join, err := f.JoinToken(ctx, "user:paul", "a", spec.DefaultNode())
	if err != nil {
		t.Fatal(err)
	}
	_, nodeToken, err := f.Join(ctx, JoinRequest{Token: join, Hostname: "box", Runtime: "docker", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	secrets = append(secrets, join, nodeToken)

	fixed, err := f.CreateProfile(ctx, "user:alice", ProfileInput{Name: "Private", PasswordPolicy: PasswordFixed,
		Password: "profile-password-7f3a", Settings: map[string]any{"server.password": "setting-password-91bc"}})
	if err != nil {
		t.Fatal(err)
	}
	secrets = append(secrets, "profile-password-7f3a", "setting-password-91bc")
	perInstance := profile(t, f, "Public")
	if err := f.UpdateProfile(ctx, "user:alice", perInstance, ProfileInput{Name: "Public", PasswordPolicy: PasswordPerInstance}); err != nil {
		t.Fatal(err)
	}
	slugs := append(servers(t, f, fixed, 1, false, "a"), servers(t, f, perInstance, 1, false, "a")...)
	for _, slug := range slugs {
		i := instance(t, f, slug)
		web, _ := f.Key.OpenString(i.WebPasswordEnc, "instance:"+slug+":web_password")
		owner, _ := f.Key.OpenString(i.OwnerPasswordEnc, "instance:"+slug+":owner_password")
		secrets = append(secrets, web, owner)
		if pw := f.JoinPassword(ctx, slug); pw != "" {
			secrets = append(secrets, pw)
		}
	}
	if f.JoinPassword(ctx, slugs[0]) != "profile-password-7f3a" || len(f.JoinPassword(ctx, slugs[1])) != 10 {
		t.Fatal("the join passwords")
	}
	if err := f.SetOverride(ctx, "user:bob", slugs[1], "server.password", "override-password-c0de"); err != nil {
		t.Fatal(err)
	}
	secrets = append(secrets, "override-password-c0de")
	if err := f.RotatePassword(ctx, "user:bob", slugs[1]); err != nil {
		t.Fatal(err)
	}
	web, _ := f.Key.OpenString(instance(t, f, slugs[1]).WebPasswordEnc, "instance:"+slugs[1]+":web_password")
	secrets = append(secrets, web)
	// (a command that sets a password: run on an offline node, audited all
	// the same)
	f.Command(ctx, "user:bob", slugs[0], "sv_password command-password-4e4e", "admin")
	secrets = append(secrets, "command-password-4e4e")
	if _, err := f.AddWebhook(ctx, "user:paul", "", "https://discord.com/api/webhooks/1/webhook-token-a1b2", "", nil); err != nil {
		t.Fatal(err)
	}
	secrets = append(secrets, "webhook-token-a1b2")
	keyID, err := f.MakeKey(ctx, "user:paul", "Main")
	if err != nil {
		t.Fatal(err)
	}
	key, _ := f.Store.GetKey(ctx, keyID)
	seed, err := f.Key.OpenString(key.SeedEnc, "key:"+keyID)
	if err != nil || seed == "" {
		t.Fatal(err)
	}
	secrets = append(secrets, seed)
	if err := f.SetSetting(ctx, "user:paul", "drain_deadline", "10m"); err != nil {
		t.Fatal(err)
	}
	if err := f.DeleteInstance(ctx, "user:paul", slugs[0]); err != nil {
		t.Fatal(err)
	}
	// (the node's agent connects with its token, and is sent its spec)
	connect(t, f, nodeToken, func(request) *agentproto.Reply { return &agentproto.Reply{OK: true} })
	until(t, "the agent's connect logged", func() bool { return strings.Contains(logs.String(), "an agent connected") })

	// each change, by whom
	want := []struct{ action, actor, target string }{
		{"node.join_token", "user:paul", "a"},
		{"node.enrol", "agent:a", "a"},
		{"profile.create", "user:alice", "Private"},
		{"profile.create", "user:paul", "Public"},
		{"profile.update", "user:alice", "Public"},
		{"instance.create", "user:paul", slugs[0]},
		{"instance.create", "user:paul", slugs[1]},
		{"instance.update", "user:bob", slugs[1]},
		{"instance.rotate_password", "user:bob", slugs[1]},
		{"instance.command", "user:bob", slugs[0]},
		{"webhook.add", "user:paul", "discord.com"},
		{"key.make", "user:paul", keyID},
		{"settings.set", "user:paul", "drain_deadline"},
		{"instance.delete", "user:paul", slugs[0]},
	}
	rows := audit(t, f)
	for _, w := range want {
		found := false
		for _, r := range rows {
			if r.Action == w.action && r.Actor == w.actor && r.Target == w.target {
				found = true
				if r.Time != f.Now().Unix() {
					t.Errorf("%s at %d", w.action, r.Time)
				}
			}
		}
		if !found {
			t.Errorf("no audit row %s by %s of %s", w.action, w.actor, w.target)
		}
	}
	// (a password's change is said, not shown)
	for _, r := range rows {
		switch {
		case r.Action == "instance.update" && !strings.Contains(r.Detail, `"server.password":"(changed)"`),
			r.Action == "profile.create" && r.Target == "Private" && !strings.Contains(r.Detail, `"server.password":"(changed)"`),
			r.Action == "instance.command" && r.Detail != "sv_password (a password's command)":
			t.Errorf("%s: %s", r.Action, r.Detail)
		}
	}
	noSecret(t, f, logs, secrets...)
}
