package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

func TestOpenMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, err := s.CreateUser(ctx, db.CreateUserParams{Name: "paul", PasswordHash: "x", Role: "owner", CreatedAt: 1})
	if err != nil || id != 1 {
		t.Fatal(err, id)
	}
	s.Close()
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the database's mode is %v", info.Mode())
	}
	// a second open applies nothing again and keeps the rows
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n, _ := s.CountUsers(ctx); n != 1 {
		t.Fatalf("%d users", n)
	}
	names, _ := Migrations()
	var version int
	s.DB.QueryRow(`SELECT max(version) FROM schema_version`).Scan(&version)
	if version != len(names) {
		t.Fatalf("version %d of %d", version, len(names))
	}
	if u, err := s.GetUserByName(ctx, "PAUL"); err != nil || u.ID != 1 {
		t.Fatal("names are not matched without case", err)
	}
}

func TestPruneAndSnapshot(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	s.InsertEvent(ctx, db.InsertEventParams{Time: now.Add(-40 * 24 * time.Hour).UnixMilli(), Slug: "a", Type: "x", Data: "{}"})
	s.InsertEvent(ctx, db.InsertEventParams{Time: now.UnixMilli(), Slug: "a", Type: "x", Data: "{}"})
	s.InsertNodeSample(ctx, db.InsertNodeSampleParams{NodeID: 1, Time: now.Add(-8 * 24 * time.Hour).Unix()})
	s.InsertNodeSample(ctx, db.InsertNodeSampleParams{NodeID: 1, Time: now.Add(-6 * 24 * time.Hour).Unix()})
	if err := s.Prune(ctx, now, 30, 7); err != nil {
		t.Fatal(err)
	}
	if samples, _ := s.ListNodeSamples(ctx, db.ListNodeSamplesParams{NodeID: 1}); len(samples) != 1 {
		t.Fatalf("%d node samples kept", len(samples))
	}
	events, _ := s.ListEventsBySlug(ctx, db.ListEventsBySlugParams{Slug: "a", ID: 1 << 62, Limit: 10})
	if len(events) != 1 {
		t.Fatalf("%d events kept", len(events))
	}
	path := filepath.Join(t.TempDir(), "copy.db")
	if err := s.Snapshot(ctx, path); err != nil {
		t.Fatal(err)
	}
	if NotFound(nil) {
		t.Fatal()
	}
}

// openAt makes a database of an older release: its schema up to a version.
func openAt(t *testing.T, path string, version int) *Store {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{Queries: db.New(conn), DB: conn, Path: path}
	if err := s.migrateTo(context.Background(), version); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestUpdatePolicyOneOff(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fleet.db")
	// a fleet of the release before: an owner, and its own policy and check
	old := openAt(t, path, 4)
	old.CreateUser(ctx, db.CreateUserParams{Name: "paul", PasswordHash: "x", Role: "owner", CreatedAt: 1})
	old.SetSetting(ctx, db.SetSettingParams{Name: "server_update_mode", Value: "check"})
	old.SetSetting(ctx, db.SetSettingParams{Name: "release_check_interval", Value: "6h"})
	old.Close()

	// its first start on this release sets both, once, and marks it
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"server_update_mode": "install", "release_check_interval": "30m",
		"update_policy_migrated": "pending"} {
		if got := s.Setting(ctx, name); got != want {
			t.Errorf("%s is %q after the upgrade, not %q", name, got, want)
		}
	}
	// an admin changes them afterwards, and the start tells the owners
	s.SetSetting(ctx, db.SetSettingParams{Name: "server_update_mode", Value: "check"})
	s.SetSetting(ctx, db.SetSettingParams{Name: "release_check_interval", Value: "2h"})
	s.SetSetting(ctx, db.SetSettingParams{Name: "update_policy_migrated", Value: "v0.2.0"})
	s.Close()

	// a later start (or upgrade) applies nothing again
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for name, want := range map[string]string{"server_update_mode": "check", "release_check_interval": "2h",
		"update_policy_migrated": "v0.2.0"} {
		if got := s.Setting(ctx, name); got != want {
			t.Errorf("%s is %q after a second start, not the admin's %q", name, got, want)
		}
	}
}

func TestUpdatePolicyNewFleet(t *testing.T) {
	// a new fleet keeps no value: the defaults (and a variable's first
	// value) are its
	s, err := Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, name := range []string{"server_update_mode", "release_check_interval", "update_policy_migrated"} {
		if got := s.Setting(context.Background(), name); got != "" {
			t.Errorf("a new fleet has %s %q", name, got)
		}
	}
}
