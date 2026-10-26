package library

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// header is a synthetic cache file (tools/fakehalo's layout): its header's
// signatures, version, size, name, build and scenario type, and nothing of
// a game; size is the file's length and the size the header says.
func header(name string, kind, version, size int) []byte {
	data := make([]byte, size)
	copy(data[0:4], "daeh")
	binary.LittleEndian.PutUint32(data[4:8], uint32(version))
	binary.LittleEndian.PutUint32(data[8:12], uint32(size))
	copy(data[0x20:0x40], name)
	copy(data[0x40:0x60], "01.10.12.2276")
	binary.LittleEndian.PutUint16(data[0x60:0x62], uint16(kind))
	copy(data[0x7fc:0x800], "toof")
	return data
}

func write(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadHeader(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		version  int
		kind     string
		edition  string
		scenario string
	}{
		{"xbox multiplayer", header("bloodgulch", Multiplayer, VersionXbox, 4096), 5, "multiplayer", "xbox", "bloodgulch"},
		{"xbox campaign", header("a10", Campaign, VersionXbox, 4096), 5, "campaign", "xbox", "a10"},
		{"xbox ui", header("ui", UI, VersionXbox, 4096), 5, "ui", "xbox", "ui"},
		{"custom edition", header("deathisland", Multiplayer, VersionCustomEdition, 4096), 609, "multiplayer", "custom", "deathisland"},
		// (Custom Edition's header says a size of its own: not checked)
		{"custom edition, another size", func() []byte {
			d := header("ratrace", Multiplayer, VersionCustomEdition, 4096)
			binary.LittleEndian.PutUint32(d[8:12], 1)
			return d
		}(), 609, "multiplayer", "custom", "ratrace"},
		{"the header alone", header("prisoner", Multiplayer, VersionXbox, 0x800), 5, "multiplayer", "xbox", "prisoner"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := ReadHeader(bytes.NewReader(tt.data), int64(len(tt.data)))
			if err != nil {
				t.Fatal(err)
			}
			if h.Version != tt.version || h.TypeName() != tt.kind || h.Edition() != tt.edition || h.Name != tt.scenario {
				t.Errorf("got %+v (%s, %s)", h, h.TypeName(), h.Edition())
			}
			if h.Build != "01.10.12.2276" || h.Size != int64(len(tt.data)) {
				t.Errorf("build %q, size %d", h.Build, h.Size)
			}
		})
	}
}

func TestReadHeaderRefuses(t *testing.T) {
	garbage := bytes.Repeat([]byte("not a map "), 400)
	noFoot := header("bloodgulch", Multiplayer, VersionXbox, 4096)
	copy(noFoot[0x7fc:0x800], "\x00\x00\x00\x00")
	tests := []struct {
		name string
		data []byte
		size int64
		want string
	}{
		{"empty", nil, 0, "shorter than its header"},
		{"shorter than the header", header("bloodgulch", Multiplayer, VersionXbox, 4096)[:0x7ff], 0x7ff, "shorter than its header"},
		{"garbage", garbage, int64(len(garbage)), "no head and foot signatures"},
		{"no foot", noFoot, 4096, "no head and foot signatures"},
		{"another version", header("bloodgulch", Multiplayer, 7, 4096), 4096, "version 7"},
		{"a truncated xbox map", header("bloodgulch", Multiplayer, VersionXbox, 4096)[:3000], 3000, "truncated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ReadHeader(bytes.NewReader(tt.data), tt.size)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("got %v, want an error with %q", err, tt.want)
			}
		})
	}
}

func TestHeaderWords(t *testing.T) {
	if got := (Header{Type: 7}).TypeName(); got != "type 7" {
		t.Errorf("TypeName: %q", got)
	}
	// (a name filling its 32 bytes has no NUL; spaces are trimmed)
	data := header(strings.Repeat("x", 40), Multiplayer, VersionXbox, 4096)
	copy(data[0x40:0x60], "  1.0  \x00junk")
	h, err := ReadHeader(bytes.NewReader(data), 4096)
	if err != nil {
		t.Fatal(err)
	}
	if h.Name != strings.Repeat("x", 32) || h.Build != "1.0" {
		t.Errorf("name %q, build %q", h.Name, h.Build)
	}
}

