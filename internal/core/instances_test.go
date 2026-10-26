package core

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

// where is an instance's ports and loopback index.
type where struct{ tunnel, web, loopback int64 }

func placed(t *testing.T, f *Fleet, slug string) where {
	t.Helper()
	i := instance(t, f, slug)
	return where{i.TunnelPort, i.WebPort, i.LoopbackIndex}
}

func TestAllocation(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	a, _ := node(t, f, "a", spec.DefaultNode())
	b, _ := node(t, f, "b", spec.DefaultNode())
	p := profile(t, f, "Ranked")

	// each one the next free ports and loopback index of its node's ranges
	onA := servers(t, f, p, 3, false, "a")
	if strings.Join(onA, " ") != "ranked-1 ranked-2 ranked-3" {
		t.Fatalf("%v", onA)
	}
	for k, slug := range onA {
		want := where{2302 + int64(k), 8101 + int64(k), int64(k)}
		if got := placed(t, f, slug); got != want || instance(t, f, slug).NodeID != a {
			t.Fatalf("%s: %+v on %d, not %+v", slug, got, instance(t, f, slug).NodeID, want)
		}
	}
	// another node has its own: the same ports again
	onB := servers(t, f, p, 2, false, "b")
	for k, slug := range onB {
		want := where{2302 + int64(k), 8101 + int64(k), int64(k)}
		if got := placed(t, f, slug); got != want || instance(t, f, slug).NodeID != b {
			t.Fatalf("%s: %+v, not %+v", slug, got, want)
		}
	}
	// the ports a removed server freed are the next ones given
	if err := f.DeleteInstance(ctx, "user:paul", "ranked-2"); err != nil {
		t.Fatal(err)
	}
	again := servers(t, f, p, 1, false, "a")
	if got := placed(t, f, again[0]); got != (where{2303, 8102, 1}) {
		t.Fatalf("%s: %+v", again[0], got)
	}
	// (its name is a free one: ranked-2 again)
	if again[0] != "ranked-2" {
		t.Fatalf("%s", again[0])
	}
	// a server's web password is its own, sealed to its slug
	first, _ := f.Key.OpenString(instance(t, f, "ranked-1").WebPasswordEnc, "instance:ranked-1:web_password")
	second, _ := f.Key.OpenString(instance(t, f, "ranked-3").WebPasswordEnc, "instance:ranked-3:web_password")
	if len(first) != 24 || first == second {
		t.Fatalf("web passwords %q and %q", first, second)
	}
}

func TestAllocationSpreads(t *testing.T) {
	f, _ := fleet(t)
	a, _ := node(t, f, "a", spec.DefaultNode())
	b, _ := node(t, f, "b", spec.DefaultNode())
	p := profile(t, f, "Casual")
	// the least loaded node first: four over two nodes are two and two
	slugs := servers(t, f, p, 4, false, "a", "b")
	count := map[int64]int{}
	seen := map[int64]map[int64]bool{a: {}, b: {}}
	for _, slug := range slugs {
		i := instance(t, f, slug)
		count[i.NodeID]++
		if seen[i.NodeID][i.TunnelPort] {
			t.Fatalf("tunnel port %d twice on node %d", i.TunnelPort, i.NodeID)
		}
		seen[i.NodeID][i.TunnelPort] = true
	}
	if count[a] != 2 || count[b] != 2 {
		t.Fatalf("%v", count)
	}
}

func TestAllocationFull(t *testing.T) {
	cases := []struct {
		name     string
		settings spec.Node
		fits     int
	}{
		{"ports", spec.Node{TunnelPorts: spec.PortRange{First: 3000, Last: 3001}, WebPorts: spec.PortRange{First: 4000, Last: 4009},
			LoopbackBase: 2}, 2},
		{"web ports", spec.Node{TunnelPorts: spec.PortRange{First: 3000, Last: 3009}, WebPorts: spec.PortRange{First: 4000, Last: 4002},
			LoopbackBase: 2}, 3},
		// (127.0.0.249 and 127.0.0.250 alone)
		{"loopback addresses", spec.Node{TunnelPorts: spec.PortRange{First: 3000, Last: 3009},
			WebPorts: spec.PortRange{First: 4000, Last: 4009}, LoopbackBase: 249}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, _ := fleet(t)
			ctx := context.Background()
			node(t, f, "small", c.settings)
			p := profile(t, f, "Small")
			// more than fit: refused whole, none made
			_, _, err := f.CreateInstances(ctx, "user:paul", CreateRequest{ProfileID: p, Nodes: []string{"small"}, Count: c.fits + 1})
			e, ok := AsError(err)
			if !ok || e.Status != http.StatusConflict || e.Code != "full" || !strings.Contains(e.Message, "node small has no free port") {
				t.Fatalf("%v", err)
			}
			if n, _ := f.Store.CountInstances(ctx); n != 0 {
				t.Fatalf("%d made of a refused request", n)
			}
			servers(t, f, p, c.fits, false, "small")
			_, _, err = f.CreateInstances(ctx, "user:paul", CreateRequest{ProfileID: p, Nodes: []string{"small"}, Count: 1})
			if e, ok := AsError(err); !ok || e.Code != "full" {
				t.Fatalf("one more: %v", err)
			}
		})
	}
}

func TestCreateInstancesRefusals(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	node(t, f, "a", spec.DefaultNode())
	p := profile(t, f, "Ranked")
	for _, c := range []struct {
		name string
		r    CreateRequest
		code string
	}{
		{"none", CreateRequest{ProfileID: p, Nodes: []string{"a"}}, "schema"},
		{"too many", CreateRequest{ProfileID: p, Nodes: []string{"a"}, Count: 101}, "schema"},
		{"no node", CreateRequest{ProfileID: p, Count: 1}, "schema"},
		{"an unknown node", CreateRequest{ProfileID: p, Nodes: []string{"z"}, Count: 1}, "not_found"},
		{"an unknown profile", CreateRequest{ProfileID: p + 1, Nodes: []string{"a"}, Count: 1}, "not_found"},
		{"an unknown release", CreateRequest{ProfileID: p, Nodes: []string{"a"}, Count: 1, Channel: "build-1"}, "no_release"},
	} {
		if _, _, err := f.CreateInstances(ctx, "user:paul", c.r); err == nil {
			t.Errorf("%s: made", c.name)
		} else if e, ok := AsError(err); !ok || e.Code != c.code {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestNodeSettingsKeepTheirServers(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	node(t, f, "a", spec.DefaultNode())
	servers(t, f, profile(t, f, "Ranked"), 2, false, "a")
	// a range that leaves a server's port out is refused
	narrow := spec.DefaultNode()
	narrow.TunnelPorts = spec.PortRange{First: 2303, Last: 2399}
	if e, ok := AsError(f.SetNodeSettings(ctx, "user:paul", "a", narrow)); !ok || e.Code != "in_use" {
		t.Fatalf("%v", e)
	}
	// a node with servers is not forgotten
	if e, ok := AsError(f.ForgetNode(ctx, "user:paul", "a")); !ok || e.Code != "in_use" {
		t.Fatalf("%v", e)
	}
}
