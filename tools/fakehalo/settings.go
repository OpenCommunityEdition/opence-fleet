package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

// setting is one of the fake's settings: the game settings of the fleet's
// table, and the server's own (ports, passwords, paths) the fleet pins.
type setting struct {
	spec.Setting
	value  any
	pinned bool
}

// the server's own settings (docs/dedicated_server.md), beside the table's
// (a game setting the fleet manages, the Safety group's say, is the table's
// alone: listed twice, config.toml would have its key twice, which TOML
// refuses at the next start)
var own = []spec.Setting{
	{Name: "server.rotation", Type: "string", Default: "bloodgulch:ctf, sidewinder:team_slayer, prisoner:slayer", Env: "HALO_SERVER_ROTATION"},
	{Name: "server.web_port", Type: "integer", Default: int64(8080), Env: "HALO_SERVER_WEB_PORT"},
	{Name: "server.web_bind", Type: "string", Default: "127.0.0.1", Env: "HALO_SERVER_WEB_BIND"},
	{Name: "server.web_password", Type: "string", Default: "", Env: "HALO_SERVER_WEB_PASSWORD", Live: true, Secret: true},
	{Name: "server.web_moderator_password", Type: "string", Default: "", Env: "HALO_SERVER_WEB_MODERATOR_PASSWORD", Live: true, Secret: true},
	{Name: "server.web_viewer_password", Type: "string", Default: "", Env: "HALO_SERVER_WEB_VIEWER_PASSWORD", Secret: true},
	{Name: "server.web_owner_password", Type: "string", Default: "", Env: "HALO_SERVER_WEB_OWNER_PASSWORD", Secret: true},
	{Name: "server.web_ui", Type: "boolean", Default: true, Env: "HALO_SERVER_WEB_UI"},
	{Name: "server.log_format", Type: "string", Default: "text", Env: "HALO_SERVER_LOG_FORMAT", Live: true, Choices: []string{"text", "json"}},
	{Name: "server.log_addresses", Type: "boolean", Default: false, Env: "HALO_SERVER_LOG_ADDRESSES", Live: true},
	{Name: "server.auto_update", Type: "string", Default: "off", Env: "HALO_SERVER_AUTO_UPDATE", Live: true, Choices: []string{"off", "check", "install"}},
	{Name: "server.status_file", Type: "string", Default: "", Env: "HALO_SERVER_STATUS_FILE", Live: true},
	{Name: "server.metrics", Type: "boolean", Default: true, Env: "HALO_SERVER_METRICS", Live: true},
	{Name: "server.instance", Type: "string", Default: "", Env: "HALO_SERVER_INSTANCE"},
	{Name: "server.region", Type: "string", Default: "", Env: "HALO_SERVER_REGION", Live: true},
	{Name: "server.console", Type: "boolean", Default: true, Env: "HALO_SERVER_CONSOLE"},
	{Name: "server.console_exit_on_eof", Type: "boolean", Default: false, Env: "HALO_SERVER_CONSOLE_EXIT_ON_EOF"},
	{Name: "server.init_file", Type: "string", Default: "init.txt", Env: "HALO_SERVER_INIT_FILE"},
	{Name: "network.tunnel_port", Type: "integer", Default: int64(0), Env: "HALO_NET_TUNNEL_PORT"},
	{Name: "network.public_address", Type: "string", Default: "", Env: "HALO_NET_PUBLIC_ADDRESS", Live: true},
	{Name: "network.address", Type: "string", Default: "", Env: "HALO_NET_ADDRESS"},
}

// the settings the web's admin may not change (the owner's alone)
var ownerOnly = map[string]bool{"server.status_file": true, "server.auto_update": true, "server.log_addresses": true}

type settings struct {
	mu   sync.Mutex
	list []*setting
	path string
}

func configPath() string {
	if path := os.Getenv("HALO_CONFIG_FILE"); path != "" {
		return path
	}
	return "config.toml"
}

func normalize(s spec.Setting, value any) (any, error) {
	text := ""
	switch v := value.(type) {
	case string:
		text = v
	case bool:
		text = strconv.FormatBool(v)
	case int64:
		text = strconv.FormatInt(v, 10)
	case float64:
		text = strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		text = v.String()
	case nil:
		return s.Default, nil
	default:
		text = fmt.Sprint(v)
	}
	if _, ok := spec.Lookup(s.Name); ok {
		return spec.Normalize(s.Name, text)
	}
	switch s.Type {
	case "boolean":
		switch strings.ToLower(text) {
		case "1", "true", "on", "yes":
			return true, nil
		case "0", "false", "off", "no", "":
			return false, nil
		}
		return nil, fmt.Errorf("%s is true or false", s.Name)
	case "integer":
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s takes a whole number", s.Name)
		}
		return n, nil
	}
	if len(s.Choices) > 0 {
		for _, c := range s.Choices {
			if strings.EqualFold(c, text) {
				return c, nil
			}
		}
		return nil, fmt.Errorf("%s is one of %s", s.Name, strings.Join(s.Choices, "|"))
	}
	if (strings.HasSuffix(s.Name, "_password")) && text != "" && len(text) < 8 {
		return nil, fmt.Errorf("a web password is empty or 8 characters or more")
	}
	return text, nil
}

