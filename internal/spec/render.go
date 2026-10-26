package spec

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Input is what an instance's files are made from: the profile's values
// with the instance's overrides already merged (Merge), and the fleet's.
type Input struct {
	Slug       string
	Name       string
	Generation int64
	Build      string
	Running    bool
	TunnelPort int
	WebPort    int
	Loopback   int
	// the game settings (Merge of the profile's and the overrides)
	Settings  map[string]any
	Rotation  string
	Region    string
	InitTxt   string
	Gametypes map[string]string
	// the join password (the profile's policy), the agent's web password,
	// the owner's (the process runtime's, with the game's owner role over
	// the web), and the publisher key's seed (64 hex)
	Password, WebPassword, OwnerPassword, PublisherSeed string
	// the node's public address (host, or host:port; the instance's
	// tunnel port is added to a host alone)
	PublicAddress string
	LogAddresses  bool
	WebUI         bool
	Needs         []string
}

// Render makes an instance's desired state.
func Render(in Input) Instance {
	settings := map[string]any{}
	for k, v := range in.Settings {
		if k == "server.password" {
			continue
		}
		settings[k] = v
	}
	if _, ok := settings["server.name"]; !ok {
		name := in.Name
		if len(name) > 15 {
			name = name[:15]
		}
		settings["server.name"] = name
	}
	gametypes := map[string]string{}
	for k, v := range in.Gametypes {
		gametypes[k] = v
	}
	needs := append([]string(nil), in.Needs...)
	sort.Strings(needs)
	return Instance{
		Slug: in.Slug, Name: in.Name, Generation: in.Generation, Build: in.Build, Running: in.Running,
		TunnelPort: in.TunnelPort, WebPort: in.WebPort, Loopback: in.Loopback,
		Env:         Environment(in),
		WebPassword: in.WebPassword, OwnerPassword: in.OwnerPassword,
		ConfigTOML: ConfigTOML(in.Slug, settings, in.Rotation), Settings: settings, Rotation: in.Rotation,
		InitTxt: InitText(in.InitTxt), Gametypes: gametypes, Needs: needs,
	}
}

func onOff(on bool) string {
	if on {
		return "1"
	}
	return "0"
}

// Environment is the fleet's settings of an instance (OPENCE_FLEET_PLAN.md
// 12.2), which the server pins: the runtime adds its paths and ports.
func Environment(in Input) map[string]string {
	env := map[string]string{
		"HALO_SERVER":                     "1",
		"HALO_SERVER_WEB_PASSWORD":        in.WebPassword,
		"HALO_SERVER_WEB_UI":              onOff(in.WebUI),
		"HALO_SERVER_LOG_FORMAT":          "json",
		"HALO_SERVER_LOG_ADDRESSES":       onOff(in.LogAddresses),
		"HALO_SERVER_AUTO_UPDATE":         "off",
		"HALO_SERVER_CONSOLE":             "1",
		"HALO_SERVER_CONSOLE_EXIT_ON_EOF": "0",
		"HALO_SERVER_INSTANCE":            in.Slug,
		"HALO_SERVER_METRICS":             "1",
		"HALO_UPDATE_AUTO":                "0",
		"HALO_NET_JOIN_FROM_CLIPBOARD":    "0",
	}
	if in.PublisherSeed != "" {
		env["HALO_SERVER_PUBLISHER_KEY"] = in.PublisherSeed
	}
	if in.Region != "" {
		env["HALO_SERVER_REGION"] = in.Region
	}
	if in.Password != "" {
		env["HALO_SERVER_PASSWORD"] = in.Password
	}
	if address := strings.TrimSpace(in.PublicAddress); address != "" {
		env["HALO_NET_PUBLIC_ADDRESS"] = PublicAddress(address, in.TunnelPort)
	}
	return env
}

