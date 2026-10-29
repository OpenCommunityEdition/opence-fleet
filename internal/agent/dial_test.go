package agent

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/config"
)

// A control plane behind a proxy that speaks HTTP/2 (Caddy's default) must
// still take the agent's WebSocket, after the agent's HTTP client has
// spoken h2 to it.
func TestDialThroughHTTP2(t *testing.T) {
	upgrader := websocket.Upgrader{Subprotocols: []string{agentproto.Subprotocol}}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent" {
			w.Write([]byte(r.Proto))
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conn.WriteMessage(websocket.TextMessage, []byte("hello"))
		conn.Close()
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	// (the client trusts the server's certificate, as FLEET_AGENT_CA_FILE
	// makes it, and speaks h2 once, which adds h2 to its TLS configuration)
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	transport.ForceAttemptHTTP2 = true
	client := &http.Client{Transport: transport}
	response, err := client.Get(server.URL + "/proto")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.ProtoMajor != 2 || !slices.Contains(transport.TLSClientConfig.NextProtos, "h2") {
		t.Fatalf("the client spoke %s, offering %v", response.Proto, transport.TLSClientConfig.NextProtos)
	}

	a := New(&config.AgentConfig{URL: server.URL, Token: "token"}, "test", nil, nil, client)
	dialer := a.dialer()
	if dialer.TLSClientConfig == transport.TLSClientConfig {
		t.Fatal("the dialer shares the transport's TLS configuration")
	}
	if !slices.Equal(dialer.TLSClientConfig.NextProtos, []string{"http/1.1"}) {
		t.Fatalf("the dialer offers %v", dialer.TLSClientConfig.NextProtos)
	}
	if !slices.Contains(transport.TLSClientConfig.NextProtos, "h2") {
		t.Fatal("the dialer changed the transport's TLS configuration")
	}
	if !strings.HasPrefix(a.websocketURL(), "wss://") {
		t.Fatal(a.websocketURL())
	}
	conn, _, err := dialer.DialContext(context.Background(), a.websocketURL(), nil)
	if err != nil {
		t.Fatalf("the upgrade failed through HTTP/2: %v", err)
	}
	defer conn.Close()
	if _, data, err := conn.ReadMessage(); err != nil || string(data) != "hello" {
		t.Fatalf("%q %v", data, err)
	}

	// (and without extra authorities, a dialer of its own, HTTP/1.1 too)
	plain := New(&config.AgentConfig{URL: server.URL}, "test", nil, nil, &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()})
	if got := plain.dialer().TLSClientConfig.NextProtos; !slices.Equal(got, []string{"http/1.1"}) {
		t.Fatalf("the plain dialer offers %v", got)
	}
}