func TestCheckName(t *testing.T) {
	tests := []struct {
		kind, name string
		ok         bool
	}{
		{KindMap, "bloodgulch", true},
		{KindCustomMap, "deathisland_v2.1", true},
		{KindGametype, "team-slayer", true},
		{KindSavedGametype, "ctf_pro", true},
		{KindResource, "bitmaps", true},
		{KindResource, "sounds", true},
		{KindResource, "loc", true},
		{KindResource, "bloodgulch", false},
		{KindMap, "BloodGulch", false},
		{KindMap, "blood gulch", false},
		{KindMap, "../etc/passwd", false},
		{KindMap, `maps\x`, false},
		{KindMap, "", false},
		{KindMap, strings.Repeat("a", 64), true},
		{KindMap, strings.Repeat("a", 65), false},
		{"texture", "bloodgulch", false},
	}
	for _, tt := range tests {
		if err := CheckName(tt.kind, tt.name); (err == nil) != tt.ok {
			t.Errorf("CheckName(%q, %q): %v", tt.kind, tt.name, err)
		}
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		path, kind, name string
		ok               bool
	}{
		{"data/maps/bloodgulch.map", KindMap, "bloodgulch", true},
		{"data/Maps/BloodGulch.MAP", KindMap, "bloodgulch", true},
		{"data/custom_maps/deathisland.map", KindCustomMap, "deathisland", true},
		{"data/custom_maps/bitmaps.map", KindResource, "bitmaps", true},
		{"data/custom_maps/sounds.map", KindResource, "sounds", true},
		{"data/maps/bitmaps.map", KindMap, "bitmaps", true},
		{"data/gametypes/ctf_pro.toml", KindGametype, "ctf_pro", true},
		{"data/gametypes/old.gametype", KindSavedGametype, "old", true},
		{"data/maps/readme.txt", "", "", false},
		{"data/gametypes/x.map", "", "", false},
		{"data/other/bloodgulch.map", "", "", false},
		{"bloodgulch.map", "", "", false},
	}
	for _, tt := range tests {
		kind, name, ok := Classify(tt.path)
		if kind != tt.kind || name != tt.name || ok != tt.ok {
			t.Errorf("Classify(%q) = %q, %q, %v", tt.path, kind, name, ok)
		}
	}
}

