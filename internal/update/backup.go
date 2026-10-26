package update

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// backups is how many databases BackupDatabase keeps.
const backups = 3

// BackupDatabase copies the database before a version opens it for the
// first time (its schema may change it): fleet.db.before-<version>, with
// its write-ahead log. The last version that opened the database is in
// fleet.db.version. It keeps the three newest backups.
func BackupDatabase(db, version string, log *slog.Logger) error {
	if db == "" || strings.HasPrefix(db, ":memory:") || !Valid(version) {
		return nil
	}
	marker := db + ".version"
	last, _ := os.ReadFile(marker)
	if strings.TrimSpace(string(last)) == version {
		return nil
	}
	if _, err := os.Stat(db); err == nil {
		backup := db + ".before-" + version
		for _, suffix := range []string{"", "-wal"} {
			if err := copyFile(db+suffix, backup+suffix); err != nil && !(suffix != "" && os.IsNotExist(err)) {
				return fmt.Errorf("the database's backup: %w", err)
			}
		}
		log.Info("update: the database is backed up before this version opens it", "backup", backup,
			"last", strings.TrimSpace(string(last)))
		prune(db)
	}
	return os.WriteFile(marker, []byte(version+"\n"), 0o600)
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// prune removes all but the newest backups.
func prune(db string) {
	matches, _ := filepath.Glob(db + ".before-v*")
	type backup struct {
		path    string
		version string
	}
	var list []backup
	for _, m := range matches {
		v := strings.TrimPrefix(m, db+".before-")
		if Valid(v) {
			list = append(list, backup{m, v})
		}
	}
	sort.Slice(list, func(i, j int) bool { return Newer(list[i].version, list[j].version) })
	for _, b := range list[min(len(list), backups):] {
		_ = os.Remove(b.path)
		_ = os.Remove(b.path + "-wal")
	}
}
