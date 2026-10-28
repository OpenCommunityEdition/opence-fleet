package core

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/serverapi"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// The game's VPN policy (its vpn capability): a server refuses machines
// from a VPN's, a Tor exit's or a datacenter's address, as the netlists it
// fetches from its masters say, unless they show a pass of a linked Discord
// account. The fleet shows what each server holds, counts its refusals
// (join_refused) and alerts when a server cannot keep a list.

// RefusalWords is a join_refused reason in words.
func RefusalWords(reason string) string {
	switch reason {
	case "vpn_link_required":
		return "a VPN, no linked Discord account"
	case "vpn_blocked":
		return "a VPN (the server blocks them)"
	case "vpn_link_invalid":
		return "a Discord link that could not be checked"
	case "vpn_link_too_new":
		return "a Discord account too young"
	case "vpn_link_in_use":
		return "a Discord account already playing"
	}
	return reason
}

// RefusedSince counts a server's joins refused for the VPN policy since a
// time (its join_refused events the fleet kept).
func (f *Fleet) RefusedSince(ctx context.Context, slug string, since time.Time) int64 {
	n, _ := f.Store.CountEventsSince(ctx, db.CountEventsSinceParams{Slug: slug, Type: "join_refused", Time: since.UnixMilli()})
	return n
}

// VPN asks a server its VPN policy and the lists it holds (GET /api/vpn).
func (f *Fleet) VPN(ctx context.Context, slug string) (*serverapi.VPN, error) {
	var answer serverapi.VPN
	if err := f.Call(ctx, slug, http.MethodGet, "/api/vpn", nil, &answer); err != nil {
		return nil, err
	}
	return &answer, nil
}

// CheckVPN asks a server whether it refuses an address as a VPN's (POST
// /api/vpn/check).
func (f *Fleet) CheckVPN(ctx context.Context, slug, ip string) (*serverapi.VPNCheck, error) {
	if a, err := netip.ParseAddr(ip); err != nil || !a.Is4() {
		return nil, Refuse(http.StatusBadRequest, "schema", "%q is not an IPv4 address", ip)
	}
	var answer serverapi.VPNCheck
	if err := f.Call(ctx, slug, http.MethodPost, "/api/vpn/check", map[string]string{"ip": ip}, &answer); err != nil {
		return nil, err
	}
	return &answer, nil
}

// how often the alerts ask a server its lists (they age by the hour)
const vpnPollEvery = 5 * time.Minute

// a server without a list this long, or whose lists are all this old,
// cannot reach its masters
const (
	vpnNoListFor = 2 * time.Hour
	vpnListAge   = 48 * time.Hour
)

// vpnPolls are the times each server's lists were last asked.
type vpnPolls struct {
	sync.Mutex
	at map[string]time.Time
}

// pollVPN asks a server with the vpn capability its lists (every
// vpnPollEvery at most, in the background: nothing waits on a node) and
// checks them.
func (f *Fleet) pollVPN(slug string, now time.Time) {
	f.vpnPolls.Lock()
	if now.Sub(f.vpnPolls.at[slug]) < vpnPollEvery {
		f.vpnPolls.Unlock()
		return
	}
	f.vpnPolls.at[slug] = now
	f.vpnPolls.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		v, err := f.VPN(ctx, slug)
		if err != nil {
			f.Log.Debug("a server's VPN lists were not read", "slug", slug, "err", err)
			return
		}
		f.checkVPN(ctx, slug, v, f.Now())
	}()
}

// checkVPN raises vpn_list for a server that holds no list for
// vpnNoListFor, or whose masters' lists are all older than vpnListAge (it
// cannot reach its masters, and refuses or lets in what it should not as
// the list ages); resolved once it holds a list that is not.
func (f *Fleet) checkVPN(ctx context.Context, slug string, v *serverapi.VPN, now time.Time) {
	if v.Policy == "off" {
		// (a server that refuses nobody for a VPN needs no list)
		f.holds("vpn:"+slug, false, now, 0)
		f.resolveAlert("vpn_list", slug)
		return
	}
	meanwhile := "every machine is let in meanwhile (server.vpn_fail open)"
	if v.Fail == "closed" {
		meanwhile = "every machine from the internet is refused meanwhile (server.vpn_fail closed)"
	}
	none := !v.Held
	// (the masters' lists alone: a file of the operator's, server.vpn_list,
	// is theirs to keep)
	newest := v.Newest()
	old := v.Held && newest > 0 && now.Sub(time.Unix(newest, 0)) > vpnListAge
	switch {
	case f.holds("vpn:"+slug, none, now, vpnNoListFor):
		f.raiseAlert(ctx, "vpn_list", slug, "no VPN list held for over 2 hours: it cannot fetch a netlist from its masters; "+
			meanwhile)
	case old:
		f.raiseAlert(ctx, "vpn_list", slug, fmt.Sprintf("its newest VPN list is %d days old (from %s): it cannot fetch "+
			"a newer netlist from its masters", int(now.Sub(time.Unix(newest, 0)).Hours()/24),
			time.Unix(newest, 0).In(f.Location()).Format("2006-01-02 15:04")))
	case !none:
		f.resolveAlert("vpn_list", slug)
	}
}