// loadSettings reads config.toml and the environment (which wins, and pins).
func loadSettings() (*settings, error) {
	st := &settings{path: configPath()}
	all := append([]spec.Setting(nil), spec.Settings...)
	for _, s := range own {
		if _, managed := spec.Lookup(s.Name); !managed {
			all = append(all, s)
		}
	}
	var file map[string]map[string]any
	data, err := os.ReadFile(st.path)
	if err == nil {
		if _, err := toml.Decode(string(data), &file); err != nil {
			return nil, fmt.Errorf("config.toml cannot be read (%v)", err)
		}
	}
	for _, s := range all {
		item := &setting{Setting: s, value: s.Default}
		table, key, _ := strings.Cut(s.Name, ".")
		if v, ok := file[table][key]; ok {
			if n, err := normalize(s, v); err == nil {
				item.value = n
			}
		}
		if env, ok := os.LookupEnv(s.Env); ok && s.Env != "" && env != "" {
			n, err := normalize(s, env)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", s.Env, err)
			}
			item.value, item.pinned = n, true
		}
		st.list = append(st.list, item)
	}
	if _, err := os.Stat(st.path); os.IsNotExist(err) {
		// (the first start writes the file whole, as the server does)
		if st.get("server.web_password") == "" {
			made := fmt.Sprintf("fake-%d", time.Now().UnixNano())
			st.find("server.web_password").value = made
			logPrivate("info", "server", "the web page's admin password: "+made)
		}
		st.write()
	}
	return st, nil
}

func (st *settings) find(name string) *setting {
	for _, s := range st.list {
		if s.Name == name {
			return s
		}
	}
	return nil
}

func (st *settings) get(name string) any {
	st.mu.Lock()
	defer st.mu.Unlock()
	if s := st.find(name); s != nil {
		return s.value
	}
	return nil
}

func (st *settings) text(name string) string { return spec.SettingText(st.get(name)) }

func (st *settings) int(name string) int {
	n, _ := strconv.Atoi(st.text(name))
	return n
}

func (st *settings) bool(name string) bool { return st.text(name) == "true" }

// set changes a setting now and in config.toml: the code of a refusal
// (pinned, forbidden, schema) and its message.
func (st *settings) set(name string, value any, role string) (string, string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.find(name)
	if s == nil || !s.Live {
		return "schema", name + " is not a setting sv_set changes"
	}
	if s.pinned {
		return "pinned", name + " is set by its environment variable " + s.Env
	}
	if ownerOnly[name] && role != "owner" {
		return "forbidden", name + " needs the owner role (the server's console)"
	}
	n, err := normalize(s.Setting, value)
	if err != nil {
		return "schema", err.Error()
	}
	s.value = n
	st.writeLocked()
	return "", ""
}

func (st *settings) write() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.writeLocked()
}

func (st *settings) writeLocked() {
	tables := map[string][]string{}
	for _, s := range st.list {
		table, key, _ := strings.Cut(s.Name, ".")
		tables[table] = append(tables[table], key+" = "+tomlValue(s.value))
	}
	var b strings.Builder
	b.WriteString("# fakehalo's settings\n")
	names := make([]string, 0, len(tables))
	for t := range tables {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		sort.Strings(tables[t])
		b.WriteString("\n[" + t + "]\n" + strings.Join(tables[t], "\n") + "\n")
	}
	_ = os.WriteFile(st.path, []byte(b.String()), 0o600)
}

func tomlValue(v any) string {
	switch x := v.(type) {
	case string:
		return spec.TOMLString(x)
	case float64:
		text := strconv.FormatFloat(x, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += ".0"
		}
		return text
	}
	return spec.SettingText(v)
}

// json is GET /api/settings' table.
func (st *settings) json() []map[string]any {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []map[string]any
	for _, s := range st.list {
		if !s.Live {
			continue
		}
		var env any
		if s.Env != "" {
			env = s.Env
		}
		entry := map[string]any{"name": s.Name, "type": s.Type, "value": s.value, "default": s.Default,
			"comment": s.Comment, "env": env, "pinned": s.pinned, "secret": s.Secret, "choices": nil, "min": nil, "max": nil}
		if len(s.Choices) > 0 {
			entry["choices"] = s.Choices
		}
		if s.Min != nil {
			entry["min"] = *s.Min
		}
		if s.Max != nil {
			entry["max"] = *s.Max
		}
		if s.Secret {
			entry["set"] = spec.SettingText(s.value) != ""
			entry["value"], entry["default"] = "", ""
		}
		out = append(out, entry)
	}
	return out
}
