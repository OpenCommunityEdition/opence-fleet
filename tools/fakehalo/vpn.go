package main

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The VPN policy (docs/dedicated_server.md, "VPNs and linked Discord
// accounts"), as far as the fleet sees it: the netlist held, the door's
// refusals (join_refused), GET /api/vpn and POST /api/vpn/check.
//
// The real server fetches each of its masters' signed netlist; the fake
// holds one "master's" list of TEST-NET-2 (198.51.100.0/24, where the
// script's VPN machines come from) when it publishes (a publisher key and
// server.master_publish), as the real one holds its masters' alone.
// FAKEHALO_NETLIST changes that for a test: "held" (a master's list though
// it publishes nowhere), "none" (no list), or a duration ("72h": a list that
// old). server.vpn_list names a file read instead: the fake takes its lines
// as ranges (vpn_block.txt's format), not a signed netlist.

// the reasons a machine is refused for the VPN policy, in GET /api/vpn's
// refused
var refusals = []string{"vpn_link_required", "vpn_blocked", "vpn_link_invalid", "vpn_link_too_new", "vpn_link_in_use"}

// the fake's master, as its status names it
const fakeMaster = "https://master.opencommunityedition.com"

// vpnCapable tells whether the fake has the VPN policy (the vpn capability:
// /api/vpn, join_refused).
func vpnCapable() bool { return options.capable && options.vpn }

// addressRange is a range of IPv4 addresses, both ends in.
type addressRange struct{ first, last uint32 }

func ipv4(a netip.Addr) (uint32, bool) {
	if !a.Is4() {
		return 0, false
	}
	b := a.As4()
	return binary.BigEndian.Uint32(b[:]), true
}

// parseRanges reads lines of ranges: 203.0.113.0/24, 198.51.100.7 or
// 198.51.100.1-198.51.100.9; # starts a comment.
func parseRanges(text string) []addressRange {
	var out []addressRange
	for _, line := range strings.Split(text, "\n") {
		line, _, _ = strings.Cut(line, "#")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if p, err := netip.ParsePrefix(line); err == nil {
			first, ok := ipv4(p.Masked().Addr())
			if !ok {
				continue
			}
			size := uint32(1) << (32 - p.Bits())
			out = append(out, addressRange{first, first + size - 1})
			continue
		}
		from, to, isRange := strings.Cut(line, "-")
		a, err := netip.ParseAddr(strings.TrimSpace(from))
		if err != nil {
			continue
		}
		b := a
		if isRange {
			if b, err = netip.ParseAddr(strings.TrimSpace(to)); err != nil {
				continue
			}
		}
		first, ok1 := ipv4(a)
		last, ok2 := ipv4(b)
		if ok1 && ok2 && first <= last {
			out = append(out, addressRange{first, last})
		}
	}
	return out
}

func inRanges(list []addressRange, address string) bool {
	a, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	n, ok := ipv4(a)
	if !ok {
		return false
	}
	for _, r := range list {
		if n >= r.first && n <= r.last {
			return true
		}
	}
	return false
}

// netlist is what the fake holds: a master's list, or the file's.
type netlist struct {
	held      bool
	generated time.Time
	ranges    []addressRange
	file      string
}

// readNetlist reads the list the server holds now (call with g.mu held).
func (g *game) readNetlist() netlist {
	if path := g.st.text("server.vpn_list"); path != "" {
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(configPath()), path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return netlist{file: g.st.text("server.vpn_list")}
		}
		return netlist{held: true, file: g.st.text("server.vpn_list"), ranges: parseRanges(string(data))}
	}
	list := netlist{held: g.publishes(), generated: g.started.Add(-time.Hour),
		ranges: parseRanges("198.51.100.0/24")}
	switch setting := os.Getenv("FAKEHALO_NETLIST"); setting {
	case "":
	case "held":
		list.held = true
	case "none":
		list.held = false
	default:
		if age, err := time.ParseDuration(setting); err == nil {
			list.held, list.generated = true, time.Now().Add(-age)
		}
	}
	return list
}

