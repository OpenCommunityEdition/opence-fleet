package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

var roles = map[string]int{"viewer": 1, "moderator": 2, "admin": 3, "owner": 4}

// the commands' roles
var commandRoles = map[string]string{
	"help": "viewer", "sv_status": "viewer", "sv_players": "viewer", "sv_rotation": "viewer", "sv_maps": "viewer",
	"sv_gametypes": "viewer", "sv_version": "viewer", "sv_log": "viewer", "sv_masters": "viewer", "sv_publisher": "viewer",
	"sv_bans": "admin", "sv_settings": "admin", "say": "moderator", "sv_say": "moderator", "tell": "moderator",
	"kick": "moderator", "sv_kick": "moderator", "ban": "moderator", "sv_ban": "moderator", "unban": "admin",
	"sv_unban": "admin", "mute": "moderator", "unmute": "moderator", "sv_map": "moderator", "sv_map_next": "moderator",
	"sv_next": "moderator", "sv_end_game": "moderator", "sv_rotation_set": "admin", "sv_set": "admin", "sv_name": "admin",
	"sv_maxplayers": "admin", "sv_password": "admin", "sv_public": "admin", "sv_motd": "admin", "sv_reload": "admin",
	"sv_restart": "owner", "quit": "owner", "exit": "owner", "sv_stop": "owner", "sv_ban_address": "admin",
	"sv_vpn": "viewer", "sv_vpn_check": "moderator", "sv_vpn_fetch": "admin",
}

// commandError is a refusal of the API's kind.
type commandError struct {
	code, message string
}

