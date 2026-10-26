package update

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Release reads a release of the fleet by its version (the agents are
// offered their control plane's own).
func (u *Updater) Release(ctx context.Context, version string) (Release, error) {
	if !Valid(version) {
		return Release{}, fmt.Errorf("%s is not a release version", version)
	}
	source := strings.TrimSuffix(u.Source, "/latest") + "/tags/" + version
	data, err := u.get(ctx, source, maximumRelease, "application/vnd.github+json")
	if err != nil {
		return Release{}, err
	}
	var answer struct {
		Tag    string `json:"tag_name"`
		URL    string `json:"html_url"`
		Body   string `json:"body"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		return Release{}, fmt.Errorf("the release is not JSON: %w", err)
	}
	release := Release{Version: answer.Tag, URL: answer.URL, Notes: answer.Body, Assets: map[string]string{}}
	for _, a := range answer.Assets {
		release.Assets[a.Name] = a.URL
	}
	return release, nil
}

// Sums downloads a release's SHA256SUMS and its signature (which the
// agent checks against the release key itself).
func (u *Updater) Sums(ctx context.Context, r Release) ([]byte, []byte, error) {
	sumsURL, signatureURL := r.Assets["SHA256SUMS"], r.Assets["SHA256SUMS.sig"]
	if sumsURL == "" || signatureURL == "" {
		return nil, nil, fmt.Errorf("release %s has no signed SHA256SUMS", r.Version)
	}
	sums, err := u.get(ctx, sumsURL, maximumSums, "")
	if err != nil {
		return nil, nil, err
	}
	signature, err := u.get(ctx, signatureURL, 1024, "")
	return sums, signature, err
}
