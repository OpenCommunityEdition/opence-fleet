package fleet_test

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agent"
	"github.com/OpenCommunityEdition/opence-fleet/internal/agent/runtime/container"
	"github.com/OpenCommunityEdition/opence-fleet/internal/config"
	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
	"github.com/OpenCommunityEdition/opence-fleet/internal/dockerapi"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

// dockerNode enrols and runs an agent with the container runtime, on the
// machine's Docker.
func (h *harness) dockerNode(docker *dockerapi.Client, name string, base int) func() {
	h.t.Helper()
	settings := spec.DefaultNode()
	settings.Runtime = "docker"
	settings.TunnelPorts = spec.PortRange{First: base, Last: base + 9}
	settings.WebPorts = spec.PortRange{First: base + 100, Last: base + 109}
	ctx := context.Background()
	token, err := h.fleet.JoinToken(ctx, "test", name, settings)
	if err != nil {
		h.t.Fatal(err)
	}
	answer, err := agent.Join(ctx, http.DefaultClient, h.server.URL, token, name, "docker")
	if err != nil {
		h.t.Fatal(err)
	}
	c := &config.AgentConfig{URL: h.server.URL, Token: answer.Token, Dir: filepath.Join(h.work, "node-"+name), Name: name,
		Runtime: "docker"}
	return func() {
		a := agent.New(c, "dev-test", logger(h.t), container.New(docker, name, quiet()), http.DefaultClient)
		a.ReleaseKey = ed25519.PublicKey(make([]byte, 32))
		run, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			a.Run(run)
			close(done)
		}()
		stop := func() {
			cancel()
			<-done
		}
		h.t.Cleanup(stop)
		h.stopAgent = stop
		h.dockerAgent = a
	}
}

