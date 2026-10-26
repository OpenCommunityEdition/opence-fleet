// Package runtime is how the agent runs a server: as a container of the
// project's server image (package container, the default) or as a child
// process of the release (package process). The agent's supervisor is the
// same for both: it writes an instance's files, starts its server through a
// Runtime, reads its output and waits for its exit code.
package runtime

import (
	"context"
	"io"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

// Paths is where an instance's files are on the node.
type Paths struct {
	// the instance's folder (config.toml, init.txt, gametypes/, logs/,
	// saves/): the container's /state
	Instance string
	// the game data: the container's /data, read-only
	Data string
	// the process runtime's releases (releases/<build>/)
	Releases string
}

// Progress tells how a download goes.
type Progress func(done, total int64)

// Runtime runs servers.
type Runtime interface {
	// Name is docker or process.
	Name() string
	// Describe is the runtime's version line for the hello (Docker's).
	Describe(ctx context.Context) string
	// Ready tells whether a build is on the node (its image pulled, its
	// release unpacked).
	Ready(ctx context.Context, build string) bool
	// Fetch gets a build: the image by its digest, or the release's zip
	// from the control plane (url, its SHA-256), unpacked and checked.
	Fetch(ctx context.Context, image spec.Image, release spec.Release, progress Progress) error
	// Start makes an instance's server from its spec and starts it,
	// attached: its output is read from the handle.
	Start(ctx context.Context, instance spec.Instance, node spec.Node, paths Paths) (Handle, error)
	// Adopt finds the servers a previous run of the agent left running
	// (containers by their label, children by the handoff), by slug.
	Adopt(ctx context.Context) (map[string]Handle, []string, error)
	// Remove removes what is left of an instance's server (its stopped
	// container).
	Remove(ctx context.Context, slug string) error
	// Prune removes the builds no instance names, but the newest other
	// (for a rollback).
	Prune(ctx context.Context, keep []string) error
}

// Handle is a running (or ended) server.
type Handle interface {
	// ID is the container's id, or the process id.
	ID() string
	Slug() string
	// Generation and Build are the spec's the server was made from.
	Generation() int64
	Build() string
	Started() time.Time
	// Stdin is the owner's console.
	Stdin() io.Writer
	// Output copies the server's standard output and error until it ends.
	Output(stdout, stderr io.Writer) error
	// Wait waits for the server to end: its exit code (128+n for a signal).
	Wait(ctx context.Context) (int, error)
	// Stop sends SIGTERM, and SIGKILL after the grace.
	Stop(ctx context.Context, grace time.Duration) error
	// Signal sends a signal (HUP, USR1).
	Signal(ctx context.Context, signal string) error
	// Web is the server's web port's base URL, on the node's loopback.
	Web() string
	// Usage is the server's memory (bytes) and CPU (percent of one core).
	Usage(ctx context.Context) (int64, float64)
	// Detach lets the server run on without the agent (a self-update).
	Detach() error
}
