package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestForwardedEntries(t *testing.T) {
	for entry, want := range map[string]string{
		"203.0.113.5":            "203.0.113.5",
		" 203.0.113.5 ":          "203.0.113.5",
		"203.0.113.5:4711":       "203.0.113.5",
		"2001:db8::1":            "2001:db8::1",
		"[2001:db8::1]":          "2001:db8::1",
		"[2001:db8::1]:4711":     "2001:db8::1",
		`"[2001:db8::1]:4711"`:   "2001:db8::1",
		"::ffff:203.0.113.5":     "203.0.113.5",
		"[::ffff:203.0.113.5]:1": "203.0.113.5",
	} {
		got, ok := ParseForwarded(entry)
		if !ok || got.String() != want {
			t.Errorf("%q: %v %v, want %s", entry, got, ok, want)
		}
	}
	for _, bad := range []string{"", "unknown", "_hidden", "[2001:db8::1", "example.org:80"} {
		if _, ok := ParseForwarded(bad); ok {
			t.Errorf("%q read", bad)
		}
	}
}

func TestClients(t *testing.T) {
	c := &Clients{Proxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.1/32")}}
	request := func(remote, forwarded, proto, host string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if forwarded != "" {
			r.Header.Set("X-Forwarded-For", forwarded)
		}
		if proto != "" {
			r.Header.Set("X-Forwarded-Proto", proto)
		}
		if host != "" {
			r.Header.Set("X-Forwarded-Host", host)
		}
		return r
	}
	// through the proxy: the forwarded address, with a port or in brackets
	for forwarded, want := range map[string]string{
		"198.51.100.7:5000":            "198.51.100.7",
		"192.0.2.1, [2001:db8::7]:443": "2001:db8::7",
		"[2001:db8::7]:443, 10.0.0.1":  "2001:db8::7",
		"garbage":                      "10.0.0.1",
	} {
		client := c.Of(request("10.0.0.1:4000", forwarded, "https", ""))
		if client.IP.String() != want || !client.Proxied || !client.Secure || client.Local {
			t.Errorf("%q: %+v, want %s", forwarded, client, want)
		}
	}
	// a header from anyone else is not believed
	client := c.Of(request("203.0.113.9:4000", "198.51.100.7", "https", ""))
	if client.IP.String() != "203.0.113.9" || client.Secure || client.Proxied {
		t.Errorf("untrusted: %+v", client)
	}
	if !c.Of(request("127.0.0.1:4000", "", "", "")).Protected() {
		t.Error("loopback directly is not protected")
	}
	// X-Forwarded-Host only from the proxy
	r := request("203.0.113.9:4000", "", "", "evil.example")
	r.Host = "master.example.org"
	if c.Host(r) != "master.example.org" {
		t.Errorf("untrusted X-Forwarded-Host believed: %s", c.Host(r))
	}
	r = request("10.0.0.1:4000", "", "", "public.example.org")
	if c.Host(r) != "public.example.org" {
		t.Errorf("the proxy's X-Forwarded-Host: %s", c.Host(r))
	}
	// the key of an IPv6 client is its /64
	if (Client{IP: netip.MustParseAddr("2001:db8:1:2::99")}).Key() != "2001:db8:1:2::/64" {
		t.Error("IPv6 key")
	}
}

func TestHSTS(t *testing.T) {
	c := &Clients{Proxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.1/32")}}
	handler := c.HSTS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, test := range []struct {
		remote, proto string
		want          bool
	}{{"10.0.0.1:1", "https", true}, {"10.0.0.1:1", "http", false}, {"203.0.113.9:1", "https", false},
		{"127.0.0.1:1", "", false}} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = test.remote
		if test.proto != "" {
			r.Header.Set("X-Forwarded-Proto", test.proto)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if got := w.Header().Get("Strict-Transport-Security") != ""; got != test.want {
			t.Errorf("%+v: HSTS %v", test, got)
		}
	}
}
