// Package serverapi is a client of the dedicated server's HTTP API and RCON
// (docs/dedicated_server.md in the game's repository): the one place that
// knows the server's paths, shapes and codes. The agent signs in to each of
// its instances' web ports with it; the control plane reaches an external
// instance with it directly.
package serverapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client signs in to a server's web port and calls its API.
type Client struct {
	// http://127.0.0.1:8101
	Base     string
	Password string
	// who signs in, in the server's log
	By   string
	HTTP *http.Client

	mu      sync.Mutex
	token   string
	role    string
	expires time.Time
}

// Error is the server's refusal.
type Error struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
	// the command's lines (failed), the rotation's rejected entries, the
	// current name (changed), the gametype file's line (invalid)
	Lines    []string          `json:"-"`
	Rejected []RejectedEntry   `json:"rejected,omitempty"`
	Name     string            `json:"name,omitempty"`
	Line     int               `json:"line,omitempty"`
	Raw      json.RawMessage   `json:"-"`
	Extra    map[string]string `json:"-"`
}

// RejectedEntry is a rotation entry the server refused.
type RejectedEntry struct {
	Index  int    `json:"index"`
	Entry  string `json:"entry"`
	Reason string `json:"reason"`
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return fmt.Sprintf("%s (%d)", e.Code, e.Status)
}

// AsError reads a server's refusal from an error.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 25 * time.Second}
}

// Role is the session's role ("" before the sign-in).
func (c *Client) Role() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.role
}

// Token is the session's token, for the RCON's auth (signing in first if
// there is none).
func (c *Client) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	token, expires := c.token, c.expires
	c.mu.Unlock()
	if token != "" && time.Until(expires) > time.Hour {
		return token, nil
	}
	if err := c.Login(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token, nil
}

// Login signs in (POST /api/login).
func (c *Client) Login(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{"password": c.Password, "by": c.By})
	status, answer, err := c.raw(ctx, http.MethodPost, "/api/login", body, "")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return refusal(status, answer)
	}
	var session struct {
		Token   string `json:"token"`
		Role    string `json:"role"`
		Expires int64  `json:"expires"`
	}
	if err := json.Unmarshal(answer, &session); err != nil || session.Token == "" {
		return fmt.Errorf("the server's sign-in answered no token")
	}
	c.mu.Lock()
	c.token, c.role = session.Token, session.Role
	c.expires = time.Unix(session.Expires, 0)
	if session.Expires == 0 {
		c.expires = time.Now().Add(12 * time.Hour)
	}
	c.mu.Unlock()
	return nil
}

// Forget drops the session (a password change, say).
func (c *Client) Forget() {
	c.mu.Lock()
	c.token, c.role = "", ""
	c.mu.Unlock()
}

func (c *Client) raw(ctx context.Context, method, path string, body []byte, token string) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.Base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.client().Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	return response.StatusCode, answer, err
}

func refusal(status int, body []byte) error {
	var answer struct {
		Error *Error   `json:"error"`
		Lines []string `json:"lines"`
	}
	if json.Unmarshal(body, &answer) == nil && answer.Error != nil {
		answer.Error.Status, answer.Error.Lines, answer.Error.Raw = status, answer.Lines, body
		return answer.Error
	}
	return &Error{Status: status, Code: "http_" + strconv.Itoa(status), Message: strings.TrimSpace(string(body)), Raw: body}
}

// Do sends a request with the session (signing in when there is none, and
// again once when the session is refused) and returns the answer's status
// and body as they are: the agent relays them.
func (c *Client) Do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	token, err := c.Token(ctx)
	if err != nil {
		return 0, nil, err
	}
	status, answer, err := c.raw(ctx, method, path, body, token)
	if err == nil && status == http.StatusUnauthorized {
		c.Forget()
		if token, err = c.Token(ctx); err != nil {
			return 0, nil, err
		}
		status, answer, err = c.raw(ctx, method, path, body, token)
	}
	return status, answer, err
}

