// Package library is the fleet's content library: content-addressed blobs
// (FLEET_DATA/library/<sha256>), resumable uploads, and the checks an item
// passes before it is kept: a map's cache file header, read as the server
// reads it (the signature, the version: 5 for the Xbox's maps, 609 for
// Custom Edition's; the scenario's type: campaign, multiplayer or ui), and a
// gametype file's TOML. The maps are the operator's own copy of the game:
// the library serves them to the fleet's enrolled nodes alone.
package library

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The cache file versions.
const (
	VersionXbox          = 5
	VersionCustomEdition = 609
)

// The scenario types.
const (
	Campaign    = 0
	Multiplayer = 1
	UI          = 2
)

// Header is what a map's header says.
type Header struct {
	Version int
	Name    string
	Build   string
	Type    int
	Size    int64
}

// TypeName is the scenario type's word.
func (h Header) TypeName() string {
	switch h.Type {
	case Campaign:
		return "campaign"
	case Multiplayer:
		return "multiplayer"
	case UI:
		return "ui"
	}
	return fmt.Sprintf("type %d", h.Type)
}

// Edition is the map's game: xbox or custom.
func (h Header) Edition() string {
	if h.Version == VersionCustomEdition {
		return "custom"
	}
	return "xbox"
}

// ReadHeader reads a cache file's 2048-byte header.
func ReadHeader(r io.ReaderAt, size int64) (Header, error) {
	head := make([]byte, 0x800)
	if _, err := r.ReadAt(head, 0); err != nil {
		return Header{}, errors.New("not a cache file: shorter than its header")
	}
	if string(head[0:4]) != "daeh" || string(head[0x7fc:0x800]) != "toof" {
		return Header{}, errors.New("not a cache file: no head and foot signatures")
	}
	h := Header{Version: int(int32(binary.LittleEndian.Uint32(head[4:8]))), Size: size,
		Name: cstring(head[0x20:0x40]), Build: cstring(head[0x40:0x60]), Type: int(binary.LittleEndian.Uint16(head[0x60:0x62]))}
	if h.Version != VersionXbox && h.Version != VersionCustomEdition {
		return h, fmt.Errorf("a cache file of version %d: the server plays the Xbox's (5) and Custom Edition's (609)", h.Version)
	}
	if length := int64(binary.LittleEndian.Uint32(head[8:12])); h.Version == VersionXbox && length != size && size > 0 {
		return h, fmt.Errorf("the header says %d bytes, the file has %d: a truncated copy?", length, size)
	}
	return h, nil
}

func cstring(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}

var nameRule = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)

// The kinds of items.
const (
	KindMap           = "map"
	KindCustomMap     = "custom_map"
	KindResource      = "resource"
	KindGametype      = "gametype"
	KindSavedGametype = "saved_gametype"
)

// Resources are Custom Edition's shared resource maps.
var Resources = map[string]bool{"bitmaps": true, "sounds": true, "loc": true}

// CheckName checks an item's name for its kind.
func CheckName(kind, name string) error {
	if !nameRule.MatchString(name) {
		return fmt.Errorf("%q: a name is a-z, 0-9, '_', '-' and '.'", name)
	}
	switch kind {
	case KindMap, KindCustomMap, KindGametype, KindSavedGametype:
	case KindResource:
		if !Resources[name] {
			return fmt.Errorf("%s is not one of Custom Edition's resource maps (bitmaps, sounds, loc)", name)
		}
	default:
		return fmt.Errorf("%q is not a kind of the library's", kind)
	}
	return nil
}

// Classify finds an item's kind and name from a file's path in a game data
// folder (maps/x.map, custom_maps/y.map, gametypes/z.toml...).
func Classify(path string) (kind, name string, ok bool) {
	dir := strings.ToLower(filepath.Base(filepath.Dir(path)))
	base := strings.ToLower(filepath.Base(path))
	switch {
	case dir == "maps" && strings.HasSuffix(base, ".map"):
		return KindMap, strings.TrimSuffix(base, ".map"), true
	case dir == "custom_maps" && strings.HasSuffix(base, ".map"):
		name := strings.TrimSuffix(base, ".map")
		if Resources[name] {
			return KindResource, name, true
		}
		return KindCustomMap, name, true
	case dir == "gametypes" && strings.HasSuffix(base, ".toml"):
		return KindGametype, strings.TrimSuffix(base, ".toml"), true
	case dir == "gametypes" && strings.HasSuffix(base, ".gametype"):
		return KindSavedGametype, strings.TrimSuffix(base, ".gametype"), true
	}
	return "", "", false
}

// Check reads a map item's header and refuses one that will not do for its
// kind (a Custom Edition map among the Xbox's, a resource map that is not
// one): its header's words for the page.
func Check(kind, path string) (map[string]any, error) {
	meta := map[string]any{}
	switch kind {
	case KindMap, KindCustomMap, KindResource:
	default:
		return meta, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if kind == KindResource {
		// (bitmaps.map, sounds.map and loc.map are not cache files of a scenario)
		meta["size"] = info.Size()
		return meta, nil
	}
	h, err := ReadHeader(f, info.Size())
	if err != nil {
		return nil, err
	}
	if kind == KindMap && h.Version != VersionXbox {
		return nil, fmt.Errorf("%s is a Custom Edition map: add it as a custom map (custom_maps/)", h.Name)
	}
	if kind == KindCustomMap && h.Version != VersionCustomEdition {
		return nil, fmt.Errorf("%s is one of the Xbox's maps: add it as a map (maps/)", h.Name)
	}
	meta["scenario"] = h.Name
	meta["build"] = h.Build
	meta["type"] = h.TypeName()
	meta["edition"] = h.Edition()
	return meta, nil
}

// Store is the blobs' folder.
type Store struct {
	Dir string
}

// Path is a blob's file.
func (s *Store) Path(sha string) string { return filepath.Join(s.Dir, sha) }

// UploadPath is an upload's partial file.
func (s *Store) UploadPath(id string) string { return filepath.Join(s.Dir, "uploads", id+".part") }

// Has tells whether a blob is kept.
func (s *Store) Has(sha string) bool {
	_, err := os.Stat(s.Path(sha))
	return err == nil
}

// Hash is a file's SHA-256 in hexadecimal.
func Hash(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// Keep moves a file into the blobs by its hash (or copies it, keep true),
// returning its hash and size.
func (s *Store) Keep(path string, copy bool) (string, int64, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return "", 0, err
	}
	sha, size, err := Hash(path)
	if err != nil {
		return "", 0, err
	}
	target := s.Path(sha)
	if s.Has(sha) {
		if !copy {
			os.Remove(path)
		}
		return sha, size, nil
	}
	if !copy {
		if err := os.Rename(path, target); err == nil {
			os.Chmod(target, 0o600)
			return sha, size, nil
		}
	}
	in, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	part := target + ".part"
	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", 0, err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(part)
		return "", 0, err
	}
	if err := out.Close(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(part, target); err != nil {
		return "", 0, err
	}
	if !copy {
		os.Remove(path)
	}
	return sha, size, nil
}

// Remove removes a blob.
func (s *Store) Remove(sha string) error {
	err := os.Remove(s.Path(sha))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
