// Package config reads the fleet's options: each is an environment variable
// (FLEET_* for the control plane, FLEET_AGENT_* for a node's agent, FLEET_URL
// and FLEET_TOKEN for the command line's client) and a command-line flag, the
// flag winning. README.md's "Options" tables repeat Serve and Agent.
package config

import (
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Option describes one option, for the flags, the environment and the help.
type Option struct {
	Env, Flag, Help, Default string
	// a secret's variable is not made its flag's default, so the help never
	// shows it
	Secret bool
}

// Serve is every option of `opence-fleet serve`, in the help's order.
var Serve = []Option{
	{Env: "FLEET_NAME", Flag: "name", Help: "the fleet's name, shown on its pages (else set on the Settings page)"},
	{Env: "FLEET_PUBLIC_URL", Flag: "public-url", Help: "the fleet's public HTTPS origin (https://fleet.example.org), for the agents' one-liner and links"},
	{Env: "FLEET_LISTEN", Flag: "listen", Help: "the HTTP listener's address", Default: "127.0.0.1:8070"},
	{Env: "FLEET_DB", Flag: "db", Help: "the SQLite database file (default: fleet.db in FLEET_DATA)"},
	{Env: "FLEET_DATA", Flag: "data", Help: "the folder of the fleet's files: the library's blobs, the releases' zips, fleet.key, bin/", Default: "fleet-data"},
	{Env: "FLEET_SECRET_KEY", Flag: "secret-key", Help: "the key that encrypts secrets at rest (made if missing; default: fleet.key in FLEET_DATA)"},
	{Env: "FLEET_TRUSTED_PROXIES", Flag: "trusted-proxies", Help: "reverse proxies (addresses or CIDRs, commas) whose X-Forwarded-* are believed"},
	{Env: "FLEET_TLS_CERT", Flag: "tls-cert", Help: "a certificate file, to serve HTTPS directly"},
	{Env: "FLEET_TLS_KEY", Flag: "tls-key", Help: "the certificate's key file"},
	{Env: "FLEET_MASTERS_FILE", Flag: "masters-file", Help: "a masters.txt naming the master servers the Masters page asks (default: the official master)"},
	{Env: "FLEET_IMAGE", Flag: "image", Help: "the dedicated server's image (or a mirror of it)", Default: "ghcr.io/opencommunityedition/opence-server"},
	{Env: "FLEET_RELEASE_REPOSITORY", Flag: "release-repository", Help: "the game's GitHub repository, whose releases' halo-linux-release.zip the process runtime runs", Default: "OpenCommunityEdition/OpenCE"},
	{Env: "FLEET_RELEASE_CHECK_INTERVAL", Flag: "release-check-interval", Help: "how often latest is resolved", Default: "6h"},
	{Env: "FLEET_EVENTS_DAYS", Flag: "events-days", Help: "days the servers' events and games are kept", Default: "30"},
	{Env: "FLEET_SAMPLES_DAYS", Flag: "samples-days", Help: "days the players' samples (the sparklines) are kept", Default: "7"},
	{Env: "FLEET_LOG_ADDRESSES", Flag: "log-addresses", Help: "the servers log and show players' addresses and hardware ids to admins (HALO_SERVER_LOG_ADDRESSES), which fleet bans need", Default: "true"},
	{Env: "FLEET_PUBLIC_STATUS", Flag: "public-status", Help: "serve a public, read-only /status page of the servers", Default: "false"},
	{Env: "FLEET_METRICS", Flag: "metrics", Help: "serve /metrics (Prometheus) without a token", Default: "true"},
	{Env: "FLEET_LOG_LEVEL", Flag: "log-level", Help: "debug, info, warn or error", Default: "info"},
	{Env: "FLEET_UPDATES", Flag: "updates", Help: "notify (tell the owners of a new release; they update with a button), auto (install it at once) or off", Default: "notify"},
	{Env: "FLEET_UPDATE_URL", Flag: "update-url", Help: "the fleet's latest release, on GitHub's API (a fork's)", Default: "https://api.github.com/repos/OpenCommunityEdition/opence-fleet/releases/latest"},
	{Env: "FLEET_UPDATE_KEY", Flag: "update-key", Help: "the release key's public key, 64 hex digits (a fork's; default the project's)"},
	{Env: "FLEET_TIMEZONE", Flag: "timezone", Help: "the time zone of the schedules and pages (an IANA name; default the machine's)"},
	{Env: "FLEET_CA_FILE", Flag: "ca-file", Help: "extra certificate authorities (PEM) for the masters, the registry and webhooks"},
	{Env: "FLEET_SESSION_HOURS", Flag: "session-hours", Help: "how long a sign-in lasts", Default: "12"},
}

// Agent is every option of `opence-fleet agent`.
var Agent = []Option{
	{Env: "FLEET_AGENT_URL", Flag: "url", Help: "the control plane's URL (https://fleet.example.org)"},
	{Env: "FLEET_AGENT_TOKEN", Flag: "node-token", Help: "the node token (made by the enrolment)", Secret: true},
	{Env: "FLEET_AGENT_TOKEN_FILE", Flag: "node-token-file", Help: "a file holding the node token"},
	{Env: "FLEET_AGENT_DIR", Flag: "dir", Help: "the node folder: data/, instances/, releases/, the state file", Default: "/var/lib/opence-fleet"},
	{Env: "FLEET_AGENT_NAME", Flag: "name", Help: "the node's name (default: the host name)"},
	{Env: "FLEET_AGENT_RUNTIME", Flag: "runtime", Help: "docker (each server a container of the server image) or process (child processes of the release)", Default: "docker"},
	{Env: "FLEET_AGENT_DOCKER_HOST", Flag: "docker-host", Help: "the Docker Engine API's socket", Default: "unix:///var/run/docker.sock"},
	{Env: "FLEET_AGENT_DATA_ROOT", Flag: "data-root", Help: "an existing game data folder (maps/, custom_maps/, gametypes/) instead of the synced library"},
	{Env: "FLEET_AGENT_LOG_LEVEL", Flag: "log-level", Help: "debug, info, warn or error", Default: "info"},
	{Env: "FLEET_AGENT_CA_FILE", Flag: "ca-file", Help: "extra certificate authorities (PEM) for the control plane and the registry"},
}

// Client is the options of the command line's client commands.
var Client = []Option{
	{Env: "FLEET_URL", Flag: "url", Help: "the control plane's URL", Default: "http://127.0.0.1:8070"},
	{Env: "FLEET_TOKEN", Flag: "token", Help: "an API token (made on the Team page)", Secret: true},
}

// Values are the flags' values, by variable.
type Values map[string]*string

// Flags adds options to a flag set, each one's default from its variable.
func Flags(set *flag.FlagSet, options []Option) Values {
	values := Values{}
	for _, o := range options {
		value := o.Default
		if env, ok := os.LookupEnv(o.Env); ok && !o.Secret {
			value = env
		}
		help := o.Help + " (" + o.Env + ")"
		values[o.Env] = set.String(o.Flag, value, help)
	}
	return values
}

// Get is an option's value: its flag's, else (a secret's) its variable's.
func (v Values) Get(env string) string {
	p := v[env]
	if p == nil {
		return strings.TrimSpace(os.Getenv(env))
	}
	if value := strings.TrimSpace(*p); value != "" {
		return value
	}
	for _, list := range [][]Option{Serve, Agent, Client} {
		for _, o := range list {
			if o.Env == env && o.Secret {
				return strings.TrimSpace(os.Getenv(env))
			}
		}
	}
	return ""
}

// FromEnvironment tells whether an option is set by its variable (the pages
// show such a setting as pinned rather than editable).
func FromEnvironment(env string) bool {
	value, ok := os.LookupEnv(env)
	return ok && strings.TrimSpace(value) != ""
}

// Duration reads a duration: Go's ("90s", "6h") or a number of seconds.
func Duration(text string) (time.Duration, error) {
	if n, err := strconv.Atoi(text); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(text)
}

// Bool reads true or false (1, yes, on; 0, no, off).
func Bool(text string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off", "":
		return false, nil
	}
	return false, fmt.Errorf("%q is not true or false", text)
}

