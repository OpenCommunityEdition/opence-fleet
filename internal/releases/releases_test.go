package releases

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// registry is a fake of a registry's distribution API behind ghcr.io's
// anonymous pull tokens: a 401 with its challenge, then the token.
type registry struct {
	repository string
	// tag → digest
	manifests map[string]string
	// the tags list's pages (the Link header to the next)
	pages [][]string
	// the token's field: token or access_token
	field string
	// the tags list answers 500
	brokenTags bool
	// the registry refuses every token
	refuse bool

	mu       sync.Mutex
	tokens   int
	accepted []string
	server   *httptest.Server
}

const token = "anonymous-pull-token"

func (g *registry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		g.mu.Lock()
		g.tokens++
		g.mu.Unlock()
		if r.URL.Query().Get("service") != "fake.registry" || r.URL.Query().Get("scope") != "repository:"+g.repository+":pull" {
			http.Error(w, "a wrong service or scope: "+r.URL.RawQuery, http.StatusBadRequest)
			return
		}
		field := g.field
		if field == "" {
			field = "token"
		}
		json.NewEncoder(w).Encode(map[string]string{field: token})
		return
	}
	if g.refuse || r.Header.Get("Authorization") != "Bearer "+token {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake.registry",scope="repository:%s:pull"`,
			g.server.URL, g.repository))
		http.Error(w, `{"errors":[{"code":"UNAUTHORIZED"}]}`, http.StatusUnauthorized)
		return
	}
	prefix := "/v2/" + g.repository + "/"
	switch {
	case r.URL.Path == prefix+"tags/list":
		if g.brokenTags {
			http.Error(w, "broken", http.StatusInternalServerError)
			return
		}
		page := 0
		fmt.Sscan(r.URL.Query().Get("page"), &page)
		if page+1 < len(g.pages) {
			w.Header().Set("Link", fmt.Sprintf(`<%stags/list?n=1000&page=%d>; rel="next"`, prefix, page+1))
		}
		var tags []string
		if page < len(g.pages) {
			tags = g.pages[page]
		}
		json.NewEncoder(w).Encode(map[string]any{"name": g.repository, "tags": tags})
	case strings.HasPrefix(r.URL.Path, prefix+"manifests/"):
		if r.Method != http.MethodHead {
			http.Error(w, "HEAD only", http.StatusMethodNotAllowed)
			return
		}
		g.mu.Lock()
		g.accepted = append(g.accepted, r.Header.Get("Accept"))
		g.mu.Unlock()
		digest, ok := g.manifests[strings.TrimPrefix(r.URL.Path, prefix+"manifests/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if digest != "" {
			w.Header().Set("Docker-Content-Digest", digest)
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
	default:
		http.NotFound(w, r)
	}
}

func fake(t *testing.T, g *registry) *Registry {
	t.Helper()
	if g.repository == "" {
		g.repository = "opencommunityedition/opence-server"
	}
	g.server = httptest.NewServer(g)
	t.Cleanup(g.server.Close)
	host := strings.TrimPrefix(g.server.URL, "http://")
	return &Registry{Image: host + "/" + g.repository, HTTP: g.server.Client()}
}

func (g *registry) tokensAsked() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tokens
}

func sha(n int) string { return "sha256:" + strings.Repeat(fmt.Sprint(n), 64)[:64] }

func TestDigest(t *testing.T) {
	g := &registry{manifests: map[string]string{"latest": sha(1), "build-7": sha(2), "nodigest": ""}}
	r := fake(t, g)
	ctx := context.Background()
	digest, err := r.Digest(ctx, "latest")
	if err != nil || digest != sha(1) {
		t.Fatalf("Digest(latest): %q, %v", digest, err)
	}
	if digest, err := r.Digest(ctx, "build-7"); err != nil || digest != sha(2) {
		t.Errorf("Digest(build-7): %q, %v", digest, err)
	}
	// (the token is asked once, then kept)
	if n := g.tokensAsked(); n != 1 {
		t.Errorf("%d tokens asked, want 1", n)
	}
	for _, accept := range g.accepted {
		if !strings.HasPrefix(accept, "application/vnd.oci.image.index.v1+json") {
			t.Errorf("Accept %q: not an index first", accept)
		}
	}
	if _, err := r.Digest(ctx, "build-404"); err == nil || !strings.Contains(err.Error(), "not in the registry") {
		t.Errorf("an unknown tag: %v", err)
	}
	if _, err := r.Digest(ctx, "nodigest"); err == nil || !strings.Contains(err.Error(), "no digest") {
		t.Errorf("no digest: %v", err)
	}
}