// publishes tells whether the server publishes to its masters (a publisher
// key, and server.master_publish).
func (g *game) publishes() bool {
	return os.Getenv("HALO_SERVER_PUBLISHER_KEY") != "" && g.st.bool("server.master_publish")
}

// vpnFiles are vpn_allow.txt's and vpn_block.txt's ranges, beside
// bans.txt (read at each check: the fake's are small).
func (g *game) vpnFiles() (allow, block []addressRange) {
	read := func(name string) []addressRange {
		data, err := os.ReadFile(filepath.Join(g.logRoot, name))
		if err != nil {
			return nil
		}
		return parseRanges(string(data))
	}
	return read("vpn_allow.txt"), read("vpn_block.txt")
}

// vpnCheck is whether an address is refused as a VPN's, and by what
// (netlist, vpn_block.txt, vpn_allow.txt, or ""); call with g.mu held.
func (g *game) vpnCheck(address string) (bool, string) {
	allow, block := g.vpnFiles()
	switch list := g.readNetlist(); {
	case inRanges(allow, address):
		return false, "vpn_allow.txt"
	case inRanges(block, address):
		return true, "vpn_block.txt"
	case list.held && inRanges(list.ranges, address):
		return true, "netlist"
	}
	return false, ""
}

// door is the VPN policy's word on a joining machine: "" to let it in, else
// the refusal's reason (call with g.mu held).
func (g *game) door(p *player, pass bool) string {
	policy := g.st.text("server.vpn_policy")
	if !vpnCapable() || policy == "off" {
		return ""
	}
	listed, by := g.vpnCheck(p.Address)
	if by == "vpn_allow.txt" {
		return ""
	}
	if !listed {
		if !g.readNetlist().held && g.st.text("server.vpn_fail") == "closed" {
			return "vpn_blocked"
		}
		return ""
	}
	switch {
	case policy == "block":
		return "vpn_blocked"
	case !pass || !g.takesPasses():
		return "vpn_link_required"
	// (the script's accounts are two years old)
	case g.st.int("server.link_min_age_days") > 730:
		return "vpn_link_too_new"
	case g.st.bool("server.link_one_seat"):
		for _, q := range g.players {
			if q.DiscordVerified && q.DiscordID == p.DiscordID {
				return "vpn_link_in_use"
			}
		}
	}
	return ""
}

// refuse turns a machine away at the door: logged, counted and a
// join_refused event (call with g.mu held).
func (g *game) refuse(p *player, reason string) {
	if g.refused == nil {
		g.refused = map[string]int{}
	}
	g.refused[reason]++
	logLine("info", "net", fmt.Sprintf("refusing a machine @%s: vpn %s", p.Address, reason))
	g.emitPrivate("moderation", "join_refused", map[string]any{"name": p.Name, "reason": reason},
		map[string]any{"address": p.Address, "hwid": p.HWID})
}

// vpnJSON is GET /api/vpn's answer (call with g.mu held).
func (g *game) vpnJSON() map[string]any {
	list := g.readNetlist()
	lists := []map[string]any{}
	if list.held && list.file == "" {
		fetched := g.started.Unix()
		lists = append(lists, map[string]any{"master": fakeMaster, "key_id": "fakehalo", "serial": 1,
			"generated": list.generated.Unix(), "fetched": fetched, "ranges": len(list.ranges), "error": ""})
	}
	allow, block := g.vpnFiles()
	refused := map[string]any{}
	for _, r := range refusals {
		refused[r] = g.refused[r]
	}
	return map[string]any{"ok": true, "policy": g.st.text("server.vpn_policy"), "fail": g.st.text("server.vpn_fail"),
		"lists": lists, "file": g.st.text("server.vpn_list"), "allow_ranges": len(allow), "block_ranges": len(block),
		"held": list.held, "refused": refused}
}
