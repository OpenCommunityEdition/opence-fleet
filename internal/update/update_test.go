package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersions(t *testing.T) {
	for _, c := range []struct {
		a, b  string
		newer bool
	}{{"v1.2.3", "v1.2.2", true}, {"v1.10.0", "v1.9.9", true}, {"v2.0.0", "v1.99.99", true},
		{"v1.2.3", "v1.2.3", false}, {"v1.2.2", "v1.2.3", false}, {"v1.2.3", "dev", false}, {"dev", "v1.0.0", false},
		{"v1.2", "v1.0.0", false}, {"v1.2.3-rc1", "v1.0.0", false}, {"v01.2.3", "v1.0.0", false}} {
		if Newer(c.a, c.b) != c.newer {
			t.Errorf("Newer(%s, %s) != %v", c.a, c.b, c.newer)
		}
	}
}

// release serves a release: its JSON, a binary (a script that prints its
// version), SHA256SUMS and their signature, changed by tamper.
func release(t *testing.T, key ed25519.PrivateKey, version string, tamper func(files map[string][]byte)) *httptest.Server {
	t.Helper()
	binary := []byte("#!/bin/sh\necho 'opence-fleet " + version + " (protocol 1)'\n")
	sum := sha256.Sum256(binary)
	sums := []byte(fmt.Sprintf("%s  %s\n%s  opence-fleet-other-arch\n", hex.EncodeToString(sum[:]), AssetName(),
		strings.Repeat("0", 64)))
	files := map[string][]byte{AssetName(): binary, "SHA256SUMS": sums,
		"SHA256SUMS.sig": []byte(hex.EncodeToString(ed25519.Sign(key, sums)) + "\n")}
	if tamper != nil {
		tamper(files)
	}
	mux := http.NewServeMux()
	var server *httptest.Server
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		type asset struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}
		var assets []asset
		for name := range files {
			assets = append(assets, asset{name, server.URL + "/redirect/" + name})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": version, "html_url": "https://example.org/r",
			"body": "notes", "assets": assets})
	})
	mux.HandleFunc("/redirect/{name}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/files/"+r.PathValue("name"), http.StatusFound)
	})
	mux.HandleFunc("/files/{name}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(files[r.PathValue("name")])
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func updater(t *testing.T, server *httptest.Server, key ed25519.PublicKey) *Updater {
	return &Updater{Current: "v1.0.0", Mode: Notify, Source: server.URL + "/latest", Dir: t.TempDir(),
		Client: server.Client(), Key: key, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestInstall(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(nil)
	server := release(t, private, "v1.1.0", nil)
	u := updater(t, server, public)
	var told []string
	u.Available = func(r Release) { told = append(told, r.Version) }
	r, err := u.Check(context.Background())
	if err != nil || r.Version != "v1.1.0" || r.Notes != "notes" || !u.Status().Available {
		t.Fatalf("%v %+v", err, r)
	}
	u.Check(context.Background())
	if len(told) != 1 {
		t.Fatalf("told %v", told)
	}
	// an old binary is removed by the install
	os.MkdirAll(u.Dir, 0o755)
	os.WriteFile(filepath.Join(u.Dir, "opence-fleet-v0.9.0"), []byte("old"), 0o755)
	binary, err := u.Install(context.Background(), r)
	if err != nil || binary != filepath.Join(u.Dir, "opence-fleet-v1.1.0") {
		t.Fatalf("%v %s", err, binary)
	}
	if s := readState(u.Dir); s.Current != "v1.1.0" || s.Good != "" || s.Starts != 0 {
		t.Fatalf("state %+v", s)
	}
	if _, err := os.Stat(filepath.Join(u.Dir, "opence-fleet-v0.9.0")); !os.IsNotExist(err) {
		t.Fatal("the old binary is kept")
	}
	MarkGood(u.Dir, "v1.1.0")
	if s := readState(u.Dir); s.Good != "v1.1.0" {
		t.Fatalf("state %+v", s)
	}
	// Launch runs nothing older than the program (a newer image)
	Launch(u.Dir, "v1.2.0", u.Log)
}

func TestRefusedReleases(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(nil)
	_, other, _ := ed25519.GenerateKey(nil)
	for name, c := range map[string]struct {
		key    ed25519.PrivateKey
		tamper func(map[string][]byte)
		want   string
	}{
		"another key's signature": {other, nil, "not signed by the release key"},
		"a changed binary": {private, func(f map[string][]byte) {
			f[AssetName()] = append(f[AssetName()], '#')
		}, "SHA-256 is not"},
		"no signature": {private, func(f map[string][]byte) { delete(f, "SHA256SUMS.sig") }, "no signed SHA256SUMS"},
		"no binary for this machine": {private, func(f map[string][]byte) { delete(f, AssetName()) },
			"this machine's binary"},
		"a binary that does not run": {private, nil, "does not run here"},
	} {
		t.Run(name, func(t *testing.T) {
			version := "v1.1.0"
			if name == "a binary that does not run" {
				// (it prints another version)
				version = "v1.1.0"
				c.tamper = func(f map[string][]byte) {
					f[AssetName()] = []byte("#!/bin/sh\necho 'opence-fleet v0.0.1 (protocol 1)'\n")
					sum := sha256.Sum256(f[AssetName()])
					f["SHA256SUMS"] = []byte(hex.EncodeToString(sum[:]) + "  " + AssetName() + "\n")
					f["SHA256SUMS.sig"] = []byte(hex.EncodeToString(ed25519.Sign(private, f["SHA256SUMS"])))
				}
			}
			u := updater(t, release(t, c.key, version, c.tamper), public)
			r, err := u.Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := u.Install(context.Background(), r); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("installed: %v", err)
			}
			if s := readState(u.Dir); s.Current != "" {
				t.Fatalf("state %+v", s)
			}
		})
	}
}

// Start refuses without a newer release; a failed install is told.
func TestStart(t *testing.T) {
	public, _, _ := ed25519.GenerateKey(nil)
	_, other, _ := ed25519.GenerateKey(nil)
	u := updater(t, release(t, other, "v1.1.0", nil), public)
	if err := u.Start("admin"); err == nil {
		t.Fatal("started without a release")
	}
	u.Check(context.Background())
	failed := make(chan error, 1)
	started := ""
	u.Started = func(v, actor string) { started = v + " " + actor }
	u.Failed = func(v string, err error) { failed <- err }
	u.Restart = func(string) { t.Error("restarted") }
	if err := u.Start("admin"); err != nil || started != "v1.1.0 admin" {
		t.Fatalf("%v %q", err, started)
	}
	if err := <-failed; !strings.Contains(err.Error(), "not signed") || u.Status().Updating || u.Status().Error == "" {
		t.Fatalf("%v %+v", err, u.Status())
	}
}

func TestBackupDatabase(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "fleet.db")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// a new database: nothing to back up
	if err := BackupDatabase(db, "v1.0.0", log); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(db, []byte("data 1"), 0o600)
	os.WriteFile(db+"-wal", []byte("wal 1"), 0o600)
	BackupDatabase(db, "v1.0.0", log)
	if matches, _ := filepath.Glob(db + ".before-*"); len(matches) != 0 {
		t.Fatalf("backed up for the same version: %v", matches)
	}
	for _, v := range []string{"v1.1.0", "v1.2.0", "v1.3.0", "v1.4.0"} {
		if err := BackupDatabase(db, v, log); err != nil {
			t.Fatal(err)
		}
	}
	if data, _ := os.ReadFile(db + ".before-v1.4.0-wal"); string(data) != "wal 1" {
		t.Fatalf("the log not backed up: %q", data)
	}
	if _, err := os.Stat(db + ".before-v1.1.0"); !os.IsNotExist(err) {
		t.Fatal("more than three backups kept")
	}
	if _, err := os.Stat(db + ".before-v1.2.0"); err != nil {
		t.Fatal(err)
	}
	// development builds back nothing up
	BackupDatabase(db, "dev", log)
	if data, _ := os.ReadFile(db + ".version"); string(data) != "v1.4.0\n" {
		t.Fatalf("%q", data)
	}
}

// CheckNow reads the latest release at once; in auto mode it installs a
// newer one, and it refuses with the updates off or in a development build.
func TestCheckNow(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(nil)
	server := release(t, private, "v1.1.0", nil)
	u := updater(t, server, public)
	u.Started = func(v, actor string) { t.Errorf("started %s by %s in notify mode", v, actor) }
	r, newer, err := u.CheckNow(context.Background(), "user:paul")
	if err != nil || r.Version != "v1.1.0" || !newer || !u.Status().Available || u.Status().Checked.IsZero() {
		t.Fatalf("%v %+v %v %+v", err, r, newer, u.Status())
	}
	u.Current = "v1.1.0"
	if _, newer, err := u.CheckNow(context.Background(), "user:paul"); err != nil || newer {
		t.Fatalf("the same release: %v %v", err, newer)
	}

	u = updater(t, server, public)
	u.Mode = Auto
	started := make(chan string, 1)
	u.Started = func(v, actor string) { started <- v + " " + actor }
	// (the install ends in Restart or Failed before the test's folder goes)
	ended := make(chan string, 1)
	u.Failed = func(_ string, err error) { ended <- err.Error() }
	u.Restart = func(string) { ended <- "restarted" }
	if _, newer, err := u.CheckNow(context.Background(), "user:paul"); err != nil || !newer {
		t.Fatalf("auto: %v %v", err, newer)
	}
	if got := <-started; got != "v1.1.0 user:paul" {
		t.Fatalf("auto started %q", got)
	}
	if got := <-ended; got != "restarted" {
		t.Fatalf("auto: %s", got)
	}

	for name, u := range map[string]*Updater{"off": {Current: "v1.0.0", Mode: Off}, "dev": {Current: "dev", Mode: Notify}} {
		if _, _, err := u.CheckNow(context.Background(), "user:paul"); err == nil {
			t.Errorf("%s: checked", name)
		}
	}
}
