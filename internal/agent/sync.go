package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agent/runtime"
	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

// unready is why an instance cannot start yet ("" when it can): its build
// is not on the node, or a library item its rotation needs is missing.
func (a *Agent) unready(i spec.Instance) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if why, ok := a.builds[i.Build]; !ok {
		return "waiting for " + i.Build + " to be fetched"
	} else if why != "" {
		return why
	}
	items := map[string]spec.LibraryItem{}
	for _, item := range a.state.Spec.Library {
		items[item.SHA256] = item
	}
	var missing []string
	for _, need := range i.Needs {
		item, ok := items[need]
		if !ok {
			continue
		}
		if !a.haveItem(item) {
			missing = append(missing, item.Path())
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		if a.Config.DataRoot != "" {
			return "missing from " + a.Config.DataRoot + ": " + strings.Join(missing, ", ")
		}
		return "waiting for " + strings.Join(missing, ", ")
	}
	return ""
}

// haveItem tells whether a library item is in the data root (the synced
// one: by its hash, as written; an external one: by its name).
func (a *Agent) haveItem(item spec.LibraryItem) bool {
	if a.Config.DataRoot != "" {
		_, err := os.Stat(filepath.Join(a.dataDir(), item.Path()))
		return err == nil
	}
	return a.state.Library[item.Path()] == item.SHA256
}

// syncLoop fetches what the spec's instances need, whenever the spec
// changes and every 10 minutes (a failure is tried again).
func (a *Agent) syncLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		a.syncOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-a.wakeSync:
		case <-ticker.C:
		}
	}
}

func (a *Agent) syncOnce(ctx context.Context) {
	a.mu.Lock()
	s := a.state.Spec
	a.mu.Unlock()
	// (every build the spec names: its instances', and those an update has
	// the node fetch before its servers get them)
	builds := map[string]bool{}
	for _, i := range s.Instances {
		builds[i.Build] = true
	}
	images := map[string]spec.Image{}
	for _, image := range s.Images {
		images[image.Build] = image
		if a.Runtime.Name() == "docker" {
			builds[image.Build] = true
		}
	}
	releases := map[string]spec.Release{}
	for _, release := range s.Releases {
		releases[release.Build] = release
		if a.Runtime.Name() == "process" {
			builds[release.Build] = true
		}
	}
	for build := range builds {
		if a.Runtime.Ready(ctx, build) {
			a.setBuild(build, "")
			continue
		}
		image, release := images[build], releases[build]
		if a.Runtime.Name() == "docker" && image.Ref == "" {
			a.setBuild(build, "the control plane knows no image of "+build)
			continue
		}
		a.setBuild(build, "fetching "+build)
		a.Log.Info("fetching a build", "build", build, "image", image.Pull())
		last := time.Time{}
		err := a.Runtime.Fetch(ctx, image, release, func(done, total int64) {
			if time.Since(last) > 2*time.Second && a.isOnline() {
				last = time.Now()
				a.send(agentproto.TSync, agentproto.Sync{Kind: "release", Name: build, Done: done, Total: total})
			}
		})
		if err != nil {
			a.Log.Error("a build was not fetched", "build", build, "err", err)
			a.setBuild(build, "the build was not fetched: "+err.Error())
			a.send(agentproto.TSync, agentproto.Sync{Kind: "release", Name: build, Error: err.Error()})
			continue
		}
		a.setBuild(build, "")
		a.send(agentproto.TSync, agentproto.Sync{Kind: "release", Name: build, Done: 1, Total: 1})
	}
	a.syncLibrary(ctx, s)
	a.mu.Lock()
	for _, sup := range a.supervisors {
		sup.poke()
	}
	a.mu.Unlock()
}

func (a *Agent) setBuild(build, why string) {
	a.mu.Lock()
	a.builds[build] = why
	a.mu.Unlock()
}

// syncLibrary fetches the library items the node's instances need (or all,
// with the node's sync_all), by hash, into data/.
func (a *Agent) syncLibrary(ctx context.Context, s spec.Spec) {
	needed := map[string]bool{}
	for _, i := range s.Instances {
		for _, n := range i.Needs {
			needed[n] = true
		}
	}
	var missing []string
	for _, item := range s.Library {
		if !needed[item.SHA256] && !s.Node.SyncAll {
			continue
		}
		a.mu.Lock()
		have := a.haveItem(item)
		a.mu.Unlock()
		if have {
			continue
		}
		if a.Config.DataRoot != "" {
			missing = append(missing, item.Path())
			continue
		}
		path := filepath.Join(a.dataDir(), item.Path())
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			missing = append(missing, item.Path())
			continue
		}
		last := time.Time{}
		err := a.download(ctx, "/agent/blobs/"+item.SHA256, path, item.SHA256, func(done, total int64) {
			if time.Since(last) > 2*time.Second && a.isOnline() {
				last = time.Now()
				a.send(agentproto.TSync, agentproto.Sync{Kind: "library", Name: item.Path(), Done: done, Total: total})
			}
		})
		if err != nil {
			a.Log.Error("a library item was not fetched", "item", item.Path(), "err", err)
			a.send(agentproto.TSync, agentproto.Sync{Kind: "library", Name: item.Path(), Error: err.Error()})
			missing = append(missing, item.Path())
			continue
		}
		os.Chmod(path, 0o644)
		a.mu.Lock()
		a.state.Library[item.Path()] = item.SHA256
		a.mu.Unlock()
		a.saveState()
		a.send(agentproto.TSync, agentproto.Sync{Kind: "library", Name: item.Path(), Done: item.Size, Total: item.Size})
	}
	sort.Strings(missing)
	a.mu.Lock()
	a.missing, a.synced = missing, len(missing) == 0
	a.mu.Unlock()
}

// Download is download, for the process runtime's releases.
func (a *Agent) Download(ctx context.Context, url, path, want string, progress runtime.Progress) error {
	return a.download(ctx, url, path, want, progress)
}

// download fetches a file of the control plane (bearer the node token) into
// path: resumed from what a .part holds (Range), checked against its
// SHA-256, then renamed into place.
func (a *Agent) download(ctx context.Context, url, path, want string, progress runtime.Progress) error {
	if strings.HasPrefix(url, "/") {
		url = a.Config.URL + url
	}
	part := path + ".part"
	var offset int64
	if info, err := os.Stat(part); err == nil {
		offset = info.Size()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+a.Config.Token)
	if offset > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	response, err := a.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	flags := os.O_WRONLY | os.O_CREATE
	switch response.StatusCode {
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	case http.StatusOK:
		flags |= os.O_TRUNC
		offset = 0
	case http.StatusRequestedRangeNotSatisfiable:
		os.Remove(part)
		return errors.New("the partial download was past the file's end: started again next time")
	default:
		return fmt.Errorf("%s answered %s", url, response.Status)
	}
	file, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	total := offset + response.ContentLength
	done := offset
	buffer := make([]byte, 256<<10)
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			if _, err := file.Write(buffer[:n]); err != nil {
				file.Close()
				return err
			}
			done += int64(n)
			if progress != nil {
				progress(done, total)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			file.Close()
			return readErr
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	got, err := fileHash(part)
	if err != nil {
		return err
	}
	if got != want {
		os.Remove(part)
		return fmt.Errorf("the download's SHA-256 is %s, not %s: refused", got, want)
	}
	return os.Rename(part, path)
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