// Call sends a request and decodes a 2xx answer into out (nil: none); any
// other answer is an *Error.
func (c *Client) Call(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	status, answer, err := c.Do(ctx, method, path, body)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return refusal(status, answer)
	}
	if out != nil {
		return json.Unmarshal(answer, out)
	}
	return nil
}

// Healthz asks GET /healthz: the state, and whether it answered 200.
func (c *Client) Healthz(ctx context.Context) (string, bool, error) {
	status, body, err := c.raw(ctx, http.MethodGet, "/healthz", nil, "")
	if err != nil {
		return "", false, err
	}
	var answer struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(body, &answer)
	return answer.State, status == http.StatusOK, nil
}

// Status is the server's status (GET /api/status, the RCON's status event).
type Status struct {
	OK             bool     `json:"ok"`
	Time           int64    `json:"time"`
	Name           string   `json:"name"`
	State          string   `json:"state"`
	Build          string   `json:"build"`
	NetworkVersion int      `json:"network_version"`
	Uptime         int64    `json:"uptime"`
	GamesHosted    int      `json:"games_hosted"`
	Map            string   `json:"map"`
	Level          string   `json:"level"`
	Gametype       string   `json:"gametype"`
	Variant        string   `json:"variant"`
	Teams          bool     `json:"teams"`
	Players        int      `json:"players"`
	MaxPlayers     int      `json:"max_players"`
	Machines       int      `json:"machines"`
	Countdown      int      `json:"countdown"`
	GameSeconds    int      `json:"game_seconds"`
	TeamScores     []int    `json:"team_scores"`
	FrameMsMax     float64  `json:"frame_ms_max"`
	FrameMsAvg     float64  `json:"frame_ms_avg"`
	Listing        Listing  `json:"listing"`
	Rotation       Rotation `json:"rotation"`
	Masters        []Master `json:"masters"`
	Update         struct {
		Available bool   `json:"available"`
		Build     string `json:"build"`
	} `json:"update"`
	// the game's identity fields (the fleet's proposal 7.5) and drain (7.3)
	PID            int      `json:"pid,omitempty"`
	Started        int64    `json:"started,omitempty"`
	Instance       string   `json:"instance,omitempty"`
	Region         string   `json:"region,omitempty"`
	TunnelPort     int      `json:"tunnel_port,omitempty"`
	WebPort        int      `json:"web_port,omitempty"`
	PublisherKeyID string   `json:"publisher_key_id,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty"`
	Drain          *Drain   `json:"drain,omitempty"`
}

// Listing is the status's listing.
type Listing struct {
	Online   bool   `json:"online"`
	Public   bool   `json:"public"`
	Password bool   `json:"password"`
	Invite   string `json:"invite"`
}

// Rotation is the status's rotation.
type Rotation struct {
	Index int    `json:"index"`
	Count int    `json:"count"`
	Next  *Entry `json:"next"`
}

// Entry is a rotation entry.
type Entry struct {
	Text     string         `json:"text"`
	Map      string         `json:"map"`
	Gametype string         `json:"gametype"`
	Rules    map[string]any `json:"rules,omitempty"`
	Rejected *string        `json:"rejected"`
}

