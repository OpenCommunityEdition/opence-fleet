package core

import (
	"context"

	"github.com/OpenCommunityEdition/opence-fleet/internal/live"
	"github.com/OpenCommunityEdition/opence-fleet/internal/releases"
	"github.com/OpenCommunityEdition/opence-fleet/internal/serverapi"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// releaseNetworkVersion is the network version a release is known to have
// (0: not known).
func (f *Fleet) releaseNetworkVersion(ctx context.Context, build string) int {
	r, err := f.Store.GetRelease(ctx, build)
	if err != nil {
		return 0
	}
	return int(r.NetworkVersion)
}

// runningNetworkVersion is the network version a server runs: its own
// word, else its build's.
func (f *Fleet) runningNetworkVersion(ctx context.Context, l *live.Instance) int {
	if l.Status != nil && l.Status.NetworkVersion > 0 {
		return l.Status.NetworkVersion
	}
	return f.releaseNetworkVersion(ctx, l.Build)
}

// learnNetworkVersion reads a release's network version: its image's label
// (org.opence.network-version, in the configuration the registry has for
// its digest), else the GitHub release's notes ("Network version: N"). A
// local release, or one of neither, is learnt from the first server that
// runs it (learnFromServer).
func (f *Fleet) learnNetworkVersion(ctx context.Context, build, digest string) {
	r, err := f.Store.GetRelease(ctx, build)
	if err != nil || r.Local != 0 || r.NetworkVersion > 0 && r.NetworkSource != "server" {
		return
	}
	n, source := 0, ""
	if digest != "" {
		labels, err := f.registry().Labels(ctx, digest)
		if err != nil {
			f.Log.Info("a release's labels were not read", "build", build, "err", err)
		}
		if n = releases.LabelNetworkVersion(labels); n > 0 {
			source = "label"
		}
	}
	if n == 0 && f.Config.ReleaseRepository != "" {
		notes, err := releases.Notes(ctx, f.HTTP, f.Config.ReleaseRepository, build)
		if err != nil {
			f.Log.Info("a release's notes were not read", "build", build, "err", err)
		}
		if n = releases.NotesNetworkVersion(notes); n > 0 {
			source = "notes"
		}
	}
	if n > 0 {
		f.Store.SetReleaseNetworkVersion(ctx, db.SetReleaseNetworkVersionParams{NetworkVersion: int64(n), NetworkSource: source,
			Build: build})
	}
}

// learnFromServer keeps the network version a server says its build has,
// for a release whose own is not known (once a build: the status comes
// every 2 seconds).
func (f *Fleet) learnFromServer(build string, status *serverapi.Status) {
	if build == "" || status == nil || status.NetworkVersion <= 0 {
		return
	}
	f.learnMu.Lock()
	done := f.learnt[build]
	f.learnt[build] = true
	f.learnMu.Unlock()
	if done {
		return
	}
	ctx := context.Background()
	if r, err := f.Store.GetRelease(ctx, build); err == nil && r.NetworkVersion == 0 {
		f.Store.SetReleaseNetworkVersion(ctx, db.SetReleaseNetworkVersionParams{NetworkVersion: int64(status.NetworkVersion),
			NetworkSource: "server", Build: build})
	}
}