// words splits a command line, a word in quotes being one word.
func words(line string) []string {
	var out []string
	var b strings.Builder
	quoted, any := false, false
	for _, r := range line {
		switch {
		case r == '"':
			quoted, any = !quoted, true
		case r == ' ' && !quoted:
			if b.Len() > 0 || any {
				out = append(out, b.String())
				b.Reset()
				any = false
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 || any {
		out = append(out, b.String())
	}
	return out
}

// command runs a command for a role: its lines, and whether it succeeded
// (a refusal of the role: code in the returned error).
func (g *game) command(line, role, origin string) ([]string, bool) {
	lines, ok, _ := g.commandCode(line, role, origin)
	return lines, ok
}

func (g *game) commandCode(line, role, origin string) ([]string, bool, *commandError) {
	w := words(line)
	if len(w) == 0 {
		return nil, false, &commandError{"bad_request", "no command"}
	}
	name := strings.ToLower(w[0])
	needed, known := commandRoles[name]
	if !known {
		if role == "owner" {
			return []string{"(the game's console: " + line + ")"}, true, nil
		}
		return []string{"not a command of the server's: " + name}, false, &commandError{"not_found", "not a command of the server's"}
	}
	if (name == "sv_stop" || name == "sv_ban_address") && !options.capable ||
		strings.HasPrefix(name, "sv_vpn") && !vpnCapable() {
		return []string{"unknown command " + name}, false, &commandError{"not_found", "not a command"}
	}
	if roles[role] < roles[needed] {
		return []string{name + " needs the " + needed + " role"}, false, &commandError{"forbidden", "needs the " + needed + " role"}
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, w[0]))
	switch name {
	case "help":
		var list []string
		for c, r := range commandRoles {
			if roles[role] >= roles[r] {
				list = append(list, c)
			}
		}
		return []string{strings.Join(list, " ")}, true, nil
	case "sv_status":
		g.mu.Lock()
		s := g.status(role)
		g.mu.Unlock()
		if len(w) > 1 && w[1] == "--json" {
			data, _ := json.Marshal(s)
			return []string{string(data)}, true, nil
		}
		return []string{fmt.Sprintf("%s: %s, %s on %s, %d of %d players", s["name"], s["state"], s["gametype"], s["map"],
			s["players"], s["max_players"])}, true, nil
	case "sv_players":
		g.mu.Lock()
		defer g.mu.Unlock()
		var out []string
		for _, p := range g.players {
			out = append(out, fmt.Sprintf("%d %s team %d score %d", p.Slot, p.Name, p.Team, p.Score))
		}
		if len(out) == 0 {
			out = []string{"no players"}
		}
		return out, true, nil
	case "sv_rotation":
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.rotationTexts(), true, nil
	case "sv_version":
		return []string{"OpenCE " + Build + " (fakehalo)"}, true, nil
	case "sv_maps":
		return []string{"bloodgulch", "sidewinder", "prisoner"}, true, nil
	case "say", "sv_say":
		if rest == "" {
			return []string{"say <text>"}, false, nil
		}
		logLine("info", "chat", "(the host) "+strings.Trim(rest, `"`))
		return []string{"said: " + strings.Trim(rest, `"`)}, true, nil
	case "tell":
		return []string{"told"}, true, nil
	case "kick", "sv_kick", "ban", "sv_ban", "mute", "unmute":
		if len(w) < 2 {
			return []string{name + " <player>"}, false, nil
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		p := g.findPlayer(w[1])
		if p == nil {
			return []string{"no player " + w[1]}, false, &commandError{"not_found", "no such player"}
		}
		switch name {
		case "kick", "sv_kick":
			g.leave(p, "kicked")
			g.emit("moderation", "kick", map[string]any{"name": p.Name, "by": origin})
			return []string{p.Name + " kicked by " + origin}, true, nil
		case "mute":
			p.Muted = true
			g.emit("moderation", "mute", map[string]any{"name": p.Name, "by": origin})
			return []string{p.Name + " muted"}, true, nil
		case "unmute":
			p.Muted = false
			g.emit("moderation", "unmute", map[string]any{"name": p.Name, "by": origin})
			return []string{p.Name + " unmuted"}, true, nil
		}
		b := ban{When: time.Now().UTC().Format("2006-01-02 15:04:05"), IP: p.Address, HWID: p.HWID, Players: p.Name,
			DiscordID: p.DiscordID, DiscordName: p.DiscordName, DiscordVerified: p.DiscordVerified}
		if len(w) > 2 {
			if d, ok := duration(w[2]); ok {
				until := time.Now().Add(d).Unix()
				b.Until = &until
				b.Reason = strings.Join(w[3:], " ")
			} else {
				b.Reason = strings.Join(w[2:], " ")
			}
		}
		if role == "moderator" && (b.Until == nil || *b.Until > time.Now().Add(24*time.Hour+time.Minute).Unix()) {
			return []string{"a moderator's ban is a day at most"}, false, nil
		}
		g.bans = append(g.bans, b)
		g.writeBans()
		g.leave(p, "banned")
		g.emit("moderation", "ban", map[string]any{"name": p.Name, "by": origin, "reason": b.Reason, "until": b.Until})
		return []string{p.Name + " banned"}, true, nil
	case "sv_ban_address":
		if len(w) < 2 {
			return []string{"sv_ban_address <address|hardware id|discord:<id>> [<duration>] [<reason>]"}, false, nil
		}
		b := ban{When: time.Now().UTC().Format("2006-01-02 15:04:05")}
		if id, ok := strings.CutPrefix(w[1], "discord:"); ok && linkCapable() {
			if !discordDigits(id) {
				return []string{"a Discord account's id is up to 20 digits"}, false, nil
			}
			b.DiscordID, b.DiscordVerified = id, true
		} else if strings.Count(w[1], ".") == 3 || strings.Contains(w[1], ":") {
			b.IP = w[1]
		} else {
			b.HWID = w[1]
		}
		if len(w) > 2 {
			if d, ok := duration(w[2]); ok {
				until := time.Now().Add(d).Unix()
				b.Until = &until
				b.Reason = strings.Join(w[3:], " ")
			} else {
				b.Reason = strings.Join(w[2:], " ")
			}
		}
		n := g.addBan(b, origin)
		return []string{fmt.Sprintf("ban %d added", n)}, true, nil
	case "unban", "sv_unban":
		g.mu.Lock()
		defer g.mu.Unlock()
		g.readBans()
		for i, b := range g.bans {
			if len(w) > 1 && (strconv.Itoa(i) == w[1] || b.IP == w[1] || b.HWID == w[1]) {
				g.bans = append(g.bans[:i], g.bans[i+1:]...)
				g.writeBans()
				g.emit("moderation", "unban", map[string]any{"name": b.Players, "by": origin})
				return []string{"unbanned"}, true, nil
			}
		}
		return []string{"no such ban"}, false, &commandError{"not_found", "no such ban"}
	case "sv_bans":
		var out []string
		g.mu.Lock()
		for _, b := range g.bansJSON() {
			out = append(out, fmt.Sprint(b))
		}
		g.mu.Unlock()
		return out, true, nil
	case "sv_map_next", "sv_next":
		g.mu.Lock()
		if g.state == "playing" {
			g.endGame("next")
		}
		g.mu.Unlock()
		return []string{"next"}, true, nil
	case "sv_end_game":
		g.mu.Lock()
		if g.state == "playing" {
			g.endGame("command")
		}
		g.mu.Unlock()
		return []string{"ended"}, true, nil
	case "sv_map":
		if len(w) < 2 {
			return []string{"sv_map <map> [<gametype>]"}, false, nil
		}
		text := w[1]
		if len(w) > 2 {
			text += ":" + w[2]
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		e := g.parseRotation(text)[0]
		if e.Rejected != "" {
			return []string{e.Rejected}, false, nil
		}
		g.override = &e
		if g.state == "playing" {
			g.endGame("command")
		}
		return []string{"next: " + e.Text}, true, nil
	case "sv_rotation_set":
		code, message := g.setRotation(spec.RotationEntries(rest))
		if code != "" {
			return []string{message}, false, nil
		}
		return []string{"the rotation is set"}, true, nil
	case "sv_set", "sv_name", "sv_maxplayers", "sv_public", "sv_motd", "sv_password":
		setting, value := "", ""
		switch name {
		case "sv_set":
			if len(w) < 2 {
				return []string{"sv_set <setting> <value>"}, false, nil
			}
			setting, value = w[1], strings.TrimSpace(strings.TrimPrefix(rest, w[1]))
		case "sv_name":
			setting, value = "server.name", rest
		case "sv_maxplayers":
			setting, value = "server.max_players", rest
		case "sv_public":
			setting, value = "server.public", rest
		case "sv_motd":
			setting, value = "server.motd", rest
		case "sv_password":
			setting, value = "server.password", rest
		}
		code, message := g.st.set(setting, strings.Trim(value, `"`), role)
		switch code {
		case "":
			return []string{setting + " = " + value}, true, nil
		case "pinned", "forbidden":
			return []string{message}, false, &commandError{code, message}
		}
		return []string{message}, false, nil
	case "sv_vpn":
		g.mu.Lock()
		v := g.vpnJSON()
		g.mu.Unlock()
		return []string{fmt.Sprintf("vpn policy %s, fail %s, held %v, lists %d, refused %v", v["policy"], v["fail"], v["held"],
			len(v["lists"].([]map[string]any)), v["refused"])}, true, nil
	case "sv_vpn_check":
		if len(w) < 2 {
			return []string{"sv_vpn_check <address>"}, false, nil
		}
		g.mu.Lock()
		listed, by := g.vpnCheck(w[1])
		g.mu.Unlock()
		if !listed {
			return []string{w[1] + " is not refused " + by}, true, nil
		}
		return []string{w[1] + " is refused (" + by + ")"}, true, nil
	case "sv_vpn_fetch":
		return []string{"fetching the masters' netlists"}, true, nil
	case "sv_reload":
		g.reload()
		return []string{"reloaded"}, true, nil
	case "sv_restart", "sv_stop", "quit", "exit":
		mode := "now"
		if len(w) > 1 {
			mode = strings.ToLower(w[1])
		}
		restart := name == "sv_restart"
		if mode == "cancel" {
			g.mu.Lock()
			g.drain = nil
			g.emit("server", "drain", map[string]any{"after": "cancel", "restart": false, "notice": ""})
			g.mu.Unlock()
			return []string{"the drain is cancelled"}, true, nil
		}
		if (mode == "game" || mode == "empty") && options.capable {
			notice := strings.Trim(strings.Join(w[2:], " "), `"`)
			g.mu.Lock()
			g.drain = &drainState{After: mode, Notice: notice, Restart: restart, Since: time.Now().Unix()}
			g.emit("server", "drain", map[string]any{"after": mode, "restart": restart, "notice": notice})
			g.checkDrain()
			g.mu.Unlock()
			if notice != "" {
				logLine("info", "chat", "(the host) "+notice)
			}
			return []string{"draining: stops once " + mode}, true, nil
		}
		code := 0
		if restart {
			code = 5
		}
		g.exit <- code
		return []string{"stopping"}, true, nil
	}
	return []string{"(done)"}, true, nil
}

func (g *game) addBan(b ban, by string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.readBans()
	g.bans = append(g.bans, b)
	g.writeBans()
	for _, p := range append([]*player(nil), g.players...) {
		if (b.IP != "" && p.Address == b.IP) || (b.HWID != "" && p.HWID == b.HWID) ||
			(b.DiscordVerified && p.DiscordVerified && b.DiscordID == p.DiscordID) {
			g.leave(p, "banned")
		}
	}
	g.emit("moderation", "ban", map[string]any{"name": b.Players, "by": by, "reason": b.Reason, "until": b.Until})
	return len(g.bans) - 1
}

func (g *game) setRotation(entries []string) (string, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	parsed := g.parseRotation(strings.Join(entries, ","))
	for _, e := range parsed {
		if e.Rejected != "" {
			return "rejected", e.Text + ": " + e.Rejected
		}
	}
	if len(parsed) == 0 {
		return "rejected", "no entries"
	}
	g.rotation = parsed
	g.st.set("server.rotation", strings.Join(entries, ", "), "owner")
	return "", ""
}

func (g *game) findPlayer(text string) *player {
	if n, err := strconv.Atoi(text); err == nil {
		for _, p := range g.players {
			if p.Slot == n {
				return p
			}
		}
		return nil
	}
	var found *player
	for _, p := range g.players {
		if strings.HasPrefix(strings.ToLower(p.Name), strings.ToLower(text)) {
			if found != nil {
				return nil
			}
			found = p
		}
	}
	return found
}

// discordDigits tells whether a text is a Discord account's id: up to 20
// digits.
func discordDigits(text string) bool {
	if text == "" || len(text) > 20 {
		return false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func duration(text string) (time.Duration, bool) {
	if text == "forever" {
		return 0, false
	}
	units := map[byte]time.Duration{'s': time.Second, 'm': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}
	if len(text) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(text[:len(text)-1])
	unit, ok := units[text[len(text)-1]]
	if err != nil || !ok {
		return 0, false
	}
	return time.Duration(n) * unit, true
}
