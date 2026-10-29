// Package e2e runs the fleet with the real game (OPENCE_FLEET_PLAN.md 9.3):
// a control plane and an agent on loopback, the agent running the game's
// server image (deploy/docker in the game's repository, built from its
// zip) with the retail maps. Not in CI: it needs the maps.
//
//	OPENCE_E2E_IMAGE=opence-server:local OPENCE_MAPS=/path/to/halo go test -v ./tests/e2e
//
// OPENCE_MAPS is the folder that holds maps/ (bloodgulch and sidewinder at
// least). Two servers are made from one profile; both reach lobby; a
// rotation changed on the profile reaches both live; a fleet ban reaches
// both servers' ban lists; an update to "another" build (the same image
// under a second name) drains and restarts both; and the control plane's
// end leaves both games running. (A game client joining by the invite, as
// the game's own tools/test_dedicated_server.py does, is not done here.)
package e2e_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agent"
	"github.com/OpenCommunityEdition/opence-fleet/internal/agent/runtime/container"
	"github.com/OpenCommunityEdition/opence-fleet/internal/auth"
	"github.com/OpenCommunityEdition/opence-fleet/internal/config"
	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
	"github.com/OpenCommunityEdition/opence-fleet/internal/crypt"
	"github.com/OpenCommunityEdition/opence-fleet/internal/dockerapi"
	"github.com/OpenCommunityEdition/opence-fleet/internal/httpx"
	"github.com/OpenCommunityEdition/opence-fleet/internal/live"
	"github.com/OpenCommunityEdition/opence-fleet/internal/serverapi"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store"
	"github.com/OpenCommunityEdition/opence-fleet/internal/web"
	"github.com/OpenCommunityEdition/opence-fleet/static"
)

const node = "e2e"

