package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/OpenCommunityEdition/opence-fleet/internal/library"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// MaximumItem is the largest library item (Custom Edition's biggest maps).
const MaximumItem = 4 << 30

// Blobs is the library's blobs.
func (f *Fleet) Blobs() *library.Store {
	return &library.Store{Dir: filepath.Join(f.Config.Data, "library")}
}

// Library lists the library's items.
func (f *Fleet) Library(ctx context.Context) ([]db.Library, error) { return f.Store.ListLibrary(ctx) }

// AddItem keeps a file as a library item (moved into the blobs, or copied),
// after its checks; an item of the same kind and name is replaced.
func (f *Fleet) AddItem(ctx context.Context, actor, kind, name, path string, copy bool) (db.Library, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if err := library.CheckName(kind, name); err != nil {
		return db.Library{}, Refuse(http.StatusBadRequest, "schema", "%v", err)
	}
	meta, err := library.Check(kind, path)
	if err != nil {
		return db.Library{}, Refuse(http.StatusBadRequest, "invalid", "%s: %v", name, err)
	}
	if kind == library.KindGametype {
		text, err := os.ReadFile(path)
		if err != nil {
			return db.Library{}, err
		}
		if err := CheckGametype(name, string(text)); err != nil {
			return db.Library{}, err
		}
		meta["text"] = string(text)
	}
	sha, size, err := f.Blobs().Keep(path, copy)
	if err != nil {
		return db.Library{}, err
	}
	if size > MaximumItem {
		return db.Library{}, Refuse(http.StatusRequestEntityTooLarge, "too_large", "an item is 4 GB at most")
	}
	data, _ := json.Marshal(meta)
	err = f.Store.Tx(ctx, func(q *db.Queries) error {
		if old, err := q.GetLibraryByName(ctx, db.GetLibraryByNameParams{Kind: kind, Name: name}); err == nil && old.Sha256 != sha {
			q.DeleteLibrary(ctx, old.Sha256)
		}
		return q.AddLibrary(ctx, db.AddLibraryParams{Sha256: sha, Kind: kind, Name: name, Size: size, Meta: string(data),
			AddedBy: actor, AddedAt: f.Now().Unix()})
	})
	if err != nil {
		return db.Library{}, err
	}
	f.Audit(ctx, actor, "library.add", kind+":"+name, map[string]any{"sha256": sha, "size": size})
	// (the nodes whose servers play it sync it)
	f.PushAll(ctx)
	return f.Store.GetLibrary(ctx, sha)
}

// AddFolder adds a game data folder's maps, custom maps, gametypes and
// saved gametypes (`opence-fleet library add <folder>` on the control
// plane's machine): the items added, and why each other file was not.
func (f *Fleet) AddFolder(ctx context.Context, actor, dir string) ([]string, []string, error) {
	var added, skipped []string
	for _, sub := range []string{"maps", "custom_maps", "gametypes"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			continue
		}
		for _, e := range entries {
			path := filepath.Join(dir, sub, e.Name())
			kind, name, ok := library.Classify(path)
			if !ok || e.IsDir() {
				continue
			}
			if sha, _, err := library.Hash(path); err == nil {
				if item, err := f.Store.GetLibrary(ctx, sha); err == nil && item.Kind == kind && item.Name == name {
					continue
				}
			}
			if _, err := f.AddItem(ctx, actor, kind, name, path, true); err != nil {
				skipped = append(skipped, fmt.Sprintf("%s/%s: %v", sub, e.Name(), err))
				continue
			}
			added = append(added, sub+"/"+e.Name())
		}
	}
	if len(added) == 0 && len(skipped) == 0 {
		return nil, nil, Refuse(http.StatusBadRequest, "empty", "%s holds no maps/, custom_maps/ or gametypes/", dir)
	}
	return added, skipped, nil
}

// ItemUsers is the profiles whose rotations play an item.
func (f *Fleet) ItemUsers(ctx context.Context, item db.Library) []string {
	profiles, _ := f.Profiles(ctx)
	items, _ := f.Store.ListLibrary(ctx)
	var users []string
	for _, p := range profiles {
		rotation := p.Rotation
		if strings.TrimSpace(rotation) == "" {
			rotation = DefaultRotation
		}
		shas, gametypes, _ := needs(rotation, items)
		used := false
		for _, s := range shas {
			if s == item.Sha256 && item.Name != "ui" {
				used = true
			}
		}
		if _, ok := gametypes[item.Name]; ok && item.Kind == library.KindGametype {
			used = true
		}
		if used {
			users = append(users, p.Name)
		}
	}
	return users
}

// RemoveItem removes an item no rotation plays.
func (f *Fleet) RemoveItem(ctx context.Context, actor, sha string) error {
	item, err := f.Store.GetLibrary(ctx, sha)
	if err != nil {
		return NotFound("the item")
	}
	if users := f.ItemUsers(ctx, item); len(users) > 0 {
		return Refuse(http.StatusConflict, "in_use", "%s is played by %s: change their rotations first", item.Name, strings.Join(users, ", "))
	}
	if err := f.Store.DeleteLibrary(ctx, sha); err != nil {
		return err
	}
	f.Blobs().Remove(sha)
	f.Audit(ctx, actor, "library.remove", item.Kind+":"+item.Name, nil)
	return nil
}

