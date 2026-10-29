// Package core is the fleet's rules: nodes and their enrolment, the spec
// each node gets, instances and their allocation, profiles, groups, the
// library, releases, publisher keys and the masters, bans, jobs, schedules,
// alerts and the audit log. Every change goes through it with an actor
// ("user:<name>", "token:<label>", "schedule:<name>", "agent:<node>",
// "auto"), writes its audit row, and pushes the specs it changes to the
// nodes (package hub). It is also the hub's Sink: the agents' reports come
// here and become the live state (package live), events, games, samples,
// fleet bans on sight and alerts.
package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/config"
	"github.com/OpenCommunityEdition/opence-fleet/internal/crypt"
	"github.com/OpenCommunityEdition/opence-fleet/internal/hub"
	"github.com/OpenCommunityEdition/opence-fleet/internal/live"
	"github.com/OpenCommunityEdition/opence-fleet/internal/master"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// Version is the fleet's version (set by the build).
var Version = "dev"

// Fleet is the control plane's core.
type Fleet struct {
	Store   *store.Store
	Key     *crypt.Key
	Config  *config.ServeConfig
	Log     *slog.Logger
	Hub     *hub.Hub
	Live    *live.Live
	Masters []master.Master
	// the masters', the registry's and the webhooks' client
	HTTP *http.Client
	Now  func() time.Time
	// the public URL of the pages, for links in alerts ("" when unknown)
	PublicURL string

	jobs      *jobRunner
	alertsMu  sync.Mutex
	pushMu    sync.Mutex
	pushTimer map[int64]*time.Timer
	gamesMu   sync.Mutex
	games     map[string]int64
	// the builds an update has each node fetch before its waves
	prefetchMu  sync.Mutex
	prefetching map[int64]map[string]bool
	// the reports' state: which node owns each instance, the crash times,
	// the events' writer, the conditions that must last before they alert
	owners  *owners
	crashes *crashTimes
	events  *eventWriter
	lasting *lastingConditions
	// when each server's VPN lists were last asked (the vpn_list alert)
	vpnPolls *vpnPolls
	// the builds whose network version a server told
	learnMu sync.Mutex
	learnt  map[string]bool
	// each node's network counters at its last sample (the rates)
	netMu       sync.Mutex
	netReadings map[int64]netReading
}

// Error is a refusal a person reads: its code (for scripts) and words.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// Refuse makes a refusal.
func Refuse(status int, code, format string, args ...any) error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// AsError reads a refusal from an error.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// NotFound is a refusal of a thing that is not there.
func NotFound(what string) error {
	return Refuse(http.StatusNotFound, "not_found", "%s is not there", what)
}

// New makes the core.
func New(s *store.Store, key *crypt.Key, c *config.ServeConfig, log *slog.Logger, l *live.Live) *Fleet {
	f := &Fleet{Store: s, Key: key, Config: c, Log: log, Live: l, Now: time.Now,
		HTTP: &http.Client{Timeout: 30 * time.Second}, pushTimer: map[int64]*time.Timer{}, games: map[string]int64{},
		netReadings: map[int64]netReading{},
		owners:      &owners{nodes: map[string]int64{}}, crashes: &crashTimes{at: map[string][]time.Time{}},
		events:   &eventWriter{ch: make(chan db.InsertEventParams, 8192)},
		lasting:  &lastingConditions{since: map[string]time.Time{}},
		vpnPolls: &vpnPolls{at: map[string]time.Time{}}, learnt: map[string]bool{}}
	f.Hub = hub.New(f, log.With("part", "hub"))
	f.jobs = newJobRunner(f)
	f.PublicURL = c.PublicURL
	return f
}