func logger() *slog.Logger {
	if os.Getenv("FLEET_TEST_VERBOSE") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func until(t *testing.T, what string, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s", timeout, what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func removeContainers(docker *dockerapi.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	list, _ := docker.List(ctx, "opence-fleet.node="+node, true)
	for _, c := range list {
		docker.Remove(ctx, c.ID, true)
	}
}

func TestRealGame(t *testing.T) {
	image, maps := os.Getenv("OPENCE_E2E_IMAGE"), os.Getenv("OPENCE_MAPS")
	if image == "" || maps == "" {
		t.Skip("OPENCE_E2E_IMAGE (the game's server image) and OPENCE_MAPS (the folder holding maps/) run the real game")
	}
	if _, err := os.Stat(filepath.Join(maps, "maps", "bloodgulch.map")); err != nil {
		t.Skipf("no maps/bloodgulch.map in %s", maps)
	}
	docker, err := dockerapi.New("")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := docker.InspectImage(ctx, image); err != nil {
		t.Skipf("no image %s: %v", image, err)
	}
	removeContainers(docker)
	t.Cleanup(func() { removeContainers(docker) })

	// ---------- the control plane
	work := t.TempDir()
	c := &config.ServeConfig{Name: "E2E", Data: filepath.Join(work, "data"), DB: filepath.Join(work, "fleet.db"),
		Image: "127.0.0.1:9/none", ReleaseCheckInterval: time.Hour, EventsDays: 30, SamplesDays: 7, LogAddresses: true,
		Updates: "off", Timezone: time.UTC, SessionHours: 12}
	s, err := store.Open(c.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := core.New(s, crypt.New(), c, logger(), live.New())
	server := web.New(f, auth.New(s, 12, 240), &httpx.Clients{}, logger(), "e2e")
	web.Static = static.Files
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	f.PublicURL = ts.URL
	fleetCtx, stopFleet := context.WithCancel(ctx)
	t.Cleanup(stopFleet)
	go f.Run(fleetCtx)
	for _, build := range []string{"real-1", "real-2"} {
		if err := f.AddLocalRelease(ctx, "test", build, image, build == "real-1"); err != nil {
			t.Fatal(err)
		}
	}

	// ---------- the node: the container runtime, the maps as its data
	settings := spec.DefaultNode()
	settings.Runtime = "docker"
	settings.TunnelPorts = spec.PortRange{First: 42900, Last: 42909}
	settings.WebPorts = spec.PortRange{First: 42950, Last: 42959}
	token, err := f.JoinToken(ctx, "test", node, settings)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := agent.Join(ctx, http.DefaultClient, ts.URL, token, node, "docker")
	if err != nil {
		t.Fatal(err)
	}
	ac := &config.AgentConfig{URL: ts.URL, Token: answer.Token, Dir: filepath.Join(work, "node"), Name: node,
		Runtime: "docker", DataRoot: maps}
	a := agent.New(ac, "e2e", logger(), container.New(docker, node, logger()), http.DefaultClient)
	a.ReleaseKey = ed25519.PublicKey(make([]byte, 32))
	agentCtx, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan struct{})
	go func() {
		a.Run(agentCtx)
		close(agentDone)
	}()
	t.Cleanup(func() {
		stopAgent()
		<-agentDone
	})

	// ---------- two servers of one profile
	profile, err := f.CreateProfile(ctx, "user:e2e", core.ProfileInput{Name: "E2E CTF",
		Rotation: "bloodgulch:ctf, sidewinder:ctf", Region: "eu", Settings: map[string]any{"network.online": false}})
	if err != nil {
		t.Fatal(err)
	}
	slugs, _, err := f.CreateInstances(ctx, "user:e2e", core.CreateRequest{ProfileID: profile, Nodes: []string{node},
		Count: 2, Pattern: "E2E #{n}", Running: true})
	if err != nil || len(slugs) != 2 {
		t.Fatal(slugs, err)
	}
	lobby := func(slug string) bool {
		l := f.Live.Instance(slug)
		return l != nil && l.State == "up" && l.Status != nil && l.Status.State == "lobby"
	}
	for _, slug := range slugs {
		until(t, slug+" in its lobby, on bloodgulch", 2*time.Minute, func() bool {
			return lobby(slug) && f.Live.Instance(slug).Status.Map == "bloodgulch"
		})
	}

	t.Run("the profile's rotation reaches both live", func(t *testing.T) {
		in := core.ProfileInput{Name: "E2E CTF", Rotation: "bloodgulch:ctf, sidewinder:slayer, bloodgulch:slayer",
			Region: "eu", Settings: map[string]any{"network.online": false}}
		if err := f.UpdateProfile(ctx, "user:e2e", profile, in); err != nil {
			t.Fatal(err)
		}
		for _, slug := range slugs {
			until(t, slug+"'s rotation", 30*time.Second, func() bool {
				var r serverapi.RotationAnswer
				return f.Call(ctx, slug, http.MethodGet, "/api/rotation", nil, &r) == nil && len(r.Entries) == 3
			})
			if l := f.Live.Instance(slug); l.RestartNeeded {
				t.Fatalf("%s needs a restart for a rotation", slug)
			}
		}
	})

	t.Run("a fleet ban reaches both servers", func(t *testing.T) {
		if _, err := f.CreateBan(ctx, "user:e2e", core.BanInput{Address: "203.0.113.77", Reason: "e2e",
			Duration: time.Hour}); err != nil {
			t.Fatal(err)
		}
		for _, slug := range slugs {
			until(t, slug+"'s ban", 30*time.Second, func() bool {
				_, body, err := f.API(ctx, slug, http.MethodGet, "/api/bans", nil)
				return err == nil && strings.Contains(string(body), "203.0.113.77")
			})
		}
	})

	t.Run("an update drains and restarts both on the other build", func(t *testing.T) {
		before := map[string]string{}
		for _, slug := range slugs {
			c, err := findContainer(docker, slug)
			if err != nil || c == nil {
				t.Fatal(slug, err)
			}
			before[slug] = c.ID
		}
		id, err := f.StartJob(ctx, "user:e2e", core.JobParams{Action: core.ActionUpdate, Slugs: slugs, Channel: "real-2",
			After: "empty", PerNode: 1})
		if err != nil {
			t.Fatal(err)
		}
		until(t, "the update job", 4*time.Minute, func() bool {
			job, _, err := f.Job(ctx, id)
			return err == nil && job.State != "running" && job.State != "queued"
		})
		job, items, _ := f.Job(ctx, id)
		data, _ := json.Marshal(items)
		if job.State != "done" {
			t.Fatalf("the update job: %s (%s) %s", job.State, job.Detail, data)
		}
		for _, slug := range slugs {
			until(t, slug+" up on real-2", time.Minute, func() bool {
				l := f.Live.Instance(slug)
				return lobby(slug) && l.Build == "real-2"
			})
			c, _ := findContainer(docker, slug)
			if c == nil || c.ID == before[slug] || c.Config.Labels["opence-fleet.build"] != "real-2" {
				t.Fatalf("%s was not restarted on real-2", slug)
			}
		}
	})

	t.Run("the control plane's end leaves the games running", func(t *testing.T) {
		ids := map[string]string{}
		for _, slug := range slugs {
			c, _ := findContainer(docker, slug)
			ids[slug] = c.ID
		}
		stopFleet()
		ts.Close()
		time.Sleep(5 * time.Second)
		for _, slug := range slugs {
			c, err := findContainer(docker, slug)
			if err != nil || c == nil || c.ID != ids[slug] || !c.State.Running {
				t.Fatalf("%s did not keep running", slug)
			}
		}
	})
}

func findContainer(docker *dockerapi.Client, slug string) (*dockerapi.Container, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	list, err := docker.List(ctx, "opence-fleet.slug="+slug, true)
	if err != nil {
		return nil, err
	}
	for _, c := range list {
		if c.Labels["opence-fleet.node"] == node && c.State == "running" {
			return docker.Inspect(ctx, c.ID)
		}
	}
	return nil, nil
}
