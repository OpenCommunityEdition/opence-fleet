// Package store is the fleet's database: SQLite through modernc.org/sqlite
// (no CGO), its schema in migrations/ (embedded, applied in order at the
// start: the schema only grows), and its queries in queries/, from which
// sqlc writes package db (sh tools/generate.sh). Everything the fleet keeps
// is here but the files of FLEET_DATA (the library's blobs, the releases'
// zips, fleet.key).
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is the database.
type Store struct {
	*db.Queries
	DB   *sql.DB
	Path string
}

// Open opens (making it, mode 0600) the database and brings its schema up
// to date.
func Open(path string) (*Store, error) {
	memory := path == ":memory:" || strings.HasPrefix(path, "file::memory:")
	if !memory {
		if dir := filepath.Dir(path); dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
		}
		// (made by us with 0600 before SQLite makes it with 0644)
		if f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600); err == nil {
			f.Close()
		}
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_txlock=immediate"
	if memory {
		// (one connection: each would be its own empty database)
		dsn = "file::memory:?_pragma=foreign_keys(1)&_txlock=immediate"
	} else {
		dsn += "&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	}
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if memory {
		conn.SetMaxOpenConns(1)
	} else {
		conn.SetMaxOpenConns(8)
	}
	s := &Store{Queries: db.New(conn), DB: conn, Path: path}
	if err := s.migrate(context.Background()); err != nil {
		conn.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.DB.Close() }

// Migrations is the schema's versions, in order.
func Migrations() ([]string, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	current := 0
	if err := s.DB.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return err
	}
	names, err := Migrations()
	if err != nil {
		return err
	}
	if current > len(names) {
		return fmt.Errorf("the database's schema (%d) is newer than this program's (%d): run the newer release, "+
			"or restore the backup made before it (fleet.db.before-<version>)", current, len(names))
	}
	for i, name := range names {
		version := i + 1
		if version <= current {
			continue
		}
		prefix, _, _ := strings.Cut(name, "_")
		if n, err := strconv.Atoi(prefix); err != nil || n != version {
			return fmt.Errorf("migration %s is not number %d", name, version)
		}
		text, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(text)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, version); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Tx runs fn in a transaction (committed when fn returns nil).
func (s *Store) Tx(ctx context.Context, fn func(q *db.Queries) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(s.Queries.WithTx(tx)); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// NotFound tells whether err is a row that is not there.
func NotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// Setting reads a setting of the settings table ("" when unset).
func (s *Store) Setting(ctx context.Context, name string) string {
	value, err := s.GetSetting(ctx, name)
	if err != nil {
		return ""
	}
	return value
}

// Prune removes what is past its retention: events and games older than
// eventsDays, samples older than samplesDays, expired sessions and join
// tokens.
func (s *Store) Prune(ctx context.Context, now time.Time, eventsDays, samplesDays int) error {
	events := now.Add(-time.Duration(eventsDays) * 24 * time.Hour)
	samples := now.Add(-time.Duration(samplesDays) * 24 * time.Hour)
	return errors.Join(
		s.PruneEvents(ctx, events.UnixMilli()),
		s.PruneGames(ctx, events.Unix()),
		s.PruneSamples(ctx, samples.Unix()),
		s.DeleteExpiredSessions(ctx, now.Unix()),
		s.DeleteOldJoinTokens(ctx, now.Add(-24*time.Hour).Unix()),
	)
}

// Snapshot writes a consistent copy of the database to path (VACUUM INTO),
// for `opence-fleet export --database` and the scheduled exports.
func (s *Store) Snapshot(ctx context.Context, path string) error {
	_ = os.Remove(path)
	_, err := s.DB.ExecContext(ctx, `VACUUM INTO ?`, path)
	if err == nil {
		err = os.Chmod(path, 0o600)
	}
	return err
}
