package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/config"
	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
	"github.com/OpenCommunityEdition/opence-fleet/internal/update"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view"
	"github.com/OpenCommunityEdition/opence-fleet/internal/view/pages"
)

func (s *Server) nodeCard(ctx context.Context, name string) (view.NodeCard, bool) {
	n, err := s.Fleet.NodeByName(ctx, name)
	if err != nil {
		return view.NodeCard{}, false
	}
	instances, _ := s.Fleet.Store.ListInstancesByNode(ctx, n.ID)
	return view.NodeCardOf(n, s.Fleet.Live.Node(n.ID), len(instances), s.Fleet.Now()), true
}

func (s *Server) nodesPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	nodes, _ := s.Fleet.Nodes(ctx)
	var cards []view.NodeCard
	for _, n := range nodes {
		if c, ok := s.nodeCard(ctx, n.Name); ok {
			cards = append(cards, c)
		}
	}
	page := s.page(r, "Nodes", "nodes")
	page.Live = true
	s.render(w, r, http.StatusOK, pages.Nodes(page, cards))
}

func (s *Server) addNodePage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, pages.AddNode(s.page(r, "Add a node", "nodes"), "", "", "", ""))
}

// publicURL is the fleet's URL as its agents reach it: FLEET_PUBLIC_URL,
// else the request's own origin.
func (s *Server) publicURL(r *http.Request) string {
	if s.Fleet.PublicURL != "" {
		return s.Fleet.PublicURL
	}
	scheme := "http"
	if s.Clients.Of(r).Secure {
		scheme = "https"
	}
	return scheme + "://" + s.Clients.Host(r)
}

func (s *Server) addNode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := config.NodeName(r.FormValue("name"))
	settings := spec.DefaultNode()
	settings.Runtime = r.FormValue("runtime")
	if settings.Runtime != "process" {
		settings.Runtime = "docker"
	}
	settings.PublicAddress = strings.TrimSpace(r.FormValue("public_address"))
	token, err := s.Fleet.JoinToken(ctx, s.actor(r), name, settings)
	if err != nil {
		s.fail(w, r, "/nodes/add", err)
		return
	}
	url := s.publicURL(r)
	command := "curl -fsSL " + url + "/agent.sh | sudo sh -s -- --join " + url + " --token " + token
	if settings.Runtime == "process" {
		command += " --runtime process"
	}
	expires := view.Clock(s.Fleet.Now().Add(core.JoinTokenTTL), s.Fleet.Location())
	if wantsJSON(r) {
		writeJSON(w, map[string]any{"ok": true, "node": name, "token": token, "command": command})
		return
	}
	s.render(w, r, http.StatusOK, pages.AddNode(s.page(r, "Add a node", "nodes"), name, command, token, expires))
}

func (s *Server) nodePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	card, ok := s.nodeCard(ctx, name)
	if !ok {
		s.render(w, r, http.StatusNotFound, s.errorPage(r, "No such node", "There is no node "+name+"."))
		return
	}
	d := s.data(ctx)
	var rows []view.ServerRow
	for _, i := range d.instances {
		if i.NodeID == card.ID {
			rows = append(rows, d.row(s, i))
		}
	}
	hello := ""
	facts := card.Facts
	if l := s.Fleet.Live.Node(card.ID); l != nil && l.Hello.Hostname != "" {
		facts = l.Hello
	}
	facts.Instances, facts.LastSeq = nil, nil
	if data, err := json.MarshalIndent(facts, "", "  "); err == nil {
		hello = string(data)
	}
	page := s.page(r, name, "nodes")
	page.Live = true
	s.render(w, r, http.StatusOK, pages.NodePage(page, card, rows, hello))
}