// PublicAddress is a node's public address for an instance: a host alone
// gets the instance's tunnel port (forwarded one to one).
func PublicAddress(address string, port int) string {
	if strings.HasPrefix(address, "[") {
		if strings.Contains(address, "]:") {
			return address
		}
		return address + ":" + strconv.Itoa(port)
	}
	if strings.Count(address, ":") == 1 {
		return address
	}
	if strings.Count(address, ":") > 1 {
		// (a bare IPv6 address)
		return "[" + address + "]:" + strconv.Itoa(port)
	}
	return address + ":" + strconv.Itoa(port)
}

// InitText is init.txt's text: the profile's, ending with a line's end.
func InitText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return ""
	}
	return text + "\n"
}

// ConfigTOML writes config.toml: the game settings in their tables, sorted,
// the rotation among them. The fleet's settings are not in it (they are the
// environment's), and neither is a secret.
func ConfigTOML(slug string, settings map[string]any, rotation string) string {
	tables := map[string][]string{}
	add := func(name string, value any) {
		table, key, ok := strings.Cut(name, ".")
		if !ok {
			return
		}
		tables[table] = append(tables[table], key+" = "+tomlValue(value))
	}
	for name, value := range settings {
		if s, ok := Lookup(name); ok && s.Secret {
			continue
		}
		add(name, value)
	}
	if strings.TrimSpace(rotation) != "" {
		add("server.rotation", rotation)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by opence-fleet for %s before each start: a change made here is\n", slug)
	b.WriteString("# overwritten. The fleet sets the rest (ports, passwords, paths, the publisher\n")
	b.WriteString("# key) in the environment, where the server pins them.\n")
	names := make([]string, 0, len(tables))
	for name := range tables {
		names = append(names, name)
	}
	sort.Strings(names)
	// ([server] first: it is the file's main table)
	sort.SliceStable(names, func(i, j int) bool { return names[i] == "server" && names[j] != "server" })
	for _, name := range names {
		lines := tables[name]
		sort.Strings(lines)
		fmt.Fprintf(&b, "\n[%s]\n", name)
		for _, line := range lines {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

func tomlValue(value any) string {
	switch v := value.(type) {
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		text := strconv.FormatFloat(v, 'f', -1, 64)
		if !strings.ContainsAny(text, ".eE") {
			// (with its point: TOML reads 15 as an integer)
			text += ".0"
		}
		return text
	case string:
		return TOMLString(v)
	}
	return TOMLString(fmt.Sprint(value))
}

// TOMLString writes a TOML basic string: quotes and backslashes escaped,
// control characters as \uXXXX (as the game writes its own file).
func TOMLString(text string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range text {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// RotationEntries splits a rotation's text into its entries, as the server
// does: commas, semicolons or new lines; a line that begins with '#' is a
// comment.
func RotationEntries(text string) []string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, entry := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ';' }) {
			if entry = strings.TrimSpace(entry); entry != "" {
				out = append(out, entry)
			}
		}
	}
	return out
}

// RotationMap is an entry's map: custom_maps\<name> or a level's path as
// written, else the bare name.
func RotationMap(entry string) string {
	m, _, _ := strings.Cut(entry, ":")
	return strings.ReplaceAll(strings.TrimSpace(m), "/", `\`)
}

// RotationGametype is an entry's gametype ("slayer" when none), without its
// rules; file=<name> stays as written.
func RotationGametype(entry string) string {
	_, rest, ok := strings.Cut(entry, ":")
	if !ok {
		return "slayer"
	}
	gametype, _, _ := strings.Cut(rest, "+")
	return strings.TrimSpace(gametype)
}

// Slug makes a slug of a name: a-z, 0-9 and '-', 32 at most (the server's
// instance name, which the masters' server id is made of).
func Slug(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-")
	}
	return s
}

// Pattern fills a name pattern's {n} with a number ("BTB EU #{n}"); a
// pattern without {n} gets the number after it, so that the names of a
// count's servers differ.
func Pattern(pattern string, n int) string {
	if strings.Contains(pattern, "{n}") {
		return strings.ReplaceAll(pattern, "{n}", strconv.Itoa(n))
	}
	return pattern + " " + strconv.Itoa(n)
}