func TestAccessToken(t *testing.T) {
	g := &registry{manifests: map[string]string{"latest": sha(1)}, field: "access_token"}
	r := fake(t, g)
	if digest, err := r.Digest(context.Background(), "latest"); err != nil || digest != sha(1) {
		t.Errorf("Digest with an access_token: %q, %v", digest, err)
	}
}

func TestTokenRefused(t *testing.T) {
	g := &registry{manifests: map[string]string{"latest": sha(1)}, refuse: true}
	r := fake(t, g)
	_, err := r.Digest(context.Background(), "latest")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a refused token: %v", err)
	}
	if n := g.tokensAsked(); n != 1 {
		t.Errorf("%d tokens asked, want 1 (no loop)", n)
	}
}

func TestChallenges(t *testing.T) {
	tests := []struct {
		name, challenge, want string
	}{
		{"basic", `Basic realm="registry"`, "not a bearer token"},
		{"no realm", `Bearer service="x"`, "no realm"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("WWW-Authenticate", tt.challenge)
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer server.Close()
			r := &Registry{Image: strings.TrimPrefix(server.URL, "http://") + "/x/y", HTTP: server.Client()}
			if _, err := r.Digest(context.Background(), "latest"); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("got %v, want an error with %q", err, tt.want)
			}
		})
	}
}

func TestTags(t *testing.T) {
	g := &registry{pages: [][]string{{"build-1", "build-2"}, {"build-10"}, {"latest", "foo"}}}
	r := fake(t, g)
	tags, err := r.Tags(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tags, ","); got != "build-1,build-2,build-10,latest,foo" {
		t.Errorf("tags %s", got)
	}
	g.brokenTags = true
	if _, err := r.Tags(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("a broken tags list: %v", err)
	}
}

func TestLatest(t *testing.T) {
	manifests := map[string]string{"latest": sha(2), "build-1": sha(1), "build-2": sha(2), "build-10": sha(3), "foo": sha(2)}
	g := &registry{manifests: manifests, pages: [][]string{{"foo", "build-1", "build-2", "build-10", "latest"}}}
	r := fake(t, g)
	ctx := context.Background()
	digest, build, err := r.Latest(ctx)
	if err != nil || digest != sha(2) || build != "build-2" {
		t.Errorf("Latest: %q, %q, %v", digest, build, err)
	}

	// latest moved past every build tag: its digest, no build
	manifests["latest"] = sha(4)
	if digest, build, err := r.Latest(ctx); err != nil || digest != sha(4) || build != "" {
		t.Errorf("Latest with no build: %q, %q, %v", digest, build, err)
	}

	// the tags list broken: the digest still
	g.brokenTags = true
	if digest, build, err := r.Latest(ctx); err != nil || digest != sha(4) || build != "" {
		t.Errorf("Latest without tags: %q, %q, %v", digest, build, err)
	}

	delete(manifests, "latest")
	if _, _, err := r.Latest(ctx); err == nil {
		t.Error("Latest without latest: no error")
	}
}

func TestLatestNewestFirst(t *testing.T) {
	// 30 builds, all of latest's digest: the newest is named, and it is
	// asked first
	manifests := map[string]string{"latest": sha(9)}
	var tags []string
	for i := 1; i <= 30; i++ {
		tag := fmt.Sprintf("build-%d", i)
		manifests[tag] = sha(9)
		tags = append(tags, tag)
	}
	g := &registry{manifests: manifests, pages: [][]string{tags}}
	r := fake(t, g)
	if _, build, err := r.Latest(context.Background()); err != nil || build != "build-30" {
		t.Errorf("Latest: %q, %v", build, err)
	}
	// latest, then build-30 alone
	if n := len(g.accepted); n != 2 {
		t.Errorf("%d manifests asked, want 2", n)
	}
}

