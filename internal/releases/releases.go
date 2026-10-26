// Package releases finds the game's releases: the server image's tags and
// the digests they point at, through a registry's distribution API
// (ghcr.io's anonymous pull tokens included), so that every node runs the
// same bytes however the tags move; and, for the process runtime, the
// release's halo-linux-release.zip on GitHub, downloaded once and pinned by
// its SHA-256.
package releases

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Registry is a client of an image's registry.
type Registry struct {
	// the image: ghcr.io/opencommunityedition/opence-server
	Image string
	HTTP  *http.Client

	mu    sync.Mutex
	token string
}

// Accept is the manifests the client takes (an index first: the digest a
// node pulls is the index's).
var Accept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// split is the registry's host and the repository's path.
func (r *Registry) split() (string, string, error) {
	host, repository, ok := strings.Cut(r.Image, "/")
	if !ok || !strings.ContainsAny(host, ".:") && host != "localhost" {
		// (Docker Hub's short names)
		if !ok {
			repository = "library/" + host
		} else {
			repository = r.Image
		}
		host = "registry-1.docker.io"
	}
	return host, repository, nil
}

func (r *Registry) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return http.DefaultClient
}

var challengeParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// authenticate asks the registry's token service for an anonymous pull
// token, from a 401's challenge.
func (r *Registry) authenticate(ctx context.Context, challenge string) error {
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		return fmt.Errorf("the registry asks %q, not a bearer token", challenge)
	}
	params := map[string]string{}
	for _, m := range challengeParam.FindAllStringSubmatch(challenge, -1) {
		params[m[1]] = m[2]
	}
	realm := params["realm"]
	if realm == "" {
		return errors.New("the registry's challenge names no realm")
	}
	query := url.Values{}
	if params["service"] != "" {
		query.Set("service", params["service"])
	}
	if params["scope"] != "" {
		query.Set("scope", params["scope"])
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	response, err := r.client().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var answer struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&answer); err != nil {
		return err
	}
	r.mu.Lock()
	r.token = answer.Token
	if r.token == "" {
		r.token = answer.AccessToken
	}
	r.mu.Unlock()
	return nil
}

// Scheme is the registry's scheme: https, or http for a registry on
// loopback (a test's, or a mirror on the machine).
func (r *Registry) scheme(host string) string {
	h := host
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	if h == "localhost" || h == "127.0.0.1" || h == "[::1]" {
		return "http://"
	}
	return "https://"
}

func (r *Registry) do(ctx context.Context, method, path, accept string) (*http.Response, error) {
	host, _, _ := r.split()
	for attempt := 0; attempt < 2; attempt++ {
		request, err := http.NewRequestWithContext(ctx, method, r.scheme(host)+host+path, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			request.Header.Set("Accept", accept)
		}
		r.mu.Lock()
		if r.token != "" {
			request.Header.Set("Authorization", "Bearer "+r.token)
		}
		r.mu.Unlock()
		response, err := r.client().Do(request)
		if err != nil {
			return nil, err
		}
		if response.StatusCode == http.StatusUnauthorized && attempt == 0 {
			challenge := response.Header.Get("WWW-Authenticate")
			response.Body.Close()
			if err := r.authenticate(ctx, challenge); err != nil {
				return nil, err
			}
			continue
		}
		return response, nil
	}
	return nil, errors.New("the registry refused its own token")
}

// Digest is the digest a tag points at.
func (r *Registry) Digest(ctx context.Context, tag string) (string, error) {
	_, repository, _ := r.split()
	response, err := r.do(ctx, http.MethodHead, "/v2/"+repository+"/manifests/"+url.PathEscape(tag), Accept)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%s:%s is not in the registry", r.Image, tag)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the registry answered %s for %s:%s", response.Status, r.Image, tag)
	}
	digest := response.Header.Get("Docker-Content-Digest")
	if !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("the registry gave no digest for %s:%s", r.Image, tag)
	}
	return digest, nil
}

// Tags lists the image's tags.
func (r *Registry) Tags(ctx context.Context) ([]string, error) {
	_, repository, _ := r.split()
	var tags []string
	path := "/v2/" + repository + "/tags/list?n=1000"
	for page := 0; page < 20 && path != ""; page++ {
		response, err := r.do(ctx, http.MethodGet, path, "application/json")
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return nil, fmt.Errorf("the registry answered %s for %s's tags", response.Status, r.Image)
		}
		var answer struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&answer)
		link := response.Header.Get("Link")
		response.Body.Close()
		if err != nil {
			return nil, err
		}
		tags = append(tags, answer.Tags...)
		path = ""
		if start, end := strings.Index(link, "<"), strings.Index(link, ">"); start >= 0 && end > start {
			path = link[start+1 : end]
		}
	}
	return tags, nil
}

var buildTag = regexp.MustCompile(`^build-(\d+)$`)

// BuildNumber is a build-<n> tag's number (0 for another tag).
func BuildNumber(tag string) int {
	m := buildTag.FindStringSubmatch(tag)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// Latest resolves latest: its digest, and the build-<n> tag that shares it
// (the newest builds are asked first; "" when none of the newest 20 does).
func (r *Registry) Latest(ctx context.Context) (string, string, error) {
	digest, err := r.Digest(ctx, "latest")
	if err != nil {
		return "", "", err
	}
	tags, err := r.Tags(ctx)
	if err != nil {
		return digest, "", nil
	}
	var builds []string
	for _, t := range tags {
		if BuildNumber(t) > 0 {
			builds = append(builds, t)
		}
	}
	sort.Slice(builds, func(i, j int) bool { return BuildNumber(builds[i]) > BuildNumber(builds[j]) })
	for i, t := range builds {
		if i == 20 {
			break
		}
		if d, err := r.Digest(ctx, t); err == nil && d == digest {
			return digest, t, nil
		}
	}
	return digest, "", nil
}

// ZipURL is a release's halo-linux-release.zip on GitHub.
func ZipURL(repository, build string) string {
	return "https://github.com/" + repository + "/releases/download/" + url.PathEscape(build) + "/halo-linux-release.zip"
}

// NotesURL is a release's page on GitHub.
func NotesURL(repository, build string) string {
	return "https://github.com/" + repository + "/releases/tag/" + url.PathEscape(build)
}

// Download fetches a URL into path (through a .part), returning its
// SHA-256 and size; 512 MB at most.
func Download(ctx context.Context, client *http.Client, source, path string) (string, int64, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return "", 0, err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("%s answered %s", source, response.Status)
	}
	part := path + ".part"
	file, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", 0, err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, 512<<20+1))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil && n > 512<<20 {
		err = errors.New("the zip is larger than 512 MB")
	}
	if err != nil {
		os.Remove(part)
		return "", 0, err
	}
	if err := os.Rename(part, path); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), n, nil
}