// Missing is what the profiles' rotations need that the library lacks.
func (f *Fleet) Missing(ctx context.Context) map[string][]string {
	profiles, _ := f.Profiles(ctx)
	items, _ := f.Store.ListLibrary(ctx)
	have := map[string]bool{}
	for _, i := range items {
		have[i.Kind+":"+i.Name] = true
	}
	out := map[string][]string{}
	for _, p := range profiles {
		rotation := p.Rotation
		if strings.TrimSpace(rotation) == "" {
			rotation = DefaultRotation
		}
		var missing []string
		if !have["map:ui"] {
			missing = append(missing, "maps/ui.map")
		}
		for _, entry := range spec.RotationEntries(rotation) {
			m := strings.ToLower(spec.RotationMap(entry))
			switch {
			case strings.HasPrefix(m, `custom_maps\`):
				if !have["custom_map:"+m[len(`custom_maps\`):]] {
					missing = append(missing, "custom_maps/"+m[len(`custom_maps\`):]+".map")
				}
			case strings.Contains(m, `\`):
				name := m[strings.LastIndexByte(m, '\\')+1:]
				if !have["map:"+name] {
					missing = append(missing, "maps/"+name+".map")
				}
			default:
				if !have["map:"+m] && !have["custom_map:"+m] {
					missing = append(missing, "maps/"+m+".map")
				}
			}
		}
		if len(missing) > 0 {
			out[p.Name] = unique(missing)
		}
	}
	return out
}

func unique(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range list {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// StartUpload begins a resumable upload: its id. An upload of a blob the
// library has is done at once (the browser hashed it first).
func (f *Fleet) StartUpload(ctx context.Context, actor, kind, name string, size int64, sha string) (string, bool, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if err := library.CheckName(kind, name); err != nil {
		return "", false, Refuse(http.StatusBadRequest, "schema", "%v", err)
	}
	if size <= 0 || size > MaximumItem {
		return "", false, Refuse(http.StatusBadRequest, "schema", "an item is 1 byte to 4 GB")
	}
	sha = strings.ToLower(sha)
	if len(sha) != 64 {
		return "", false, Refuse(http.StatusBadRequest, "schema", "the upload's SHA-256 is 64 hexadecimal digits")
	}
	if f.Blobs().Has(sha) {
		if _, err := f.AddItem(ctx, actor, kind, name, f.Blobs().Path(sha), true); err != nil {
			return "", false, err
		}
		return "", true, nil
	}
	id := Token(12)
	if err := f.Store.CreateUpload(ctx, db.CreateUploadParams{ID: id, Kind: kind, Name: name, Size: size, Sha256: sha,
		CreatedBy: actor, CreatedAt: f.Now().Unix()}); err != nil {
		return "", false, err
	}
	os.MkdirAll(filepath.Dir(f.Blobs().UploadPath(id)), 0o700)
	return id, false, nil
}

// UploadChunk writes a chunk at its offset (which must be what was received
// so far): the bytes received.
func (f *Fleet) UploadChunk(ctx context.Context, id string, offset int64, chunk io.Reader) (int64, error) {
	u, err := f.Store.GetUpload(ctx, id)
	if err != nil {
		return 0, NotFound("the upload")
	}
	if offset != u.Received {
		return u.Received, Refuse(http.StatusConflict, "offset", "the upload has %d bytes: send from there", u.Received)
	}
	file, err := os.OpenFile(f.Blobs().UploadPath(id), os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.Copy(file, io.LimitReader(chunk, min(64<<20, u.Size-offset)+1))
	if err != nil {
		return offset, err
	}
	if offset+n > u.Size {
		return offset, Refuse(http.StatusRequestEntityTooLarge, "too_large", "more bytes than the upload's size")
	}
	if err := f.Store.UpdateUploadReceived(ctx, db.UpdateUploadReceivedParams{Received: offset + n, ID: id}); err != nil {
		return 0, err
	}
	return offset + n, nil
}

// FinishUpload checks an upload's hash and keeps it as an item.
func (f *Fleet) FinishUpload(ctx context.Context, actor, id string) (db.Library, error) {
	u, err := f.Store.GetUpload(ctx, id)
	if err != nil {
		return db.Library{}, NotFound("the upload")
	}
	if u.Received != u.Size {
		return db.Library{}, Refuse(http.StatusConflict, "incomplete", "the upload has %d of %d bytes", u.Received, u.Size)
	}
	path := f.Blobs().UploadPath(id)
	sha, _, err := library.Hash(path)
	if err != nil {
		return db.Library{}, err
	}
	f.Store.DeleteUpload(ctx, id)
	if sha != u.Sha256 {
		os.Remove(path)
		return db.Library{}, Refuse(http.StatusBadRequest, "hash", "the upload's SHA-256 is not the one announced: refused")
	}
	item, err := f.AddItem(ctx, actor, u.Kind, u.Name, path, false)
	if err != nil {
		os.Remove(path)
	}
	return item, err
}
