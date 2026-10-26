package web

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
	"github.com/OpenCommunityEdition/opence-fleet/internal/serverapi"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view/pages"
)

// fleetPlayers asks every up server its players, at once.
func (s *Server) fleetPlayers(ctx context.Context, search string) ([]pages.FleetPlayer, []string) {
	d := s.data(ctx)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var out []pages.FleetPlayer
	var problems []string
	search = strings.ToLower(strings.TrimSpace(search))
	call, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	for _, i := range d.instances {
		l := s.Fleet.Live.Instance(i.Slug)
		if l == nil || l.Status == nil || l.Status.Players == 0 {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			players, err := s.Fleet.Players(call, i.Slug)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				problems = append(problems, i.Name+" did not answer: "+err.Error())
				return
			}
			for _, p := range players {
				if search != "" && !strings.Contains(strings.ToLower(p.Name+" "+p.Address+" "+p.HWID+" "+clientText(p.Client)),
					search) {
					continue
				}
				out = append(out, pages.FleetPlayer{Player: p, Slug: i.Slug, Server: i.Name})
			}
		}()
	}
	wg.Wait()
	sort.Slice(out, func(a, b int) bool {
		if out[a].Server != out[b].Server {
			return out[a].Server < out[b].Server
		}
		return out[a].Slot < out[b].Slot
	})
	sort.Strings(problems)
	return out, problems
}

func (s *Server) playersPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	players, problems := s.fleetPlayers(r.Context(), q)
	s.render(w, r, http.StatusOK, pages.Players(s.page(r, "Players", "players"), players, q, problems))
}

func (s *Server) playersFragment(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	players, problems := s.fleetPlayers(r.Context(), q)
	s.render(w, r, http.StatusOK, pages.PlayersFleetTable(s.page(r, "", "players"), players, q, problems))
}

func (s *Server) bansPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	all := r.URL.Query().Get("all") == "1"
	bans, _ := s.Fleet.Bans(ctx, all)
	states, _ := s.Fleet.Store.ListBanStates(ctx)
	counts := map[int64][3]int{}
	for _, st := range states {
		c := counts[st.BanID]
		switch st.State {
		case "applied", "source":
			c[0]++
		case "pending":
			c[1]++
		default:
			c[2]++
		}
		counts[st.BanID] = c
	}
	var lines []pages.BanLine
	for _, b := range bans {
		c := counts[b.ID]
		lines = append(lines, pages.BanLine{FleetBan: b, Held: c[0], Pending: c[1], Failed: c[2]})
	}
	s.render(w, r, http.StatusOK, pages.Bans(s.page(r, "Bans", "bans"), lines, all))
}

func (s *Server) addBan(w http.ResponseWriter, r *http.Request) {
	duration, _ := time.ParseDuration(r.FormValue("duration"))
	_, err := s.Fleet.CreateBan(r.Context(), s.actor(r), core.BanInput{Address: r.FormValue("address"), HWID: r.FormValue("hwid"),
		Names: r.FormValue("names"), Reason: r.FormValue("reason"), Duration: duration, Source: "by hand"})
	if err != nil {
		s.fail(w, r, "/bans", err)
		return
	}
	s.done(w, r, "/bans", "The ban is made; every server gets it.")
}

func (s *Server) importBans(w http.ResponseWriter, r *http.Request) {
	n, err := s.Fleet.ImportBans(r.Context(), s.actor(r), r.FormValue("text"))
	if err != nil {
		s.fail(w, r, "/bans", err)
		return
	}
	s.done(w, r, "/bans", "Imported "+strconv.Itoa(n)+" bans.")
}

func (s *Server) unban(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Fleet.Unban(r.Context(), s.actor(r), id); err != nil {
		s.fail(w, r, "/bans", err)
		return
	}
	s.done(w, r, "/bans", "Unbanned: its lines go from every server's bans.txt.")
}

// clientText is what a player's game told of itself, for the search: its
// label, repository and mods.
func clientText(c *serverapi.ClientBuild) string {
	if c == nil {
		return ""
	}
	return c.Version + " " + c.Repository + " " + strings.Join(c.Mods, " ")
}
