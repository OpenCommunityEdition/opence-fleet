// Package conformance checks tools/fakehalo against the game's own protocol
// simulator, tools/server_sim.py of the game's repository, which the game's
// tests keep true to the real server (OPENCE_FLEET_PLAN.md 9.1): the same
// requests to both, and the fake's answers must have the simulator's shape
// (every key the simulator answers, with a value of the same JSON kind).
//
//	OPENCE_SERVER_SIM=../halo/tools/server_sim.py go test ./tests/conformance
//
// Without OPENCE_SERVER_SIM (or Python), the test is skipped.
package conformance_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/serverapi"
)

const (
	adminPassword = "conformance-admin"
	ownerPassword = "conformance-owner"
)

// simulator starts server_sim.py on a free port; its base URL.
func simulator(t *testing.T, script string) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	cmd := exec.Command(python, script, "--port", "0", "--tick", "0.05", "--password", adminPassword,
		"--owner-password", ownerPassword, "--log-addresses")
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	lines := bufio.NewScanner(stdout)
	if !lines.Scan() {
		t.Fatal("the simulator said nothing")
	}
	base, ok := strings.CutPrefix(strings.TrimSpace(lines.Text()), "listening on ")
	if !ok {
		t.Fatalf("the simulator's first line: %q", lines.Text())
	}
	go func() {
		for lines.Scan() {
		}
	}()
	return base
}

func freePort(t *testing.T, network string) int {
	t.Helper()
	if network == "udp" {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).Port
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// fake builds and starts tools/fakehalo; its base URL.
func fake(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	bin := filepath.Join(work, "fakehalo")
	build := exec.Command("go", "build", "-o", bin, "../../tools/fakehalo")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "--make-maps", filepath.Join(work, "data")).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	port := freePort(t, "tcp")
	cmd := exec.Command(bin, "--server")
	cmd.Env = append(os.Environ(), "FAKEHALO_TICK=0.05", "HALO_DATA_ROOT="+filepath.Join(work, "data"),
		"HALO_CONFIG_FILE="+filepath.Join(work, "config.toml"), "HALO_SERVER_WEB_BIND=127.0.0.1",
		"HALO_SERVER_WEB_PORT="+strconv.Itoa(port), "HALO_NET_TUNNEL_PORT="+strconv.Itoa(freePort(t, "udp")),
		"HALO_SERVER_WEB_PASSWORD="+adminPassword, "HALO_SERVER_WEB_OWNER_PASSWORD="+ownerPassword,
		"HALO_SERVER_LOG_ADDRESSES=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(15 * time.Second)
	for {
		response, err := http.Get(base + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return base
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("fakehalo did not answer /healthz")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// shape compares a fake's answer to the simulator's: every key the
// simulator has, with a value of the same kind (null on either side is any
// kind; an array's first elements are compared, and events by their kind).
// A rotation entry's rules are free-form (the entry's own +rules).
func shape(path string, sim, fake any, problems *[]string) {
	if sim == nil || fake == nil || strings.HasSuffix(path, ".rules") {
		return
	}
	if strings.HasSuffix(path, ".events") {
		s, sok := sim.([]any)
		f, fok := fake.([]any)
		if sok && fok {
			first := func(list []any) map[string]any {
				m := map[string]any{}
				for _, e := range list {
					if o, ok := e.(map[string]any); ok {
						if kind, _ := o["event"].(string); kind != "" && m[kind] == nil {
							m[kind] = o
						}
					}
				}
				return m
			}
			fakes := first(f)
			for kind, e := range first(s) {
				if fe, ok := fakes[kind]; ok {
					shape(path+"["+kind+"]", e, fe, problems)
				}
			}
			return
		}
	}
	switch s := sim.(type) {
	case map[string]any:
		f, ok := fake.(map[string]any)
		if !ok {
			*problems = append(*problems, fmt.Sprintf("%s: an object in the simulator, %T in the fake", path, fake))
			return
		}
		keys := make([]string, 0, len(s))
		for k := range s {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fv, ok := f[k]
			if !ok {
				*problems = append(*problems, fmt.Sprintf("%s.%s: missing in the fake", path, k))
				continue
			}
			shape(path+"."+k, s[k], fv, problems)
		}
	case []any:
		f, ok := fake.([]any)
		if !ok {
			*problems = append(*problems, fmt.Sprintf("%s: an array in the simulator, %T in the fake", path, fake))
			return
		}
		if len(s) > 0 && len(f) > 0 {
			shape(path+"[0]", s[0], f[0], problems)
		}
	default:
		if fmt.Sprintf("%T", sim) != fmt.Sprintf("%T", fake) {
			*problems = append(*problems, fmt.Sprintf("%s: %T in the simulator, %T in the fake", path, sim, fake))
		}
	}
}

func TestFakeMatchesSimulator(t *testing.T) {
	script := os.Getenv("OPENCE_SERVER_SIM")
	if script == "" {
		t.Skip("OPENCE_SERVER_SIM names the game's tools/server_sim.py")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go to build fakehalo")
	}
	clients := map[string]*serverapi.Client{}
	for name, base := range map[string]string{"simulator": simulator(t, script), "fake": fake(t)} {
		clients[name] = &serverapi.Client{Base: base, Password: adminPassword, By: "conformance",
			HTTP: &http.Client{Timeout: 10 * time.Second}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// (a few of the scripted seconds, so that both have players and events)
	time.Sleep(time.Second)

	requests := []struct {
		method, path, body string
		// the answers' statuses differ by design (the fake is no simulator):
		// the shape alone is compared when both are 2xx
	}{
		{"GET", "/api/status", ""},
		{"GET", "/api/players", ""},
		{"GET", "/api/settings", ""},
		{"GET", "/api/rotation", ""},
		{"GET", "/api/gametypes", ""},
		{"GET", "/api/maps", ""},
		{"GET", "/api/bans", ""},
		{"GET", "/api/log?after=0&limit=20", ""},
		{"GET", "/api/events?after=0", ""},
		{"POST", "/api/command", `{"line":"sv_status"}`},
		{"POST", "/api/say", `{"text":"conformance"}`},
		{"PUT", "/api/rotation", `{"entries":["bloodgulch:ctf","nowhere:slayer"]}`},
		{"PUT", "/api/settings", `{"settings":{"server.max_players":"12"}}`},
		{"PUT", "/api/settings", `{"settings":{"server.max_players":"999"}}`},
		{"POST", "/api/bans", `{"ip":"203.0.113.9","reason":"conformance"}`},
		{"GET", "/api/nothing-here", ""},
	}
	for _, r := range requests {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			answers := map[string]any{}
			statuses := map[string]int{}
			for name, c := range clients {
				var body []byte
				if r.body != "" {
					body = []byte(r.body)
				}
				status, data, err := c.Do(ctx, r.method, r.path, body)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				var v any
				if err := json.Unmarshal(data, &v); err != nil {
					t.Fatalf("%s answered %d, not JSON: %.200s", name, status, data)
				}
				answers[name], statuses[name] = v, status
			}
			if statuses["simulator"]/100 != statuses["fake"]/100 {
				t.Fatalf("the simulator answered %d, the fake %d", statuses["simulator"], statuses["fake"])
			}
			var problems []string
			shape("answer", answers["simulator"], answers["fake"], &problems)
			if len(problems) > 0 {
				t.Errorf("the fake's answer is not the simulator's shape:\n  %s", strings.Join(problems, "\n  "))
			}
		})
	}
}
