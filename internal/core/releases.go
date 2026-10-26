package core

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/live"
	"github.com/OpenCommunityEdition/opence-fleet/internal/releases"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

var buildName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func (f *Fleet) registry() *releases.Registry {
	return &releases.Registry{Image: f.Config.Image, HTTP: f.HTTP}
}

// Releases lists the releases known.
func (f *Fleet) Releases(ctx context.Context) ([]db.Release, error) { return f.Store.ListReleases(ctx) }

// CheckLatest resolves latest through the registry: the release it is, and
// whether it moved.
func (f *Fleet) CheckLatest(ctx context.Context, actor string) (db.Release, bool, error) {
	digest, tag, err := f.registry().Latest(ctx)
	if err != nil {
		return db.Release{}, false, Refuse(http.StatusBadGateway, "registry", "latest was not resolved: %v", err)
	}
	if tag == "" {
		tag = "sha-" + strings.TrimPrefix(digest, "sha256:")[:12]
	}
	previous, _ := f.Store.GetLatestRelease(ctx)
	if err := f.upsertRelease(ctx, tag, digest, true); err != nil {
		return db.Release{}, false, err
	}
	moved := previous.Build != tag
	if moved {
		f.Audit(ctx, actor, "release.latest", tag, map[string]string{"digest": digest, "was": previous.Build})
		if previous.Build != "" {
			f.raiseAlert(ctx, "release_out", tag, fmt.Sprintf("%s is out (was %s): update the servers that follow latest", tag, previous.Build))
			f.Live.AddFeed(live.FeedItem{Kind: "info", Event: "release", Text: tag + " is out"})
		}
		// (the nodes pull it; their servers that follow latest are marked
		// restart needed, and run it at their next start: nothing restarts
		// by itself)
		f.PushAll(ctx)
	}
	r, err := f.Store.GetRelease(ctx, tag)
	return r, moved, err
}

func (f *Fleet) upsertRelease(ctx context.Context, build, digest string, latest bool) error {
	return f.Store.Tx(ctx, func(q *db.Queries) error {
		if err := q.UpsertRelease(ctx, db.UpsertReleaseParams{Build: build, Image: f.Config.Image + ":" + build, Digest: digest,
			ResolvedAt: f.Now().Unix(), Url: releases.ZipURL(f.Config.ReleaseRepository, build),
			NotesUrl: releases.NotesURL(f.Config.ReleaseRepository, build)}); err != nil {
			return err
		}
		if latest {
			if err := q.ClearLatest(ctx); err != nil {
				return err
			}
			return q.SetLatest(ctx, build)
		}
		return nil
	})
}

// FetchRelease resolves a tag (build-206) to its digest and keeps it.
func (f *Fleet) FetchRelease(ctx context.Context, actor, tag string) error {
	tag = strings.TrimSpace(tag)
	if !buildName.MatchString(tag) || tag == "latest" {
		return Refuse(http.StatusBadRequest, "schema", "a release's tag is build-<n> (latest is resolved by itself)")
	}
	digest, err := f.registry().Digest(ctx, tag)
	if err != nil {
		return Refuse(http.StatusBadGateway, "registry", "%v", err)
	}
	if err := f.upsertRelease(ctx, tag, digest, false); err != nil {
		return err
	}
	f.Audit(ctx, actor, "release.fetch", tag, map[string]string{"digest": digest})
	return nil
}

// AddLocalRelease names an image the nodes have (built on them, or pushed
// to a registry of the operator's): no digest is pinned, and the agents use
// it as it is. It can be made latest.
func (f *Fleet) AddLocalRelease(ctx context.Context, actor, build, image string, latest bool) error {
	if !buildName.MatchString(build) {
		return Refuse(http.StatusBadRequest, "schema", "a build's name is letters, digits, '.', '_' and '-'")
	}
	image = strings.TrimSpace(image)
	if image == "" || strings.ContainsAny(image, " \t") {
		return Refuse(http.StatusBadRequest, "schema", "an image reference (opence-server:dev, or registry/opence-server:tag)")
	}
	err := f.Store.Tx(ctx, func(q *db.Queries) error {
		if err := q.UpsertRelease(ctx, db.UpsertReleaseParams{Build: build, Image: image, ResolvedAt: f.Now().Unix(), Local: 1}); err != nil {
			return err
		}
		if latest {
			if err := q.ClearLatest(ctx); err != nil {
				return err
			}
			return q.SetLatest(ctx, build)
		}
		return nil
	})
	if err != nil {
		return err
	}
	f.Audit(ctx, actor, "release.local", build, map[string]any{"image": image, "latest": latest})
	if latest {
		f.PushAll(ctx)
	}
	return nil
}