func TestCheck(t *testing.T) {
	dir := t.TempDir()
	xbox := write(t, dir, "maps/bloodgulch.map", header("bloodgulch", Multiplayer, VersionXbox, 4096))
	campaign := write(t, dir, "maps/a10.map", header("a10", Campaign, VersionXbox, 4096))
	custom := write(t, dir, "custom_maps/deathisland.map", header("deathisland", Multiplayer, VersionCustomEdition, 4096))
	resource := write(t, dir, "custom_maps/bitmaps.map", bytes.Repeat([]byte{1}, 1000))
	garbage := write(t, dir, "maps/garbage.map", bytes.Repeat([]byte("garbage!"), 512))
	truncated := write(t, dir, "maps/prisoner.map", header("prisoner", Multiplayer, VersionXbox, 4096)[:3000])

	meta, err := Check(KindMap, xbox)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"scenario": "bloodgulch", "build": "01.10.12.2276", "type": "multiplayer", "edition": "xbox"}
	for k, v := range want {
		if meta[k] != v {
			t.Errorf("meta[%s] = %v, want %v", k, meta[k], v)
		}
	}
	if meta, err := Check(KindMap, campaign); err != nil || meta["type"] != "campaign" {
		t.Errorf("a campaign map: %v, %v", meta, err)
	}
	if meta, err := Check(KindCustomMap, custom); err != nil || meta["edition"] != "custom" {
		t.Errorf("a custom map: %v, %v", meta, err)
	}
	// (a resource map is not a scenario's cache file: its size alone)
	if meta, err := Check(KindResource, resource); err != nil || meta["size"] != int64(1000) {
		t.Errorf("a resource map: %v, %v", meta, err)
	}
	// (a gametype's TOML is checked against the schema by the caller,
	// internal/core: the library reads no file of it)
	if meta, err := Check(KindGametype, filepath.Join(dir, "nowhere.toml")); err != nil || len(meta) != 0 {
		t.Errorf("a gametype: %v, %v", meta, err)
	}

	refused := []struct {
		name, kind, path, want string
	}{
		{"a custom edition map among the xbox's", KindMap, custom, "custom map"},
		{"an xbox map among the custom ones", KindCustomMap, xbox, "add it as a map"},
		{"garbage", KindMap, garbage, "not a cache file"},
		{"a truncated map", KindMap, truncated, "truncated"},
		{"no file", KindMap, filepath.Join(dir, "maps/nowhere.map"), "no such file"},
	}
	for _, tt := range refused {
		if _, err := Check(tt.kind, tt.path); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: got %v, want an error with %q", tt.name, err, tt.want)
		}
	}
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestHash(t *testing.T) {
	dir := t.TempDir()
	data := []byte("a blob's bytes")
	sha, size, err := Hash(write(t, dir, "blob", data))
	if err != nil || sha != digest(data) || size != int64(len(data)) {
		t.Errorf("Hash: %s, %d, %v", sha, size, err)
	}
	if _, _, err := Hash(filepath.Join(dir, "nowhere")); err == nil {
		t.Error("Hash of no file: no error")
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: filepath.Join(dir, "library")}
	if got := s.Path("abc"); got != filepath.Join(dir, "library", "abc") {
		t.Errorf("Path: %s", got)
	}
	if got := s.UploadPath("u1"); got != filepath.Join(dir, "library", "uploads", "u1.part") {
		t.Errorf("UploadPath: %s", got)
	}

	// kept by moving: the blob is named by its hash, the source is gone
	data := header("bloodgulch", Multiplayer, VersionXbox, 4096)
	source := write(t, dir, "upload/bloodgulch.map", data)
	sha, size, err := s.Keep(source, false)
	if err != nil {
		t.Fatal(err)
	}
	if sha != digest(data) || size != 4096 {
		t.Errorf("Keep: %s, %d", sha, size)
	}
	if !s.Has(sha) {
		t.Error("the blob is not kept")
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Errorf("the source is still there: %v", err)
	}
	if got, _ := os.ReadFile(s.Path(sha)); !bytes.Equal(got, data) {
		t.Error("the blob's bytes are not the source's")
	}
	if info, err := os.Stat(s.Path(sha)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the blob's mode: %v, %v", info.Mode(), err)
	}

	// kept by copying: the source stays
	other := []byte("another blob")
	source = write(t, dir, "upload/other", other)
	sha2, _, err := s.Keep(source, true)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Has(sha2) || sha2 == sha {
		t.Errorf("the copy: %s", sha2)
	}
	if _, err := os.Stat(source); err != nil {
		t.Errorf("the copied source is gone: %v", err)
	}
	if _, err := os.Stat(s.Path(sha2) + ".part"); !os.IsNotExist(err) {
		t.Errorf("a .part is left: %v", err)
	}

	// the same bytes again: one blob, the second source dropped (moved) or
	// left (copied)
	again := write(t, dir, "upload/again.map", data)
	if got, _, err := s.Keep(again, true); err != nil || got != sha {
		t.Errorf("the same bytes copied: %s, %v", got, err)
	}
	if _, err := os.Stat(again); err != nil {
		t.Errorf("the copied duplicate is gone: %v", err)
	}
	if got, _, err := s.Keep(again, false); err != nil || got != sha {
		t.Errorf("the same bytes moved: %s, %v", got, err)
	}
	if _, err := os.Stat(again); !os.IsNotExist(err) {
		t.Errorf("the moved duplicate is still there: %v", err)
	}
	entries, _ := os.ReadDir(s.Dir)
	if len(entries) != 2 {
		t.Errorf("%d blobs, want 2", len(entries))
	}

	if _, _, err := s.Keep(filepath.Join(dir, "nowhere"), false); err == nil {
		t.Error("Keep of no file: no error")
	}

	if err := s.Remove(sha); err != nil {
		t.Fatal(err)
	}
	if s.Has(sha) {
		t.Error("the removed blob is still kept")
	}
	if err := s.Remove(sha); err != nil {
		t.Errorf("removing a removed blob: %v", err)
	}
	if !s.Has(sha2) {
		t.Error("the other blob went with it")
	}
}