func TestSplit(t *testing.T) {
	tests := []struct {
		image, host, repository string
	}{
		{"ghcr.io/opencommunityedition/opence-server", "ghcr.io", "opencommunityedition/opence-server"},
		{"127.0.0.1:5000/opence-server", "127.0.0.1:5000", "opence-server"},
		{"localhost:5000/a/b", "localhost:5000", "a/b"},
		{"ubuntu", "registry-1.docker.io", "library/ubuntu"},
		{"someone/server", "registry-1.docker.io", "someone/server"},
	}
	for _, tt := range tests {
		host, repository, err := (&Registry{Image: tt.image}).split()
		if err != nil || host != tt.host || repository != tt.repository {
			t.Errorf("split(%q) = %q, %q, %v", tt.image, host, repository, err)
		}
	}
}

func TestScheme(t *testing.T) {
	tests := []struct{ host, scheme string }{
		{"ghcr.io", "https://"},
		{"registry-1.docker.io", "https://"},
		{"localhost:5000", "http://"},
		{"127.0.0.1:5000", "http://"},
		{"[::1]:5000", "http://"},
		{"10.0.0.1:5000", "https://"},
		{"localhost.example.com", "https://"},
	}
	for _, tt := range tests {
		if got := (&Registry{}).scheme(tt.host); got != tt.scheme {
			t.Errorf("scheme(%q) = %q", tt.host, got)
		}
	}
}

func TestBuildNumber(t *testing.T) {
	tests := []struct {
		tag string
		n   int
	}{
		{"build-1", 1}, {"build-2276", 2276}, {"latest", 0}, {"build-", 0}, {"build-1a", 0}, {"v1.0", 0}, {"xbuild-3", 0},
	}
	for _, tt := range tests {
		if got := BuildNumber(tt.tag); got != tt.n {
			t.Errorf("BuildNumber(%q) = %d", tt.tag, got)
		}
	}
}

func TestURLs(t *testing.T) {
	if got := ZipURL("OpenCommunityEdition/OpenCE", "build-42"); got != "https://github.com/OpenCommunityEdition/OpenCE/releases/download/build-42/halo-linux-release.zip" {
		t.Errorf("ZipURL: %s", got)
	}
	if got := NotesURL("OpenCommunityEdition/OpenCE", "build-42"); got != "https://github.com/OpenCommunityEdition/OpenCE/releases/tag/build-42" {
		t.Errorf("NotesURL: %s", got)
	}
	// (a tag is one path segment)
	if got := ZipURL("a/b", "x/../y"); !strings.Contains(got, "/download/x%2F..%2Fy/") {
		t.Errorf("ZipURL escapes no tag: %s", got)
	}
}

func TestDownload(t *testing.T) {
	zip := []byte(strings.Repeat("PK the release's bytes ", 1000))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/OpenCommunityEdition/OpenCE/releases/download/build-42/halo-linux-release.zip" {
			http.NotFound(w, r)
			return
		}
		w.Write(zip)
	}))
	defer server.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "halo-linux-release.zip")
	source := server.URL + strings.TrimPrefix(ZipURL("OpenCommunityEdition/OpenCE", "build-42"), "https://github.com")
	sum, size, err := Download(context.Background(), server.Client(), source, path)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(zip)
	if sum != hex.EncodeToString(want[:]) || size != int64(len(zip)) {
		t.Errorf("Download: %s, %d", sum, size)
	}
	if got, _ := os.ReadFile(path); string(got) != string(zip) {
		t.Error("the file's bytes are not the zip's")
	}
	if _, err := os.Stat(path + ".part"); !os.IsNotExist(err) {
		t.Errorf("a .part is left: %v", err)
	}

	// a 404: no file, no .part
	missing := filepath.Join(dir, "missing.zip")
	if _, _, err := Download(context.Background(), server.Client(), server.URL+"/nowhere", missing); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a 404: %v", err)
	}
	for _, p := range []string{missing, missing + ".part"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s is there: %v", p, err)
		}
	}

	// a cancelled context: an error
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Download(ctx, server.Client(), source, filepath.Join(dir, "cancelled.zip")); err == nil {
		t.Error("a cancelled download: no error")
	}
}
