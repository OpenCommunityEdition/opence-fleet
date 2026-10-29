package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenCommunityEdition/opence-fleet/internal/releases"
)

// A release's network version: its image's label, else its notes' line.
func TestLearnNetworkVersion(t *testing.T) {
	config := map[string]string{
		"sha256:" + strings.Repeat("c", 64): `{"config":{"Labels":{"org.opence.network-version":"39"}}}`,
		"sha256:" + strings.Repeat("d", 64): `{"config":{"Labels":{}}}`,
	}
	digests := map[string]string{"build-901": strings.Repeat("a", 64), "build-902": strings.Repeat("b", 64)}
	configs := map[string]string{strings.Repeat("a", 64): "sha256:" + strings.Repeat("c", 64),
		strings.Repeat("b", 64): "sha256:" + strings.Repeat("d", 64)}
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v2/x/y/")
		switch {
		case strings.HasPrefix(path, "manifests/sha256:"):
			fmt.Fprintf(w, `{"config":{"digest":%q}}`, configs[strings.TrimPrefix(path, "manifests/sha256:")])
		case strings.HasPrefix(path, "manifests/"):
			w.Header().Set("Docker-Content-Digest", "sha256:"+digests[strings.TrimPrefix(path, "manifests/")])
		case strings.HasPrefix(path, "blobs/"):
			io.WriteString(w, config[strings.TrimPrefix(path, "blobs/")])
		default:
			http.NotFound(w, r)
		}
	}))
	defer registry.Close()
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"body": "Commit: abc\nBuild: 902\nNetwork version: 40\n"})
	}))
	defer github.Close()
	defer func(api string) { releases.GitHubAPI = api }(releases.GitHubAPI)
	releases.GitHubAPI = github.URL
	f, _ := fleet(t)
	f.Config.Image = strings.TrimPrefix(registry.URL, "http://") + "/x/y"
	f.Config.ReleaseRepository = "x/y"
	ctx := context.Background()
	for build, want := range map[string]string{"build-901": "39 label", "build-902": "40 notes"} {
		if err := f.FetchRelease(ctx, "user:paul", build); err != nil {
			t.Fatal(err)
		}
		r, _ := f.Store.GetRelease(ctx, build)
		if got := fmt.Sprint(r.NetworkVersion, " ", r.NetworkSource); got != want {
			t.Errorf("%s: %s, want %s", build, got, want)
		}
	}
}