// ServeConfig is the control plane's options, read.
type ServeConfig struct {
	Name, PublicURL, Listen, DB, Data, SecretKey string
	TrustedProxies                               []netip.Prefix
	TLSCert, TLSKey, MastersFile                 string
	Image, ReleaseRepository                     string
	ReleaseCheckInterval                         time.Duration
	EventsDays, SamplesDays                      int
	LogAddresses, PublicStatus, Metrics          bool
	LogLevel, Updates, UpdateURL, UpdateKey      string
	Timezone                                     *time.Location
	CAFile                                       string
	SessionHours                                 int
}

// LoadServe reads the control plane's options.
func LoadServe(v Values) (*ServeConfig, error) {
	c := &ServeConfig{
		Name: v.Get("FLEET_NAME"), PublicURL: strings.TrimRight(v.Get("FLEET_PUBLIC_URL"), "/"),
		Listen: v.Get("FLEET_LISTEN"), DB: v.Get("FLEET_DB"), Data: v.Get("FLEET_DATA"),
		SecretKey: v.Get("FLEET_SECRET_KEY"), TLSCert: v.Get("FLEET_TLS_CERT"), TLSKey: v.Get("FLEET_TLS_KEY"),
		MastersFile: v.Get("FLEET_MASTERS_FILE"), Image: strings.TrimRight(v.Get("FLEET_IMAGE"), "/"),
		ReleaseRepository: v.Get("FLEET_RELEASE_REPOSITORY"), LogLevel: strings.ToLower(v.Get("FLEET_LOG_LEVEL")),
		Updates: strings.ToLower(v.Get("FLEET_UPDATES")), UpdateURL: v.Get("FLEET_UPDATE_URL"),
		UpdateKey: v.Get("FLEET_UPDATE_KEY"), CAFile: v.Get("FLEET_CA_FILE"),
	}
	var errs []error
	check := func(err error, env string) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", env, err))
		}
	}
	var err error
	if c.Data == "" {
		c.Data = "."
	}
	if c.DB == "" {
		c.DB = filepath.Join(c.Data, "fleet.db")
	}
	if c.SecretKey == "" {
		c.SecretKey = filepath.Join(c.Data, "fleet.key")
	}
	c.TrustedProxies, err = Prefixes(v.Get("FLEET_TRUSTED_PROXIES"))
	check(err, "FLEET_TRUSTED_PROXIES")
	c.ReleaseCheckInterval, err = Duration(v.Get("FLEET_RELEASE_CHECK_INTERVAL"))
	check(err, "FLEET_RELEASE_CHECK_INTERVAL")
	if err == nil && c.ReleaseCheckInterval < 10*time.Minute {
		errs = append(errs, errors.New("FLEET_RELEASE_CHECK_INTERVAL: 10 minutes at least"))
	}
	c.EventsDays, err = strconv.Atoi(v.Get("FLEET_EVENTS_DAYS"))
	check(err, "FLEET_EVENTS_DAYS")
	c.SamplesDays, err = strconv.Atoi(v.Get("FLEET_SAMPLES_DAYS"))
	check(err, "FLEET_SAMPLES_DAYS")
	c.SessionHours, err = strconv.Atoi(v.Get("FLEET_SESSION_HOURS"))
	check(err, "FLEET_SESSION_HOURS")
	if err == nil && (c.SessionHours < 1 || c.SessionHours > 24*30) {
		errs = append(errs, errors.New("FLEET_SESSION_HOURS: 1 to 720"))
	}
	c.LogAddresses, err = Bool(v.Get("FLEET_LOG_ADDRESSES"))
	check(err, "FLEET_LOG_ADDRESSES")
	c.PublicStatus, err = Bool(v.Get("FLEET_PUBLIC_STATUS"))
	check(err, "FLEET_PUBLIC_STATUS")
	c.Metrics, err = Bool(v.Get("FLEET_METRICS"))
	check(err, "FLEET_METRICS")
	switch c.Updates {
	case "notify", "auto", "off":
	case "":
		c.Updates = "notify"
	default:
		errs = append(errs, errors.New("FLEET_UPDATES: notify, auto or off"))
	}
	// (the machine's zone unless one is named: a container's is UTC)
	c.Timezone = time.Local
	if tz := v.Get("FLEET_TIMEZONE"); tz != "" {
		c.Timezone, err = time.LoadLocation(tz)
		check(err, "FLEET_TIMEZONE")
		if c.Timezone == nil {
			c.Timezone = time.UTC
		}
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		errs = append(errs, errors.New("FLEET_TLS_CERT and FLEET_TLS_KEY go together"))
	}
	if c.PublicURL != "" && !strings.HasPrefix(c.PublicURL, "https://") && !strings.HasPrefix(c.PublicURL, "http://") {
		errs = append(errs, errors.New("FLEET_PUBLIC_URL: an http:// or https:// origin"))
	}
	if len(c.Name) > 64 {
		errs = append(errs, errors.New("FLEET_NAME: 64 characters at most"))
	}
	return c, errors.Join(errs...)
}

