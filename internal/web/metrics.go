package web

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
)

func label(text string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(text)
}

// metrics is /metrics: the fleet's state for Prometheus, one scrape target
// for a whole fleet.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if !s.Fleet.Config.Metrics {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	d := s.data(ctx)
	var b strings.Builder
	write := func(name, help, kind string, lines []string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
		sort.Strings(lines)
		for _, l := range lines {
			b.WriteString(name + l + "\n")
		}
	}
	online, offline := 0, 0
	for _, n := range d.nodes {
		if s.Fleet.NodeOnline(n.ID) {
			online++
		} else {
			offline++
		}
	}
	write("fleet_nodes", "Nodes by state.", "gauge", []string{fmt.Sprintf(`{state="online"} %d`, online),
		fmt.Sprintf(`{state="offline"} %d`, offline)})
	states := map[string]int{}
	var players, most, frames, up, listed, games []string
	for _, i := range d.instances {
		row := d.row(s, i)
		state := strings.TrimPrefix(row.State, "up: ")
		states[fmt.Sprintf(`{state="%s",node="%s",profile="%s"}`, label(state), label(row.Node), label(row.Profile))]++
		upValue := 0
		if row.Up() {
			upValue = 1
		}
		up = append(up, fmt.Sprintf(`{instance="%s"} %d`, label(i.Slug), upValue))
		if st := row.Status; st != nil {
			players = append(players, fmt.Sprintf(`{instance="%s",node="%s"} %d`, label(i.Slug), label(row.Node), st.Players))
			most = append(most, fmt.Sprintf(`{instance="%s"} %d`, label(i.Slug), st.MaxPlayers))
			frames = append(frames, fmt.Sprintf(`{instance="%s"} %g`, label(i.Slug), st.FrameMsMax))
			for _, m := range st.Masters {
				v := 0
				if m.Listed {
					v = 1
				}
				listed = append(listed, fmt.Sprintf(`{instance="%s",master="%s"} %d`, label(i.Slug), label(m.URL), v))
			}
		}
		if n, err := s.Fleet.Store.CountGames(ctx, i.Slug); err == nil {
			games = append(games, fmt.Sprintf(`{instance="%s"} %d`, label(i.Slug), n))
		}
	}
	var stateLines []string
	for k, v := range states {
		stateLines = append(stateLines, fmt.Sprintf("%s %d", k, v))
	}
	write("fleet_instances", "Servers by state, node and profile.", "gauge", stateLines)
	write("fleet_instance_up", "Whether a server's game runs.", "gauge", up)
	write("fleet_players", "Players on a server now.", "gauge", players)
	write("fleet_players_max", "A server's most players.", "gauge", most)
	write("fleet_frame_ms_max", "A server's longest frame of the last minute.", "gauge", frames)
	write("fleet_master_listed", "Whether a master lists a server.", "gauge", listed)
	write("fleet_games_total", "Games a server played (kept).", "counter", games)
	alerts, _ := s.Fleet.Store.ListOpenAlerts(ctx)
	rules := map[string]int{}
	for _, a := range alerts {
		rules[a.Rule]++
	}
	var alertLines []string
	for _, rule := range core.Rules {
		alertLines = append(alertLines, fmt.Sprintf(`{rule="%s"} %d`, rule.ID, rules[rule.ID]))
	}
	write("fleet_alerts", "Open alerts by rule.", "gauge", alertLines)
	jobs, _ := s.Fleet.Store.ListJobs(ctx, 1000)
	jobStates := map[string]int{}
	for _, j := range jobs {
		jobStates[j.State]++
	}
	var jobLines []string
	for k, v := range jobStates {
		jobLines = append(jobLines, fmt.Sprintf(`{state="%s"} %d`, label(k), v))
	}
	write("fleet_jobs", "The last thousand jobs by state.", "gauge", jobLines)
	write("fleet_build_info", "The fleet's version.", "gauge", []string{fmt.Sprintf(`{version="%s"} 1`, label(s.Version))})
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write([]byte(b.String()))
}
