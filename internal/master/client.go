package master

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Official is the official master, built into the game: asked when
// masters.txt names none.
var Official = Master{URL: "https://master.opencommunityedition.com",
	PublicKey: "ed25519:89f4eda71229f11f3538679f49ef7fb0c2b407b380093f7f4fe02105b92225aa"}

// Master is a line of masters.txt.
type Master struct {
	URL       string
	PublicKey string
}

// KeyID is the master's key id, which the requests to it sign.
func (m Master) KeyID() string {
	public, err := ParsePublicKey(m.PublicKey)
	if err != nil {
		return ""
	}
	return KeyID(public)
}

// ParseMasters reads masters.txt: one master a line, its URL and its
// public key; '#' starts a comment; the first 4 count. A file that names
// none leaves the official master.
func ParseMasters(text string) ([]Master, error) {
	var out []Master
	scanner := bufio.NewScanner(strings.NewReader(text))
	n := 0
	for scanner.Scan() {
		n++
		line := scanner.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 || !(strings.HasPrefix(fields[0], "https://") || strings.HasPrefix(fields[0], "http://")) {
			return nil, fmt.Errorf("line %d: a master is its https:// URL and its ed25519: key", n)
		}
		if _, err := ParsePublicKey(fields[1]); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if len(out) < 4 {
			out = append(out, Master{URL: strings.TrimRight(fields[0], "/"), PublicKey: strings.ToLower(fields[1])})
		}
	}
	if len(out) == 0 {
		out = []Master{Official}
	}
	return out, nil
}

// LoadMasters reads a masters.txt file ("" for none: the official master).
func LoadMasters(path string) ([]Master, error) {
	if path == "" {
		return []Master{Official}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseMasters(string(data))
}

// Me is a master's answer to GET /v1/me.
type Me struct {
	OK          bool         `json:"ok"`
	KeyID       string       `json:"key_id"`
	Status      string       `json:"status"`
	Publisher   *Publisher   `json:"publisher"`
	Application *Application `json:"application,omitempty"`
	MasterTime  int64        `json:"master_time"`
	// a refusal's code and message (unknown_key, key_revoked, clock...)
	Error *Error `json:"error,omitempty"`
}

// Error is the protocol's error.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Publisher is a publisher as a master sees it.
type Publisher struct {
	ID      any         `json:"id"`
	Name    string      `json:"name"`
	Since   any         `json:"since"`
	Status  string      `json:"status"`
	Quota   int         `json:"quota"`
	Origin  string      `json:"origin"`
	Keys    []MeKey     `json:"keys"`
	Servers []MeServer  `json:"servers"`
	Extra   interface{} `json:"-"`
}

// MeKey is one of the publisher's keys.
type MeKey struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	Status    string `json:"status"`
	Updated   int64  `json:"updated"`
	LastUsed  int64  `json:"last_used"`
}

// MeServer is the master's record of one of the publisher's servers.
type MeServer struct {
	ID           string `json:"id"`
	Instance     string `json:"instance"`
	KeyID        string `json:"key_id"`
	Listed       bool   `json:"listed"`
	Probe        string `json:"probe"`
	Reachable    bool   `json:"reachable"`
	Hidden       bool   `json:"hidden"`
	HiddenReason string `json:"hidden_reason,omitempty"`
	Trusted      bool   `json:"trusted"`
	Updated      int64  `json:"updated"`
	Name         string `json:"name"`
	Players      int    `json:"players"`
	MaxPlayers   int    `json:"max_players"`
}

// Application is a key's application still under review.
type Application struct {
	ID      any    `json:"id"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Created any    `json:"created"`
}

// Live is how many of the publisher's servers the master has live.
func (m *Me) Live() int {
	if m == nil || m.Publisher == nil {
		return 0
	}
	return len(m.Publisher.Servers)
}

// Words is the Masters page's sentence for an answer: what the status
// means and what to do.
func (m *Me) Words() string {
	if m == nil {
		return "not asked yet"
	}
	if m.Error != nil {
		switch m.Error.Code {
		case "unknown_key":
			return "this master does not know the key: apply with this key"
		case "key_revoked":
			return "the key is revoked on this master: make another and apply with it"
		case "clock":
			return "the master refused the request's time: this machine's clock is wrong"
		case "tls_required":
			return "the master needs its https:// URL"
		}
		return m.Error.Code + ": " + m.Error.Message
	}
	switch m.Status {
	case "approved", "active":
		if m.Publisher != nil {
			return fmt.Sprintf("approved: %d of %d servers live", m.Live(), m.Publisher.Quota)
		}
		return "approved"
	case "pending":
		return "the application waits for review: keep the servers publishing, they are listed once it is approved"
	case "denied":
		if m.Application != nil && m.Application.Reason != "" {
			return "the application was denied: " + m.Application.Reason
		}
		return "the application was denied"
	case "suspended":
		return "the publisher is suspended: ask the master's approvers why"
	case "revoked":
		return "the publisher is revoked"
	}
	return m.Status
}

// Client asks the masters, signing as a publisher.
type Client struct {
	HTTP *http.Client
	Now  func() time.Time
	// User-Agent
	Agent string
}

// Me signs GET /v1/me to a master with a publisher key. A refusal of the
// protocol's (403 unknown_key, key_revoked...) is an answer with Error set,
// not an error: the page shows it.
func (c *Client) Me(ctx context.Context, m Master, key ed25519.PrivateKey) (*Me, error) {
	masterKeyID := m.KeyID()
	if masterKeyID == "" {
		return nil, errors.New("the master's key in masters.txt does not parse")
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	path := "/v1/me"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL+path, nil)
	if err != nil {
		return nil, err
	}
	// (the signed path is the URL's path: a master served under a path signs it whole)
	signedPath := request.URL.EscapedPath()
	request.Header.Set("Authorization", Authorization(key, masterKeyID, http.MethodGet, signedPath, nil, now(), NewNonce()))
	request.Header.Set("Accept", "application/json")
	if c.Agent != "" {
		request.Header.Set("User-Agent", c.Agent)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var me Me
	if err := json.Unmarshal(body, &me); err != nil {
		return nil, fmt.Errorf("%s answered %s, not JSON", m.URL, response.Status)
	}
	if response.StatusCode != http.StatusOK && me.Error == nil {
		return nil, fmt.Errorf("%s answered %s", m.URL, response.Status)
	}
	return &me, nil
}

// Proof is the proof of key an application asks for: the publisher key's
// Ed25519 signature of the nonce's text, in hexadecimal (what
// `halo --server --sign <nonce>` prints).
func Proof(key ed25519.PrivateKey, nonce string) string {
	return hex.EncodeToString(ed25519.Sign(key, []byte(strings.TrimSpace(nonce))))
}
