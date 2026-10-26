package core

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/live"
	"github.com/OpenCommunityEdition/opence-fleet/internal/serverapi"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// the fleet's own sign-in name on its servers: a ban event by it is the
// fleet's own, not one to import
const fleetBy = "opence-fleet"

// BanInput is a fleet ban to make.
type BanInput struct {
	Address, HWID, Names, Reason string
	// 0: for ever
	Duration time.Duration
	Source   string
}

// BanLine is a fleet ban as a line of bans.txt, as the server writes one
// (network_distributed.c): tab-separated fields after the time, each kept
// to the server's lengths (it reads a line of 511 characters at most: a
// longer one would lose its until).
func BanLine(b db.FleetBan, now time.Time) string {
	field := func(text string, n int) string {
		text = strings.Map(func(r rune) rune {
			if r < 0x20 || r > 0x7e {
				return -1
			}
			return r
		}, text)
		if len(text) > n {
			text = text[:n]
		}
		if text == "" {
			return "none"
		}
		return text
	}
	reason := "fleet ban"
	if b.Reason != "" {
		reason += ": " + b.Reason
	}
	line := fmt.Sprintf("%s\tip=%s\thwid=%s\tdiscord_username=none\tdiscord_id=none\tplayers=%s\treason=%s",
		now.Format("2006-01-02 15:04:05"), field(b.Address, 64), field(b.Hwid, 64), field(b.Names, 95), field(reason, 95))
	if b.Until != nil {
		line += "\tuntil=" + strconv.FormatInt(*b.Until, 10)
	}
	return line
}

// duration is a ban's duration for the server ("" for ever).
func durationText(until *int64, now time.Time) string {
	if until == nil {
		return ""
	}
	seconds := *until - now.Unix()
	if seconds < 60 {
		seconds = 60
	}
	return strconv.FormatInt(seconds/60, 10) + "m"
}

// Bans lists the fleet's bans (the active ones, or all of them).
func (f *Fleet) Bans(ctx context.Context, all bool) ([]db.FleetBan, error) {
	if all {
		return f.Store.ListBans(ctx, 1000)
	}
	return f.Store.ListActiveBans(ctx, &[]int64{f.Now().Unix()}[0])
}

