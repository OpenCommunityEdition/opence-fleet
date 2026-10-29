package web_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// A node's page draws its health's graphs over the window it is asked.
func TestNodeGraphs(t *testing.T) {
	x := newFixture(t, nil, true)
	x.instance()
	ctx := context.Background()
	n, err := x.fleet.NodeByName(ctx, "home")
	if err != nil {
		t.Fatal(err)
	}
	cpu, in, out := 37.0, 2048.0, 512.0
	now := time.Now().Truncate(time.Minute)
	for m := 1; m <= 5; m++ {
		if err := x.fleet.Store.InsertNodeSample(ctx, db.InsertNodeSampleParams{NodeID: n.ID, Time: now.Add(-time.Duration(m) * time.Minute).Unix(),
			Cpu: &cpu, MemoryUsed: 1 << 30, MemoryTotal: 2 << 30, DiskUsed: 10, DiskTotal: 40, NetIn: &in, NetOut: &out}); err != nil {
			t.Fatal(err)
		}
	}
	page := x.do(signed(request(http.MethodGet, "/nodes/home?window=1h", loopback, nil), x.cookie)).Body.String()
	for _, want := range []string{"Health", "the last 1 hour", `aria-current="true"`, "37 %", "50 % · 1.0 GB of 2.0 GB",
		"in 2 KB/s · out 512 B/s", "<path d=\"M", "<title>"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the node's page has no %q", want)
		}
	}
	// a window without samples: the graphs say so
	other := x.do(signed(request(http.MethodGet, "/nodes/home?window=7d", loopback, nil), x.cookie)).Body.String()
	if !strings.Contains(other, "the last 7 days") || strings.Contains(other, "No samples yet") {
		t.Fatal("the week's window")
	}
}
