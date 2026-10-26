package web

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
	"github.com/OpenCommunityEdition/opence-fleet/internal/library"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view/pages"
)

func (s *Server) libraryPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	items, _ := s.Fleet.Library(ctx)
	var out []pages.LibraryItem
	for _, it := range items {
		meta := map[string]string{}
		var raw map[string]any
		json.Unmarshal([]byte(it.Meta), &raw)
		for k, v := range raw {
			if k != "text" {
				if text, ok := v.(string); ok {
					meta[k] = text
				}
			}
		}
		out = append(out, pages.LibraryItem{Library: it, Meta: meta, Users: s.Fleet.ItemUsers(ctx, it)})
	}
	s.render(w, r, http.StatusOK, pages.Library(s.page(r, "Library", "library"), out, s.Fleet.Missing(ctx)))
}

// uploadItem is the form's upload without JavaScript (one request; the
// resumable one is /library/uploads).
func (s *Server) uploadItem(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, core.MaximumItem+1<<20)
	file, header, err := r.FormFile("file")
	if err != nil {
		s.fail(w, r, "/library", core.Refuse(http.StatusBadRequest, "schema", "choose a file"))
		return
	}
	defer file.Close()
	kind, name := r.FormValue("kind"), strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(filepath.Base(header.Filename), ".map"), ".toml"), ".gametype")
	}
	dir := filepath.Join(s.Fleet.Config.Data, "library", "uploads")
	os.MkdirAll(dir, 0o700)
	temp, err := os.CreateTemp(dir, "form-*.part")
	if err != nil {
		s.fail(w, r, "/library", err)
		return
	}
	_, err = io.Copy(temp, file)
	temp.Close()
	if err != nil {
		os.Remove(temp.Name())
		s.fail(w, r, "/library", err)
		return
	}
	item, err := s.Fleet.AddItem(r.Context(), s.actor(r), kind, name, temp.Name(), false)
	if err != nil {
		os.Remove(temp.Name())
		s.fail(w, r, "/library", err)
		return
	}
	s.done(w, r, "/library", item.Name+" is in the library ("+view.Bytes(item.Size)+").")
}

// startUpload begins a resumable upload: {kind, name, size, sha256} →
// {id, done}.
func (s *Server) startUpload(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind   string `json:"kind"`
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	if err := readJSON(r, &in); err != nil {
		s.fail(w, r, "", err)
		return
	}
	id, done, err := s.Fleet.StartUpload(r.Context(), s.actor(r), in.Kind, in.Name, in.Size, in.SHA256)
	if err != nil {
		s.fail(w, r, "", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id, "done": done})
}

// uploadChunk writes a chunk (the body) at its offset (?offset=).
func (s *Server) uploadChunk(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	received, err := s.Fleet.UploadChunk(r.Context(), r.PathValue("id"), offset, http.MaxBytesReader(w, r.Body, 65<<20))
	if err != nil {
		if e, ok := core.AsError(err); ok && e.Code == "offset" {
			writeJSONStatus(w, http.StatusConflict, map[string]any{"ok": false, "received": received,
				"error": map[string]string{"code": "offset", "message": e.Message}})
			return
		}
		s.fail(w, r, "", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "received": received})
}

func (s *Server) finishUpload(w http.ResponseWriter, r *http.Request) {
	item, err := s.Fleet.FinishUpload(r.Context(), s.actor(r), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, "", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "name": item.Name, "sha256": item.Sha256, "size": item.Size})
}

func (s *Server) addFolder(w http.ResponseWriter, r *http.Request) {
	added, skipped, err := s.Fleet.AddFolder(r.Context(), s.actor(r), strings.TrimSpace(r.FormValue("path")))
	if err != nil {
		s.fail(w, r, "/library", err)
		return
	}
	notice := "Added " + view.Count(len(added), "item", "items") + "."
	if len(skipped) > 0 {
		notice += " Left out: " + strings.Join(skipped, "; ")
	}
	s.done(w, r, "/library", notice)
}

func (s *Server) removeItem(w http.ResponseWriter, r *http.Request) {
	if err := s.Fleet.RemoveItem(r.Context(), s.actor(r), r.PathValue("sha")); err != nil {
		s.fail(w, r, "/library", err)
		return
	}
	s.done(w, r, "/library", "Removed from the library (the nodes keep their copies).")
}

func (s *Server) releasesPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	releases, _ := s.Fleet.Releases(ctx)
	d := s.data(ctx)
	running := map[string][]string{}
	behind := 0
	for _, i := range d.instances {
		if l := s.Fleet.Live.Instance(i.Slug); l != nil && l.Build != "" {
			running[l.Build] = append(running[l.Build], i.Slug)
			if d.latest != "" && l.Build != d.latest && (i.Channel == "" || i.Channel == "latest") {
				behind++
			}
		}
	}
	var lines []pages.ReleaseLine
	for _, rel := range releases {
		lines = append(lines, pages.ReleaseLine{Release: rel, Running: running[rel.Build]})
	}
	s.render(w, r, http.StatusOK, pages.Releases(s.page(r, "Releases", "releases"), lines, s.Fleet.Config.Image, d.latest, behind))
}

func (s *Server) checkLatest(w http.ResponseWriter, r *http.Request) {
	release, moved, err := s.Fleet.CheckLatest(r.Context(), s.actor(r))
	if err != nil {
		s.fail(w, r, "/releases", err)
		return
	}
	notice := "latest is " + release.Build + "."
	if moved {
		notice = "latest moved to " + release.Build + "."
	}
	s.done(w, r, "/releases", notice)
}

func (s *Server) fetchRelease(w http.ResponseWriter, r *http.Request) {
	tag := r.FormValue("tag")
	if err := s.Fleet.FetchRelease(r.Context(), s.actor(r), tag); err != nil {
		s.fail(w, r, "/releases", err)
		return
	}
	s.done(w, r, "/releases", tag+" is known.")
}