// CreateBan makes a fleet ban and applies it to every server.
func (f *Fleet) CreateBan(ctx context.Context, actor string, in BanInput) (int64, error) {
	in.Address, in.HWID = strings.TrimSpace(in.Address), strings.TrimSpace(in.HWID)
	if in.Address == "" && in.HWID == "" {
		return 0, Refuse(http.StatusBadRequest, "schema", "a ban names an address or a hardware id")
	}
	if in.Address != "" {
		if _, err := netip.ParseAddr(in.Address); err != nil {
			return 0, Refuse(http.StatusBadRequest, "schema", "%q is not an address", in.Address)
		}
	}
	var until *int64
	if in.Duration > 0 {
		until = ptrInt(f.Now().Add(in.Duration).Unix())
	}
	id, err := f.Store.CreateBan(ctx, db.CreateBanParams{Hwid: in.HWID, Address: in.Address, Names: in.Names, Reason: in.Reason,
		Until: until, CreatedBy: actor, CreatedAt: f.Now().Unix(), SourceSlug: in.Source})
	if err != nil {
		return 0, err
	}
	f.Audit(ctx, actor, "ban.create", strconv.FormatInt(id, 10), map[string]any{"names": in.Names, "reason": in.Reason,
		"until": until, "source": in.Source})
	f.Live.AddFeed(live.FeedItem{Slug: in.Source, Kind: "mod", Event: "fleet_ban", Text: fmt.Sprintf("%s: fleet ban of %s (%s)",
		actor, firstNonEmpty(in.Names, in.Address, in.HWID), in.Reason)})
	go f.applyBan(context.Background(), id, in.Source)
	return id, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// BanPlayer bans a present player: the server's own ban (its name checked,
// 409 changed when another player holds the slot), and a fleet ban of its
// address and hardware id for the other servers.
func (f *Fleet) BanPlayer(ctx context.Context, actor, slug string, slot int, name string, duration time.Duration,
	reason string, fleet bool) error {
	players, err := f.Players(ctx, slug)
	if err != nil {
		return err
	}
	var target *serverapi.Player
	for i := range players {
		if players[i].Slot == slot {
			target = &players[i]
		}
	}
	if target == nil {
		return Refuse(http.StatusNotFound, "not_found", "no player in slot %d", slot)
	}
	if name != "" && target.Name != name {
		return Refuse(http.StatusConflict, "changed", "the slot holds %s now: nothing done", target.Name)
	}
	body := map[string]any{"name": target.Name, "reason": byReason(reason, actor)}
	if duration > 0 {
		body["duration"] = strconv.FormatInt(int64(duration/time.Minute), 10) + "m"
	}
	if err := f.Call(ctx, slug, http.MethodPost, fmt.Sprintf("/api/players/%d/ban", slot), body, nil); err != nil {
		return err
	}
	f.Audit(ctx, actor, "player.ban", slug, map[string]any{"name": target.Name, "reason": reason, "duration": duration.String()})
	if fleet && (target.Address != "" || target.HWID != "") {
		_, err := f.CreateBan(ctx, actor, BanInput{Address: target.Address, HWID: target.HWID, Names: target.Name, Reason: reason,
			Duration: duration, Source: slug})
		return err
	}
	return nil
}

// byReason is a reason that names the fleet's user, so that the server's
// log and the fleet's audit agree.
func byReason(reason, actor string) string {
	who := strings.TrimPrefix(actor, "user:")
	if reason == "" {
		return "by " + who
	}
	return reason + " (by " + who + ")"
}

// applyBan applies a fleet ban to every server but its source: by the
// game's POST /api/bans where it has it (7.2); else the server's own ban of
// the machine when present, else a line in its bans.txt (the agent's);
// a server whose node is offline waits (pending), and gets it at its next
// connect.
func (f *Fleet) applyBan(ctx context.Context, id int64, source string) {
	ban, err := f.Store.GetBan(ctx, id)
	if err != nil {
		return
	}
	instances, err := f.Store.ListInstances(ctx)
	if err != nil {
		return
	}
	for _, i := range instances {
		if i.Slug == source {
			f.Store.UpsertBanState(ctx, db.UpsertBanStateParams{BanID: id, Slug: i.Slug, State: "source", AppliedAt: ptrInt(f.Now().Unix())})
			continue
		}
		f.applyBanTo(ctx, ban, i)
	}
}

func (f *Fleet) applyBanTo(ctx context.Context, ban db.FleetBan, i db.Instance) {
	state, detail := "applied", ""
	l := f.Live.Instance(i.Slug)
	online := f.Hub.Online(i.NodeID) || i.Kind == "external"
	switch {
	case !online:
		state, detail = "pending", "the node is offline"
	case l != nil && l.State == "up" && l.Has("ban_address"):
		if _, err := f.addServerBan(ctx, i.Slug, ban); err != nil {
			state, detail = "failed", err.Error()
		} else {
			detail = "POST /api/bans"
		}
	default:
		// (the machine on this server now: the server's own ban)
		done := false
		if l != nil && l.State == "up" {
			if players, err := f.Players(ctx, i.Slug); err == nil {
				for _, p := range players {
					if (ban.Address != "" && p.Address == ban.Address) || (ban.Hwid != "" && p.HWID == ban.Hwid) {
						body := map[string]any{"name": p.Name, "reason": "fleet ban: " + ban.Reason}
						if d := durationText(ban.Until, f.Now()); d != "" {
							body["duration"] = d
						}
						if err := f.Call(ctx, i.Slug, http.MethodPost, fmt.Sprintf("/api/players/%d/ban", p.Slot), body, nil); err == nil {
							done, detail = true, "banned "+p.Name+" there"
						}
					}
				}
			}
		}
		if !done && i.Kind == "managed" {
			if _, err := f.Control(ctx, i.Slug, agentproto.Control{Action: agentproto.BanFile, Line: BanLine(ban, f.Now())}); err != nil {
				state, detail = "pending", err.Error()
			} else {
				detail = "bans.txt"
			}
		}
	}
	f.Store.UpsertBanState(ctx, db.UpsertBanStateParams{BanID: ban.ID, Slug: i.Slug, State: state,
		AppliedAt: ptrInt(f.Now().Unix()), Detail: detail})
}

func (f *Fleet) addServerBan(ctx context.Context, slug string, ban db.FleetBan) (int, error) {
	body := map[string]string{"reason": "fleet ban: " + ban.Reason, "names": ban.Names}
	if ban.Address != "" {
		body["ip"] = ban.Address
	}
	if ban.Hwid != "" {
		body["hwid"] = ban.Hwid
	}
	if d := durationText(ban.Until, f.Now()); d != "" {
		body["duration"] = d
	}
	var answer struct {
		N int `json:"n"`
	}
	err := f.Call(ctx, slug, http.MethodPost, "/api/bans", body, &answer)
	return answer.N, err
}

// ApplyPendingBans applies the bans that waited for a node's servers.
func (f *Fleet) ApplyPendingBans(ctx context.Context, node int64) {
	states, err := f.Store.ListBanStates(ctx)
	if err != nil {
		return
	}
	pending := map[string]map[int64]bool{}
	for _, s := range states {
		if s.State == "pending" {
			if pending[s.Slug] == nil {
				pending[s.Slug] = map[int64]bool{}
			}
			pending[s.Slug][s.BanID] = true
		}
	}
	instances, _ := f.Store.ListInstancesByNode(ctx, node)
	active, _ := f.Bans(ctx, false)
	for _, i := range instances {
		for _, b := range active {
			if pending[i.Slug][b.ID] {
				f.applyBanTo(ctx, b, i)
			}
		}
	}
}

// Unban removes a fleet ban, and its lines from every server's bans.txt.
func (f *Fleet) Unban(ctx context.Context, actor string, id int64) error {
	ban, err := f.Store.GetBan(ctx, id)
	if err != nil {
		return NotFound("the ban")
	}
	if err := f.Store.RemoveBan(ctx, db.RemoveBanParams{RemovedAt: ptrInt(f.Now().Unix()), RemovedBy: actor, ID: id}); err != nil {
		return err
	}
	f.Audit(ctx, actor, "ban.remove", strconv.FormatInt(id, 10), map[string]any{"names": ban.Names})
	go func() {
		ctx := context.Background()
		instances, _ := f.Store.ListInstances(ctx)
		for _, i := range instances {
			f.unbanOn(ctx, i.Slug, ban)
		}
		f.Store.DeleteBanStates(ctx, id)
	}()
	return nil
}

func (f *Fleet) unbanOn(ctx context.Context, slug string, ban db.FleetBan) {
	var answer struct {
		Bans []serverapi.Ban `json:"bans"`
	}
	if err := f.Call(ctx, slug, http.MethodGet, "/api/bans", nil, &answer); err != nil {
		return
	}
	var lines []int
	for _, b := range answer.Bans {
		if (ban.Address != "" && b.IP == ban.Address) || (ban.Hwid != "" && b.HWID == ban.Hwid) {
			lines = append(lines, b.N)
		}
	}
	// (the last first: the numbers after a removed line move)
	sort.Sort(sort.Reverse(sort.IntSlice(lines)))
	for _, n := range lines {
		f.Call(ctx, slug, http.MethodDelete, fmt.Sprintf("/api/bans/%d", n), nil, nil)
	}
}

// importServerBan makes a fleet ban of one a server made itself (its own
// console or page), and pushes it to the others.
func (f *Fleet) importServerBan(ctx context.Context, slug string, ev eventFields) {
	if ev.By == fleetBy || strings.Contains(ev.Reason, "fleet ban") || strings.Contains(ev.Reason, "(by ") {
		return
	}
	var answer struct {
		Bans []serverapi.Ban `json:"bans"`
	}
	if err := f.Call(ctx, slug, http.MethodGet, "/api/bans", nil, &answer); err != nil {
		return
	}
	for i := len(answer.Bans) - 1; i >= 0; i-- {
		b := answer.Bans[i]
		if ev.Name != "" && !strings.Contains(b.Players, ev.Name) {
			continue
		}
		address, hwid := b.IP, b.HWID
		if hwid == "none" {
			hwid = ""
		}
		active, _ := f.Bans(ctx, false)
		for _, existing := range active {
			if (address != "" && existing.Address == address) || (hwid != "" && existing.Hwid == hwid) {
				return
			}
		}
		var duration time.Duration
		if b.Until != nil {
			duration = time.Until(time.Unix(*b.Until, 0))
			if duration <= 0 {
				return
			}
		}
		f.CreateBan(ctx, "server:"+slug, BanInput{Address: address, HWID: hwid, Names: b.Players,
			Reason: firstNonEmpty(b.Reason, ev.Reason, "banned on "+slug), Duration: duration, Source: slug})
		return
	}
}

// checkJoin bans on sight a machine of the fleet's list that joins (a ban
// made while it was away): by the event's address and hardware id (7.4),
// else the server's player list.
func (f *Fleet) checkJoin(ctx context.Context, slug string, ev eventFields) {
	active, err := f.Bans(ctx, false)
	if err != nil || len(active) == 0 {
		return
	}
	address, hwid := ev.Address, ev.HWID
	if address == "" && hwid == "" {
		players, err := f.Players(ctx, slug)
		if err != nil {
			return
		}
		for _, p := range players {
			if p.Slot == ev.Slot {
				address, hwid = p.Address, p.HWID
			}
		}
	}
	for _, b := range active {
		if (b.Address != "" && b.Address == address) || (b.Hwid != "" && b.Hwid == hwid) {
			body := map[string]any{"name": ev.Name, "reason": "fleet ban: " + b.Reason}
			if d := durationText(b.Until, f.Now()); d != "" {
				body["duration"] = d
			}
			if err := f.Call(ctx, slug, http.MethodPost, fmt.Sprintf("/api/players/%d/ban", ev.Slot), body, nil); err == nil {
				f.Audit(ctx, "auto", "ban.on_sight", slug, map[string]any{"name": ev.Name, "ban": b.ID})
				f.Live.AddFeed(live.FeedItem{Slug: slug, Kind: "mod", Event: "ban", Text: "fleet ban on sight: " + ev.Name})
			}
			return
		}
	}
}

// ImportBans reads a bans.txt into the fleet's list.
func (f *Fleet) ImportBans(ctx context.Context, actor, text string) (int, error) {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := map[string]string{}
		for _, part := range strings.FieldsFunc(line, func(r rune) bool { return r == '\t' }) {
			if k, v, ok := strings.Cut(part, "="); ok {
				fields[k] = v
			}
		}
		if fields["ip"] == "none" {
			fields["ip"] = ""
		}
		if fields["hwid"] == "none" {
			fields["hwid"] = ""
		}
		if fields["ip"] == "" && fields["hwid"] == "" {
			continue
		}
		var duration time.Duration
		if until, err := strconv.ParseInt(fields["until"], 10, 64); err == nil {
			duration = time.Until(time.Unix(until, 0))
			if duration <= 0 {
				continue
			}
		}
		if _, err := f.CreateBan(ctx, actor, BanInput{Address: fields["ip"], HWID: fields["hwid"], Names: fields["players"],
			Reason: fields["reason"], Duration: duration, Source: "import"}); err == nil {
			n++
		}
	}
	return n, nil
}

// Players is a server's players (with addresses for the fleet: the
// server's log_addresses, or the owner's role).
func (f *Fleet) Players(ctx context.Context, slug string) ([]serverapi.Player, error) {
	var answer struct {
		Players []serverapi.Player `json:"players"`
	}
	err := f.Call(ctx, slug, http.MethodGet, "/api/players", nil, &answer)
	return answer.Players, err
}