// TestDockerRuntime runs servers as containers of tools/fakehalo's image
// (FLEET_TEST_IMAGE, built by `docker build -f tools/fakehalo/Dockerfile`),
// on the machine's Docker: FLEET_TEST_DOCKER=1.
func TestDockerRuntime(t *testing.T) {
	if os.Getenv("FLEET_TEST_DOCKER") == "" {
		t.Skip("FLEET_TEST_DOCKER=1 runs the container runtime on this machine's Docker")
	}
	image := os.Getenv("FLEET_TEST_IMAGE")
	if image == "" {
		image = "fakehalo-server:test"
	}
	docker, err := dockerapi.New("")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := docker.InspectImage(ctx, image); err != nil {
		t.Skipf("no image %s (docker build -f tools/fakehalo/Dockerfile -t %s .): %v", image, image, err)
	}
	h := start(t)
	// (the node's own servers, whatever a failed run left)
	t.Cleanup(func() { removeContainers(docker, "docker-test") })
	removeContainers(docker, "docker-test")
	if err := h.fleet.AddLocalRelease(ctx, "test", "fake-image", image, true); err != nil {
		t.Fatal(err)
	}
	run := h.dockerNode(docker, "docker-test", 42500)
	run()
	h.until("the node", 10*time.Second, func() bool {
		n, err := h.fleet.NodeByName(ctx, "docker-test")
		return err == nil && h.fleet.NodeOnline(n.ID)
	})
	profile, err := h.fleet.CreateProfile(ctx, "user:paul", core.ProfileInput{Name: "Boxed", Rotation: "bloodgulch:ctf",
		Region: "eu", Settings: map[string]any{"server.max_players": "8"}})
	if err != nil {
		t.Fatal(err)
	}
	slugs, _, err := h.fleet.CreateInstances(ctx, "user:paul", core.CreateRequest{ProfileID: profile, Nodes: []string{"docker-test"},
		Count: 2, Pattern: "Boxed #{n}", Running: true})
	if err != nil || len(slugs) != 2 {
		t.Fatal(slugs, err)
	}
	for _, slug := range slugs {
		h.until(slug+" up", 60*time.Second, func() bool { return h.up(slug) })
	}
	if l := h.fleet.Live.Instance(slugs[0]); l.Status.MaxPlayers != 8 {
		t.Fatalf("%+v", l.Status)
	}

	first := containerOf(t, docker, slugs[0])
	i, _ := h.fleet.Instance(ctx, slugs[0])
	t.Run("the container is the plan's", func(t *testing.T) {
		c := first
		if c.Config.Labels["opence-fleet.slug"] != slugs[0] || c.Config.Labels["opence-fleet.node"] != "docker-test" ||
			c.Config.Labels["opence-fleet.build"] != "fake-image" {
			t.Fatalf("labels %v", c.Config.Labels)
		}
		if !c.HostConfig.ReadonlyRootfs || len(c.HostConfig.CapDrop) == 0 || c.HostConfig.RestartPolicy.Name != "" &&
			c.HostConfig.RestartPolicy.Name != "no" {
			t.Fatalf("host config %+v", c.HostConfig)
		}
		if c.HostConfig.Memory != 600<<20 {
			t.Fatalf("memory %d", c.HostConfig.Memory)
		}
		mounts := map[string]bool{}
		for _, m := range c.HostConfig.Mounts {
			mounts[m.Target] = !m.ReadOnly
		}
		if rw, ok := mounts["/data"]; !ok || rw {
			t.Fatalf("/data is not mounted read-only: %+v", c.Mounts)
		}
		if rw, ok := mounts["/state"]; !ok || !rw {
			t.Fatalf("/state is not mounted: %+v", c.Mounts)
		}
		web := c.HostConfig.PortBindings["8080/tcp"]
		tunnel := c.HostConfig.PortBindings["2302/udp"]
		if len(web) != 1 || web[0].HostIP != "127.0.0.1" || web[0].HostPort != itoa(i.WebPort) {
			t.Fatalf("web port %+v (want 127.0.0.1:%d)", web, i.WebPort)
		}
		if len(tunnel) != 1 || tunnel[0].HostPort != itoa(i.TunnelPort) {
			t.Fatalf("tunnel port %+v (want %d)", tunnel, i.TunnelPort)
		}
	})

	t.Run("the agent's self-update leaves the containers running", func(t *testing.T) {
		// (as handleUpdate does before it execs the new binary)
		h.dockerAgent.Detach()
		h.stopAgent()
		run()
		h.until("the node again", 10*time.Second, func() bool {
			n, _ := h.fleet.NodeByName(ctx, "docker-test")
			return h.fleet.NodeOnline(n.ID)
		})
		h.until("the status again", 20*time.Second, func() bool { return h.up(slugs[0]) })
		time.Sleep(2 * time.Second)
		if again := containerOf(t, docker, slugs[0]); again.ID != first.ID {
			t.Fatal("the container was made again")
		}
	})

	t.Run("restart and stop", func(t *testing.T) {
		if err := h.fleet.Restart(ctx, "user:paul", slugs[0]); err != nil {
			t.Fatal(err)
		}
		h.until("a new container", 30*time.Second, func() bool {
			c, err := findContainer(docker, slugs[0])
			return err == nil && c != nil && c.ID != first.ID && c.State.Running
		})
		h.until(slugs[0]+" up again", 60*time.Second, func() bool { return h.up(slugs[0]) })
		if err := h.fleet.SetRunning(ctx, "user:paul", slugs[1], false); err != nil {
			t.Fatal(err)
		}
		h.until(slugs[1]+" stopped", 30*time.Second, func() bool {
			l := h.fleet.Live.Instance(slugs[1])
			return l != nil && l.State == "stopped"
		})
		c, err := findContainer(docker, slugs[1])
		if err == nil && c != nil && c.State.Running {
			t.Fatal("the stopped server's container runs")
		}
	})

	t.Run("deleted, the server's container goes", func(t *testing.T) {
		if err := h.fleet.DeleteInstance(ctx, "user:paul", slugs[1]); err != nil {
			t.Fatal(err)
		}
		h.until("the container gone", 30*time.Second, func() bool {
			c, err := findContainer(docker, slugs[1])
			return err == nil && c == nil
		})
	})
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func findContainer(docker *dockerapi.Client, slug string) (*dockerapi.Container, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	list, err := docker.List(ctx, "opence-fleet.slug="+slug, true)
	if err != nil {
		return nil, err
	}
	for _, c := range list {
		if c.Labels["opence-fleet.node"] == "docker-test" {
			return docker.Inspect(ctx, c.ID)
		}
	}
	return nil, nil
}

func containerOf(t *testing.T, docker *dockerapi.Client, slug string) *dockerapi.Container {
	t.Helper()
	c, err := findContainer(docker, slug)
	if err != nil || c == nil {
		t.Fatalf("no container of %s: %v", slug, err)
	}
	return c
}

func removeContainers(docker *dockerapi.Client, node string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, _ := docker.List(ctx, "opence-fleet.node="+node, true)
	for _, c := range list {
		docker.Remove(ctx, c.ID, true)
	}
}