// Master is a master server's answer, as the server saw it.
type Master struct {
	URL     string `json:"url"`
	Listed  bool   `json:"listed"`
	Probe   string `json:"probe"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Updated int64  `json:"updated"`
}

// Drain is a drain under way (7.3).
type Drain struct {
	After   string `json:"after"`
	Restart bool   `json:"restart"`
	Since   int64  `json:"since"`
}

// Has tells whether the server has a capability.
func (s *Status) Has(capability string) bool {
	for _, c := range s.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// ParseStatus reads a status object.
func ParseStatus(data []byte) (*Status, error) {
	var s Status
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Status asks GET /api/status.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var s Status
	return &s, c.Call(ctx, http.MethodGet, "/api/status", nil, &s)
}

// Player is a player of /api/players.
type Player struct {
	Slot    int    `json:"slot"`
	Name    string `json:"name"`
	Team    int    `json:"team"`
	Score   int    `json:"score"`
	Kills   int    `json:"kills"`
	Deaths  int    `json:"deaths"`
	Assists int    `json:"assists"`
	Ping    *int   `json:"ping"`
	Machine int    `json:"machine"`
	Joined  int64  `json:"joined"`
	Muted   bool   `json:"muted"`
	Address string `json:"address,omitempty"`
	HWID    string `json:"hwid,omitempty"`
}

// Players asks GET /api/players.
func (c *Client) Players(ctx context.Context) ([]Player, error) {
	var answer struct {
		Players []Player `json:"players"`
	}
	err := c.Call(ctx, http.MethodGet, "/api/players", nil, &answer)
	return answer.Players, err
}

// SettingInfo is a setting of GET /api/settings.
type SettingInfo struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Value   any      `json:"value"`
	Default any      `json:"default"`
	Comment string   `json:"comment"`
	Env     *string  `json:"env"`
	Pinned  bool     `json:"pinned"`
	Secret  bool     `json:"secret"`
	Set     bool     `json:"set"`
	Choices []string `json:"choices"`
	Min     *float64 `json:"min"`
	Max     *float64 `json:"max"`
}

// Settings asks GET /api/settings.
func (c *Client) Settings(ctx context.Context) ([]SettingInfo, error) {
	var answer struct {
		Settings []SettingInfo `json:"settings"`
	}
	err := c.Call(ctx, http.MethodGet, "/api/settings", nil, &answer)
	return answer.Settings, err
}

// SettingsResult is PUT /api/settings' answer.
type SettingsResult struct {
	Applied []string `json:"applied"`
	Errors  []struct {
		Name    string `json:"name"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// PutSettings changes settings (each changed or refused on its own).
func (c *Client) PutSettings(ctx context.Context, values map[string]any) (*SettingsResult, error) {
	var answer SettingsResult
	err := c.Call(ctx, http.MethodPut, "/api/settings", values, &answer)
	return &answer, err
}

// RotationAnswer is GET /api/rotation's answer.
type RotationAnswer struct {
	Index   int     `json:"index"`
	Entries []Entry `json:"entries"`
	Next    *Entry  `json:"next"`
}

// Rotation asks GET /api/rotation.
func (c *Client) Rotation(ctx context.Context) (*RotationAnswer, error) {
	var answer RotationAnswer
	return &answer, c.Call(ctx, http.MethodGet, "/api/rotation", nil, &answer)
}

// PutRotation replaces the rotation: all its entries, or none (400
// rejected, with the entries refused).
func (c *Client) PutRotation(ctx context.Context, entries []string) error {
	return c.Call(ctx, http.MethodPut, "/api/rotation", map[string]any{"entries": entries}, nil)
}

// GametypeFile is a gametype file of GET /api/gametypes.
type GametypeFile struct {
	Name        string  `json:"name"`
	Base        string  `json:"base"`
	Description string  `json:"description"`
	Error       *string `json:"error"`
}

// Gametypes is GET /api/gametypes' answer.
type Gametypes struct {
	Builtin []string        `json:"builtin"`
	Schema  json.RawMessage `json:"schema"`
	Files   []GametypeFile  `json:"files"`
}

// Gametypes asks GET /api/gametypes.
func (c *Client) Gametypes(ctx context.Context) (*Gametypes, error) {
	var answer Gametypes
	return &answer, c.Call(ctx, http.MethodGet, "/api/gametypes", nil, &answer)
}

// Gametype reads a gametype file's text.
func (c *Client) Gametype(ctx context.Context, name string) (string, error) {
	var answer struct {
		Text string `json:"text"`
	}
	err := c.Call(ctx, http.MethodGet, "/api/gametypes/"+url.PathEscape(name), nil, &answer)
	return answer.Text, err
}

// PutGametype writes a gametype file (400 invalid with its line).
func (c *Client) PutGametype(ctx context.Context, name, text string) error {
	return c.Call(ctx, http.MethodPut, "/api/gametypes/"+url.PathEscape(name), map[string]string{"text": text}, nil)
}

// DeleteGametype removes a gametype file.
func (c *Client) DeleteGametype(ctx context.Context, name string) error {
	return c.Call(ctx, http.MethodDelete, "/api/gametypes/"+url.PathEscape(name), nil, nil)
}

// MapInfo is a map of GET /api/maps.
type MapInfo struct {
	Name  string `json:"name"`
	Level string `json:"level"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

// Maps asks GET /api/maps.
func (c *Client) Maps(ctx context.Context) ([]MapInfo, error) {
	var answer struct {
		Maps []MapInfo `json:"maps"`
	}
	err := c.Call(ctx, http.MethodGet, "/api/maps", nil, &answer)
	return answer.Maps, err
}

// Ban is a line of bans.txt, as GET /api/bans gives it.
type Ban struct {
	N       int    `json:"n"`
	When    string `json:"when"`
	IP      string `json:"ip"`
	HWID    string `json:"hwid"`
	Players string `json:"players"`
	Reason  string `json:"reason"`
	Until   *int64 `json:"until"`
}

// Bans asks GET /api/bans.
func (c *Client) Bans(ctx context.Context) ([]Ban, error) {
	var answer struct {
		Bans []Ban `json:"bans"`
	}
	err := c.Call(ctx, http.MethodGet, "/api/bans", nil, &answer)
	return answer.Bans, err
}

// DeleteBan removes bans.txt's line n.
func (c *Client) DeleteBan(ctx context.Context, n int) error {
	return c.Call(ctx, http.MethodDelete, "/api/bans/"+strconv.Itoa(n), nil, nil)
}

// AddBan bans an address or hardware id without a present player (the
// game's POST /api/bans, 7.2): the new line's number.
func (c *Client) AddBan(ctx context.Context, ip, hwid, names, duration, reason string) (int, error) {
	body := map[string]string{}
	for k, v := range map[string]string{"ip": ip, "hwid": hwid, "names": names, "duration": duration, "reason": reason} {
		if v != "" {
			body[k] = v
		}
	}
	var answer struct {
		N int `json:"n"`
	}
	err := c.Call(ctx, http.MethodPost, "/api/bans", body, &answer)
	return answer.N, err
}

// Command runs a command (POST /api/command): its lines, and a refusal's.
func (c *Client) Command(ctx context.Context, line string) ([]string, error) {
	var answer struct {
		Lines []string `json:"lines"`
	}
	err := c.Call(ctx, http.MethodPost, "/api/command", map[string]string{"line": line}, &answer)
	if e, ok := AsError(err); ok {
		return e.Lines, err
	}
	return answer.Lines, err
}

// LogLine is a line of /api/log.
type LogLine struct {
	N     int64  `json:"n"`
	Time  int64  `json:"time"`
	Level string `json:"level"`
	Cat   string `json:"cat"`
	Text  string `json:"text"`
}

// Log reads the log's lines after n.
func (c *Client) Log(ctx context.Context, after int64, limit int) ([]LogLine, int64, error) {
	var answer struct {
		Lines []LogLine `json:"lines"`
		Next  int64     `json:"next"`
	}
	err := c.Call(ctx, http.MethodGet, fmt.Sprintf("/api/log?after=%d&limit=%d", after, limit), nil, &answer)
	return answer.Lines, answer.Next, err
}

// Events reads the events after seq.
func (c *Client) Events(ctx context.Context, after int64) ([]json.RawMessage, int64, error) {
	var answer struct {
		Events []json.RawMessage `json:"events"`
		Next   int64             `json:"next"`
	}
	err := c.Call(ctx, http.MethodGet, fmt.Sprintf("/api/events?after=%d", after), nil, &answer)
	return answer.Events, answer.Next, err
}

// Stop asks a drain or a stop (the game's POST /api/stop, 7.3, with the
// owner role).
func (c *Client) Stop(ctx context.Context, after, notice string, restart bool) error {
	return c.Call(ctx, http.MethodPost, "/api/stop", map[string]any{"after": after, "notice": notice, "restart": restart}, nil)
}