// SetLatest makes a known release the one latest follows (a rollback of
// every server that follows latest, say).
func (f *Fleet) SetLatest(ctx context.Context, actor, build string) error {
	if _, err := f.Store.GetRelease(ctx, build); err != nil {
		return NotFound("release " + build)
	}
	err := f.Store.Tx(ctx, func(q *db.Queries) error {
		if err := q.ClearLatest(ctx); err != nil {
			return err
		}
		return q.SetLatest(ctx, build)
	})
	if err != nil {
		return err
	}
	f.Audit(ctx, actor, "release.set_latest", build, nil)
	return nil
}

// ZipPath is where a release's zip is kept.
func (f *Fleet) ZipPath(build string) string {
	return filepath.Join(f.Config.Data, "releases", build+".zip")
}

// FetchZip downloads a release's halo-linux-release.zip (the process
// runtime's) from GitHub and pins its SHA-256.
func (f *Fleet) FetchZip(ctx context.Context, actor, build string) error {
	r, err := f.Store.GetRelease(ctx, build)
	if err != nil {
		return NotFound("release " + build)
	}
	source := r.Url
	if source == "" {
		source = releases.ZipURL(f.Config.ReleaseRepository, build)
	}
	os.MkdirAll(filepath.Dir(f.ZipPath(build)), 0o700)
	client := &http.Client{Timeout: 10 * time.Minute, Transport: f.HTTP.Transport}
	sha, size, err := releases.Download(ctx, client, source, f.ZipPath(build))
	if err != nil {
		return Refuse(http.StatusBadGateway, "download", "%v", err)
	}
	if err := f.Store.UpdateReleaseZip(ctx, db.UpdateReleaseZipParams{ZipUrl: source, ZipSha256: sha, ZipSize: size, Build: build}); err != nil {
		return err
	}
	f.Audit(ctx, actor, "release.zip", build, map[string]any{"sha256": sha, "size": size})
	f.PushAll(ctx)
	return nil
}

// AddZip keeps a release's zip from a file (a build of the operator's).
func (f *Fleet) AddZip(ctx context.Context, actor, build, path string) error {
	if !buildName.MatchString(build) {
		return Refuse(http.StatusBadRequest, "schema", "a build's name is letters, digits, '.', '_' and '-'")
	}
	if _, err := f.Store.GetRelease(ctx, build); err != nil {
		if err := f.Store.UpsertRelease(ctx, db.UpsertReleaseParams{Build: build, Image: f.Config.Image + ":" + build,
			ResolvedAt: f.Now().Unix(), Local: 1}); err != nil {
			return err
		}
	}
	os.MkdirAll(filepath.Dir(f.ZipPath(build)), 0o700)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := os.WriteFile(f.ZipPath(build), data, 0o600); err != nil {
		return err
	}
	sha, size, err := hashFile(f.ZipPath(build))
	if err != nil {
		return err
	}
	if err := f.Store.UpdateReleaseZip(ctx, db.UpdateReleaseZipParams{ZipUrl: "", ZipSha256: sha, ZipSize: size, Build: build}); err != nil {
		return err
	}
	f.Audit(ctx, actor, "release.zip", build, map[string]any{"sha256": sha, "size": size})
	f.PushAll(ctx)
	return nil
}

// ForgetRelease forgets a release no instance runs or follows.
func (f *Fleet) ForgetRelease(ctx context.Context, actor, build string) error {
	r, err := f.Store.GetRelease(ctx, build)
	if err != nil {
		return NotFound("release " + build)
	}
	if r.IsLatest != 0 {
		return Refuse(http.StatusConflict, "in_use", "%s is latest", build)
	}
	for slug, l := range f.Live.Instances() {
		if l.Build == build {
			return Refuse(http.StatusConflict, "in_use", "%s runs %s", slug, build)
		}
	}
	instances, _ := f.Instances(ctx)
	for _, i := range instances {
		if i.Channel == build {
			return Refuse(http.StatusConflict, "in_use", "%s follows %s", i.Slug, build)
		}
	}
	profiles, _ := f.Profiles(ctx)
	for _, p := range profiles {
		if p.Channel == build {
			return Refuse(http.StatusConflict, "in_use", "profile %s follows %s", p.Name, build)
		}
	}
	if err := f.Store.DeleteRelease(ctx, build); err != nil {
		return err
	}
	os.Remove(f.ZipPath(build))
	f.Audit(ctx, actor, "release.forget", build, nil)
	return nil
}

// releasesLoop resolves latest a minute after the start, then every
// FLEET_RELEASE_CHECK_INTERVAL.
func (f *Fleet) releasesLoop(ctx context.Context) {
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = f.Config.ReleaseCheckInterval
		if _, _, err := f.CheckLatest(ctx, "auto"); err != nil {
			f.Log.Info("latest was not resolved", "err", err)
		}
	}
}