func (s *Server) localRelease(w http.ResponseWriter, r *http.Request) {
	build := strings.TrimSpace(r.FormValue("build"))
	if err := s.Fleet.AddLocalRelease(r.Context(), s.actor(r), build, r.FormValue("image"), r.FormValue("latest") == "1"); err != nil {
		s.fail(w, r, "/releases", err)
		return
	}
	s.done(w, r, "/releases", build+" is added.")
}

func (s *Server) fetchZip(w http.ResponseWriter, r *http.Request) {
	build := r.PathValue("build")
	if err := s.Fleet.FetchZip(r.Context(), s.actor(r), build); err != nil {
		s.fail(w, r, "/releases", err)
		return
	}
	s.done(w, r, "/releases", build+"'s zip is fetched: nodes without Docker can run it.")
}

func (s *Server) setLatest(w http.ResponseWriter, r *http.Request) {
	build := r.PathValue("build")
	if err := s.Fleet.SetLatest(r.Context(), s.actor(r), build); err != nil {
		s.fail(w, r, "/releases", err)
		return
	}
	s.done(w, r, "/releases", build+" is latest: the servers that follow latest run it at their next restart (or an update now).")
}

func (s *Server) forgetRelease(w http.ResponseWriter, r *http.Request) {
	build := r.PathValue("build")
	if err := s.Fleet.ForgetRelease(r.Context(), s.actor(r), build); err != nil {
		s.fail(w, r, "/releases", err)
		return
	}
	s.done(w, r, "/releases", build+" is forgotten.")
}

// updateServers starts the rolling update of the servers behind latest.
func (s *Server) updateServers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := s.data(ctx)
	var slugs []string
	for _, i := range d.instances {
		if l := s.Fleet.Live.Instance(i.Slug); l != nil && l.Build != "" && l.Build != d.latest &&
			(i.Channel == "" || i.Channel == "latest") {
			slugs = append(slugs, i.Slug)
		}
	}
	id, err := s.Fleet.StartJob(ctx, s.actor(r), core.JobParams{Action: core.ActionUpdate, Slugs: slugs, Channel: "latest",
		After: r.FormValue("after"), Notice: "This server restarts for an update after this game."})
	if err != nil {
		s.fail(w, r, "/releases", err)
		return
	}
	s.done(w, r, "/jobs/"+strconv.FormatInt(id, 10), "The update runs.")
}

func (s *Server) mastersPage(w http.ResponseWriter, r *http.Request) {
	s.renderMasters(w, r, "", "", "")
}

func (s *Server) renderMasters(w http.ResponseWriter, r *http.Request, signed, nonce, proof string) {
	ctx := r.Context()
	keys, _ := s.Fleet.Keys(ctx)
	var views []pages.KeyView
	for _, k := range keys {
		n, _ := s.Fleet.Store.CountKeyInstances(ctx, &k.KeyID)
		if k.IsDefault != 0 {
			all, _ := s.Fleet.Store.CountInstances(ctx)
			n = all
		}
		v := pages.KeyView{PublisherKey: k, Answers: s.Fleet.MeAnswers(k), Masters: s.Fleet.Masters, Servers: int(n),
			Cached: view.UnixPtr(k.MeCachedAt, s.Fleet.Location())}
		if k.KeyID == signed {
			v.Nonce, v.Proof = nonce, proof
		}
		views = append(views, v)
	}
	s.render(w, r, http.StatusOK, pages.Masters(s.page(r, "Masters", "masters"), views, s.Fleet.Masters))
}

func (s *Server) makeKey(w http.ResponseWriter, r *http.Request) {
	id, err := s.Fleet.MakeKey(r.Context(), s.actor(r), r.FormValue("label"))
	if err != nil {
		s.fail(w, r, "/masters", err)
		return
	}
	s.done(w, r, "/masters", "Key "+id+" is made. Apply with it on the master's Discord (the steps are below).")
}

func (s *Server) importKey(w http.ResponseWriter, r *http.Request) {
	id, err := s.Fleet.ImportKey(r.Context(), s.actor(r), r.FormValue("label"), r.FormValue("seed"))
	if err != nil {
		s.fail(w, r, "/masters", err)
		return
	}
	s.done(w, r, "/masters", "Key "+id+" is imported.")
}

func (s *Server) defaultKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Fleet.SetDefaultKey(r.Context(), s.actor(r), id); err != nil {
		s.fail(w, r, "/masters", err)
		return
	}
	s.done(w, r, "/masters", id+" is the default key: the servers take it at their next restart.")
}

func (s *Server) forgetKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Fleet.ForgetKey(r.Context(), s.actor(r), id); err != nil {
		s.fail(w, r, "/masters", err)
		return
	}
	s.done(w, r, "/masters", id+" is forgotten.")
}

func (s *Server) signNonce(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	nonce := strings.TrimSpace(r.FormValue("nonce"))
	proof, err := s.Fleet.SignNonce(r.Context(), s.actor(r), id, nonce)
	if err != nil {
		s.fail(w, r, "/masters", err)
		return
	}
	if wantsJSON(r) {
		writeJSON(w, map[string]any{"ok": true, "proof": proof})
		return
	}
	s.renderMasters(w, r, id, nonce, proof)
}

func (s *Server) refreshMe(w http.ResponseWriter, r *http.Request) {
	if err := s.Fleet.RefreshMe(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, "/masters", err)
		return
	}
	s.done(w, r, "/masters", "The masters were asked.")
}

func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	data, _ := json.Marshal(value)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(append(data, '\n'))
}

// keep library's import for the folder classification in handlers
var _ = library.Classify