func (s *Server) nodeSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	n, err := s.Fleet.NodeByName(ctx, name)
	if err != nil {
		s.fail(w, r, "/nodes", err)
		return
	}
	settings := n.Settings
	settings.Runtime = r.FormValue("runtime")
	if settings.TunnelPorts, err = spec.ParsePortRange(r.FormValue("tunnel_ports")); err != nil {
		s.fail(w, r, "/nodes/"+name, err)
		return
	}
	if settings.WebPorts, err = spec.ParsePortRange(r.FormValue("web_ports")); err != nil {
		s.fail(w, r, "/nodes/"+name, err)
		return
	}
	settings.PublicAddress = strings.TrimSpace(r.FormValue("public_address"))
	settings.MemoryMB, _ = strconv.Atoi(r.FormValue("memory_mb"))
	settings.CPUs, _ = strconv.ParseFloat(r.FormValue("cpus"), 64)
	settings.LoopbackBase, _ = strconv.Atoi(r.FormValue("loopback_base"))
	settings.HostNetwork = r.FormValue("host_network") == "1"
	settings.WebUI = r.FormValue("web_ui") == "1"
	settings.SyncAll = r.FormValue("sync_all") == "1"
	if err := s.Fleet.SetNodeSettings(ctx, s.actor(r), name, settings); err != nil {
		s.fail(w, r, "/nodes/"+name, err)
		return
	}
	s.done(w, r, "/nodes/"+name, "Saved: the node's servers take it at their next restart.")
}

func (s *Server) forgetNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.Fleet.ForgetNode(r.Context(), s.actor(r), name); err != nil {
		s.fail(w, r, "/nodes/"+name, err)
		return
	}
	s.done(w, r, "/nodes", name+" is forgotten.")
}

// updateAgent offers a node's agent the control plane's own release: the
// release's binary for its machine, with its signed SHA256SUMS.
func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	n, err := s.Fleet.NodeByName(ctx, name)
	if err != nil {
		s.fail(w, r, "/nodes", err)
		return
	}
	offer, err := s.agentOffer(ctx, n)
	if err != nil {
		s.fail(w, r, "/nodes/"+name, err)
		return
	}
	call, cancel := context.WithTimeout(ctx, 11*time.Minute)
	defer cancel()
	reply, err := s.Fleet.Hub.Request(call, n.ID, agentproto.TUpdateAgent, offer)
	if err != nil {
		s.fail(w, r, "/nodes/"+name, err)
		return
	}
	if reply.Error != nil {
		s.fail(w, r, "/nodes/"+name, core.Refuse(http.StatusBadGateway, reply.Error.Code, "%s", reply.Error.Message))
		return
	}
	s.Fleet.Audit(ctx, s.actor(r), "node.update_agent", name, offer.Version)
	s.done(w, r, "/nodes/"+name, strings.Join(reply.Lines, " "))
}

// agentOffer is the release of the agent that matches the control plane
// (its version's binary for the node's machine, and the signed sums).
func (s *Server) agentOffer(ctx context.Context, n core.Node) (agentproto.UpdateAgent, error) {
	if !update.Valid(s.Version) {
		return agentproto.UpdateAgent{}, core.Refuse(http.StatusConflict, "development", "a development build of the "+
			"control plane offers no agent updates (only vX.Y.Z releases do)")
	}
	if s.Updater == nil {
		return agentproto.UpdateAgent{}, core.Refuse(http.StatusConflict, "off", "updates are off (FLEET_UPDATES)")
	}
	release, err := s.Updater.Release(ctx, s.Version)
	if err != nil {
		return agentproto.UpdateAgent{}, core.Refuse(http.StatusBadGateway, "release", "%v", err)
	}
	arch := n.Facts.Arch
	if l := s.Fleet.Live.Node(n.ID); l != nil && l.Hello.Arch != "" {
		arch = l.Hello.Arch
	}
	asset := "opence-fleet-linux-" + arch
	if arch == "arm" {
		asset += "v7"
	}
	sums, sig, err := s.Updater.Sums(ctx, release)
	if err != nil {
		return agentproto.UpdateAgent{}, core.Refuse(http.StatusBadGateway, "release", "%v", err)
	}
	url := release.Assets[asset]
	if url == "" {
		return agentproto.UpdateAgent{}, core.Refuse(http.StatusBadGateway, "release", "the release has no %s", asset)
	}
	return agentproto.UpdateAgent{Version: s.Version, URL: url, Sums: string(sums), Sig: string(sig)}, nil
}