// Token is a new random secret: n bytes in hexadecimal.
func Token(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Password is a new password of n characters from 62 letters and digits.
func Password(n int) string {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789abcdefghij"
	b := make([]byte, n)
	raw := make([]byte, n*2)
	rand.Read(raw)
	for i := range b {
		b[i] = alphabet[int(raw[i])%62]
	}
	return string(b)
}

// Audit writes an audit row (a failure is logged: the action happened).
func (f *Fleet) Audit(ctx context.Context, actor, action, target string, detail any) {
	text := ""
	switch d := detail.(type) {
	case nil:
	case string:
		text = d
	default:
		data, _ := json.Marshal(d)
		text = string(data)
	}
	if err := f.Store.InsertAudit(ctx, db.InsertAuditParams{Time: f.Now().Unix(), Actor: actor, Action: action,
		Target: target, Detail: text}); err != nil {
		f.Log.Error("an audit row was not written", "action", action, "err", err)
	}
}

// Setting is one of the fleet's own settings: its variable's value when the
// environment sets it, else the database's, else the default.
func (f *Fleet) Setting(ctx context.Context, name string) string {
	if env, ok := settingEnv[name]; ok && config.FromEnvironment(env) {
		return f.envSetting(name)
	}
	if v := f.Store.Setting(ctx, name); v != "" {
		return v
	}
	return settingDefaults[name]
}

// the settings the Settings page changes, and the variables that pin them
var settingEnv = map[string]string{"name": "FLEET_NAME", "masters": "FLEET_MASTERS_FILE", "events_days": "FLEET_EVENTS_DAYS",
	"log_addresses": "FLEET_LOG_ADDRESSES", "public_status": "FLEET_PUBLIC_STATUS", "timezone": "FLEET_TIMEZONE"}

var settingDefaults = map[string]string{"name": "OpenCE Fleet", "events_days": "30", "log_addresses": "true",
	"public_status": "false", "default_channel": "latest", "rolling_per_node": "2", "drain_deadline": "30m",
	"apply_mode": "live", "theme": "dark"}

func (f *Fleet) envSetting(name string) string {
	c := f.Config
	switch name {
	case "name":
		return c.Name
	case "masters":
		return c.MastersFile
	case "events_days":
		return fmt.Sprint(c.EventsDays)
	case "log_addresses":
		return fmt.Sprint(c.LogAddresses)
	case "public_status":
		return fmt.Sprint(c.PublicStatus)
	case "timezone":
		return c.Timezone.String()
	}
	return ""
}

// Pinned tells whether a setting is set by the environment.
func Pinned(name string) bool {
	env, ok := settingEnv[name]
	return ok && config.FromEnvironment(env)
}

// SetSetting changes one of the fleet's settings.
func (f *Fleet) SetSetting(ctx context.Context, actor, name, value string) error {
	if Pinned(name) {
		return Refuse(http.StatusConflict, "pinned", "%s is set by the environment (%s)", name, settingEnv[name])
	}
	if _, ok := settingDefaults[name]; !ok {
		if _, ok := settingEnv[name]; !ok {
			return Refuse(http.StatusBadRequest, "schema", "%s is not a setting", name)
		}
	}
	if err := f.Store.SetSetting(ctx, db.SetSettingParams{Name: name, Value: value}); err != nil {
		return err
	}
	f.Audit(ctx, actor, "settings.set", name, value)
	if name == "log_addresses" {
		f.PushAll(ctx)
	}
	return nil
}

// Name is the fleet's name.
func (f *Fleet) Name(ctx context.Context) string { return f.Setting(ctx, "name") }

// Location is the fleet's time zone.
func (f *Fleet) Location() *time.Location {
	if f.Config.Timezone != nil {
		return f.Config.Timezone
	}
	return time.UTC
}

// Run runs the core's background work until ctx ends: the jobs left
// running, the offline nodes, the samples, retention, the masters, the
// releases, the schedules and the alerts.
func (f *Fleet) Run(ctx context.Context) {
	go f.jobs.resume(ctx)
	go f.sampleLoop(ctx)
	go f.retentionLoop(ctx)
	go f.mastersLoop(ctx)
	go f.releasesLoop(ctx)
	go f.scheduleLoop(ctx)
	go f.alertsLoop(ctx)
	<-ctx.Done()
}

func (f *Fleet) retentionLoop(ctx context.Context) {
	for {
		days := f.Config.EventsDays
		if n := f.Setting(ctx, "events_days"); n != "" {
			fmt.Sscan(n, &days)
		}
		if err := f.Store.Prune(ctx, f.Now(), max(days, 1), max(f.Config.SamplesDays, 1)); err != nil {
			f.Log.Warn("retention", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

func ptrString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ptrInt(n int64) *int64 { return &n }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}
