// Package httpx holds what the fleet's HTTP handlers share: who a request
// comes from (believing X-Forwarded-* only from a trusted proxy), whether
// it came over TLS, JSON answers and the error body (the dedicated server's
// shape, {"ok":false,"error":{"code","message"}}). It is the master server's
// package, re-homed.
package httpx

import (
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/OpenCommunityEdition/opence-fleet/internal/netaddr"
)

// Clients works out where requests come from.
type Clients struct {
	Proxies []netip.Prefix
	// plain HTTP is taken from anywhere (FLEET_INSECURE_HTTP: a LAN, a test)
	Insecure bool
}

// Client is a request's origin.
type Client struct {
	IP netip.Addr
	// over TLS: the fleet's own, or a trusted proxy's (X-Forwarded-Proto)
	Secure bool
	// from loopback, not through a trusted proxy
	Local bool
	// from a trusted proxy
	Proxied bool
	// the fleet takes plain HTTP from anywhere (FLEET_INSECURE_HTTP)
	Insecure bool
}

func (c *Clients) trusted(ip netip.Addr) bool {
	for _, p := range c.Proxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// ParseForwarded reads one entry of X-Forwarded-For: an address alone
// ("203.0.113.5", "2001:db8::1"), with a port ("203.0.113.5:4711",
// "[2001:db8::1]:4711"), or in brackets ("[2001:db8::1]").
func ParseForwarded(entry string) (netip.Addr, bool) {
	entry = strings.Trim(strings.TrimSpace(entry), `"`)
	if address, err := netip.ParseAddr(entry); err == nil {
		return address.Unmap(), true
	}
	if address, err := netip.ParseAddrPort(entry); err == nil {
		return address.Addr().Unmap(), true
	}
	if strings.HasPrefix(entry, "[") && strings.HasSuffix(entry, "]") {
		if address, err := netip.ParseAddr(entry[1 : len(entry)-1]); err == nil {
			return address.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

func (c *Clients) remote(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

// Of returns a request's client.
func (c *Clients) Of(r *http.Request) Client {
	ip, ok := c.remote(r)
	if !ok {
		return Client{}
	}
	client := Client{IP: ip, Secure: r.TLS != nil, Insecure: c.Insecure}
	if !c.trusted(ip) {
		client.Local = ip.IsLoopback()
		return client
	}
	client.Proxied = true
	// the rightmost address that is not a trusted proxy's (an entry the
	// proxy wrote that does not parse leaves the last one that did)
	forwarded := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(forwarded) - 1; i >= 0; i-- {
		candidate, ok := ParseForwarded(forwarded[i])
		if !ok {
			break
		}
		client.IP = candidate
		if !c.trusted(candidate) {
			break
		}
	}
	if !client.Secure {
		protos := strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")
		client.Secure = strings.EqualFold(strings.TrimSpace(protos[len(protos)-1]), "https")
	}
	return client
}

// Host is the host a request was sent to: the trusted proxy's
// X-Forwarded-Host (its last entry) when it sends one, else the Host header.
func (c *Clients) Host(r *http.Request) string {
	if ip, ok := c.remote(r); ok && c.trusted(ip) {
		if forwarded := r.Header.Values("X-Forwarded-Host"); len(forwarded) > 0 {
			entries := strings.Split(strings.Join(forwarded, ","), ",")
			if host := strings.TrimSpace(entries[len(entries)-1]); host != "" {
				return host
			}
		}
	}
	return r.Host
}

// Key is what the client's requests are counted under (netaddr.Key: an
// IPv6 address's /64).
func (c Client) Key() string { return netaddr.Key(c.IP) }

// HSTS adds Strict-Transport-Security to every answer to a request that
// came over TLS (the fleet's own, or a trusted proxy's).
func (c *Clients) HSTS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.Of(r).Secure {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// Protected tells whether a request may carry secrets: over TLS, or from
// loopback directly (or anyhow, when the fleet was told to take plain HTTP).
func (c Client) Protected() bool { return c.Secure || c.Local || c.Insecure }

// JSON writes a JSON answer.
func JSON(w http.ResponseWriter, status int, value any) {
	body, _ := json.Marshal(value)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// ErrorBody is the error body.
type ErrorBody struct {
	OK    bool        `json:"ok"`
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is an error's code and message, and the field at fault.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

// Fail writes an error.
func Fail(w http.ResponseWriter, status int, code, message, field string) {
	JSON(w, status, ErrorBody{Error: ErrorDetail{Code: code, Message: message, Field: field}})
}

// Secure adds the headers every HTML page has.
func Secure(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; img-src 'self' data:; "+
		"frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	// (same-origin, not no-referrer: under no-referrer a browser sends its
	// own forms' posts with Origin: null, which SameOrigin must then refuse)
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
}
