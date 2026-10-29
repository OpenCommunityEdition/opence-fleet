package store

import (
	"context"
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