// AgentConfig is a node agent's options, read.
type AgentConfig struct {
	URL, Token, Dir, Name, Runtime, DockerHost, DataRoot, LogLevel, CAFile string
}

// LoadAgent reads the agent's options (the URL and token are checked by
// the agent: an enrolment has neither yet).
func LoadAgent(v Values) (*AgentConfig, error) {
	c := &AgentConfig{
		URL: strings.TrimRight(v.Get("FLEET_AGENT_URL"), "/"), Token: v.Get("FLEET_AGENT_TOKEN"),
		Dir: v.Get("FLEET_AGENT_DIR"), Name: v.Get("FLEET_AGENT_NAME"),
		Runtime: strings.ToLower(v.Get("FLEET_AGENT_RUNTIME")), DockerHost: v.Get("FLEET_AGENT_DOCKER_HOST"),
		DataRoot: v.Get("FLEET_AGENT_DATA_ROOT"), LogLevel: strings.ToLower(v.Get("FLEET_AGENT_LOG_LEVEL")),
		CAFile: v.Get("FLEET_AGENT_CA_FILE"),
	}
	var errs []error
	if file := v.Get("FLEET_AGENT_TOKEN_FILE"); file != "" && c.Token == "" {
		data, err := os.ReadFile(file)
		if err != nil {
			errs = append(errs, fmt.Errorf("FLEET_AGENT_TOKEN_FILE: %w", err))
		}
		c.Token = strings.TrimSpace(string(data))
	}
	switch c.Runtime {
	case "docker", "process":
	case "":
		c.Runtime = "docker"
	default:
		errs = append(errs, errors.New("FLEET_AGENT_RUNTIME: docker or process"))
	}
	if c.Dir == "" {
		errs = append(errs, errors.New("FLEET_AGENT_DIR: the node folder is needed"))
	}
	if c.Name == "" {
		c.Name, _ = os.Hostname()
	}
	c.Name = NodeName(c.Name)
	if c.Name == "" {
		errs = append(errs, errors.New("FLEET_AGENT_NAME: a name of a-z, 0-9 and '-'"))
	}
	if c.URL != "" && !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://") {
		errs = append(errs, errors.New("FLEET_AGENT_URL: an http:// or https:// URL"))
	}
	return c, errors.Join(errs...)
}

// NodeName makes a node's name of a host name: lower case, a-z, 0-9 and
// '-', 32 characters at most ("" when nothing is left).
func NodeName(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(text)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.' || r == ' ':
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
		if b.Len() >= 32 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// Prefixes reads a list of addresses and CIDRs (commas or spaces).
func Prefixes(text string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == ' ' }) {
		if strings.Contains(part, "/") {
			prefix, err := netip.ParsePrefix(part)
			if err != nil {
				return nil, err
			}
			out = append(out, prefix.Masked())
			continue
		}
		address, err := netip.ParseAddr(part)
		if err != nil {
			return nil, err
		}
		out = append(out, netip.PrefixFrom(address.Unmap(), address.Unmap().BitLen()))
	}
	return out, nil
}

// Usage writes a table of options for the help.
func Usage(options []Option) string {
	var b strings.Builder
	for _, o := range options {
		fmt.Fprintf(&b, "  -%s (%s)\n      %s", o.Flag, o.Env, o.Help)
		if o.Default != "" {
			fmt.Fprintf(&b, " [%s]", o.Default)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
