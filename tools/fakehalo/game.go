package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

var (
	logMu   sync.Mutex
	logJSON = os.Getenv("HALO_SERVER_LOG_FORMAT") == "json"
	keeper  func(level, cat, text string)
)

// logLine prints a line as the server does (JSON with log_format = json)
// and keeps it in the log the web's viewers read.
func logLine(level, cat, text string) {
	printLine(level, cat, text)
	if keeper != nil {
		keeper(level, cat, text)
	}
}

// logPrivate prints a line on standard output alone (a password, the
// owner's console).
func logPrivate(level, cat, text string) { printLine(level, cat, text) }

func printLine(level, cat, text string) {
	logMu.Lock()
	defer logMu.Unlock()
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	if logJSON {
		data, _ := json.Marshal(map[string]string{"time": now, "level": level, "cat": cat, "msg": text})
		os.Stdout.Write(append(data, '\n'))
		return
	}
	fmt.Fprintf(os.Stdout, "%s %s %s: %s\n", now, strings.ToUpper(map[string]string{"info": "info", "warn": "warn",
		"error": "error"}[level]), cat, text)
}

type player struct {
	Slot, Team, Score, Kills, Deaths, Machine int
	Name, Address, HWID                       string
	Joined                                    int64
	Muted                                     bool
}

type entry struct {
	Text, Map, Gametype, Rejected string
}

type ban struct {
	When, IP, HWID, Players, Reason string
	Until                           *int64
}

type event struct {
	Seq   int64
	Group string
	Data  map[string]any
	// the player's address and hardware id, for those who may read them
	Private map[string]any
}

// view is the event as a role reads it.
func (g *game) view(e event, role string) map[string]any {
	if len(e.Private) == 0 || !options.capable || !g.addresses(role) {
		return e.Data
	}
	out := map[string]any{}
	for k, v := range e.Private {
		out[k] = v
	}
	for k, v := range e.Data {
		out[k] = v
	}
	return out
}

type logEntry struct {
	N     int64  `json:"n"`
	Time  int64  `json:"time"`
	Level string `json:"level"`
	Cat   string `json:"cat"`
	Text  string `json:"text"`
}

type game struct {
	mu sync.Mutex
	// the log's ring and the RCON's subscribers have their own locks, so
	// that a line logged with mu held does not wait for it
	ringMu      sync.Mutex
	subMu       sync.Mutex
	st          *settings
	state       string
	started     time.Time
	rotation    []entry
	index       int
	played      int
	current     entry
	override    *entry
	players     []*player
	machines    int
	countdown   int
	seconds     int
	scores      [2]int
	empty       int
	games       int
	bans        []ban
	logs        []logEntry
	logN        int64
	events      []event
	seq         int64
	subscribers map[*rcon]bool
	sessions    map[string]*session
	failures    map[string][]time.Time
	exit        chan int
	drain       *drainState
	stopping    bool
	stalledAt   time.Time
	dataRoot    string
	logRoot     string
	tunnel      *net.UDPConn
	web         net.Listener
	tunnelPort  int
	webPort     int
	frameMax    float64
	lastHealth  time.Time
}

type drainState struct {
	After   string
	Notice  string
	Restart bool
	Since   int64
}

func newGame(st *settings) (*game, int) {
	logJSON = st.text("server.log_format") == "json"
	g := &game{st: st, state: "waiting", started: time.Now(), subscribers: map[*rcon]bool{}, sessions: map[string]*session{},
		failures: map[string][]time.Time{}, exit: make(chan int, 4), index: -1}
	keeper = g.keep
	g.dataRoot = os.Getenv("HALO_DATA_ROOT")
	if g.dataRoot == "" {
		g.dataRoot = "."
	}
	g.logRoot = os.Getenv("HALO_LOG_ROOT")
	if g.logRoot == "" {
		g.logRoot = g.dataRoot
	}
	if _, err := os.Stat(filepath.Join(g.dataRoot, "maps", "ui.map")); err != nil {
		logLine("error", "server", "no usable maps: "+filepath.Join(g.dataRoot, "maps", "ui.map")+" is missing (fakehalo)")
		return nil, 2
	}
	g.rotation = g.parseRotation(st.text("server.rotation"))
	playable := 0
	for _, e := range g.rotation {
		if e.Rejected == "" {
			playable++
		} else {
			logLine("warn", "server", fmt.Sprintf("rotation: %s left out: %s", e.Text, e.Rejected))
		}
	}
	if playable == 0 {
		logLine("error", "server", "no rotation entry can be played: stopping (server.rotation)")
		return nil, 3
	}
	if w := st.text("server.web_password"); w != "" && len(w) < 8 && st.text("server.web_bind") != "127.0.0.1" {
		logLine("error", "server", "server.web_password is shorter than 8 characters with the web port beyond loopback")
		return nil, 3
	}
	g.tunnelPort = st.int("network.tunnel_port")
	if g.tunnelPort == 0 {
		g.tunnelPort = 2302
	}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{Port: g.tunnelPort})
	if err != nil {
		logLine("error", "net", fmt.Sprintf("the tunnel's port %d/udp is in use: %v", g.tunnelPort, err))
		return nil, 4
	}
	g.tunnel = udp
	g.webPort = st.int("server.web_port")
	if g.webPort != 0 {
		listener, err := net.Listen("tcp4", net.JoinHostPort(st.text("server.web_bind"), strconv.Itoa(g.webPort)))
		if err != nil {
			logLine("error", "server", fmt.Sprintf("the web port %d is in use: %v", g.webPort, err))
			return nil, 4
		}
		g.web = listener
	}
	g.readBans()
	return g, 0
}

func (g *game) mapExists(name string) bool {
	if rest, ok := strings.CutPrefix(name, `custom_maps\`); ok {
		_, err := os.Stat(filepath.Join(g.dataRoot, "custom_maps", rest+".map"))
		return err == nil
	}
	if strings.Contains(name, `\`) {
		name = name[strings.LastIndexByte(name, '\\')+1:]
	}
	_, err := os.Stat(filepath.Join(g.dataRoot, "maps", name+".map"))
	return err == nil
}

func (g *game) parseRotation(text string) []entry {
	var out []entry
	for _, t := range spec.RotationEntries(text) {
		e := entry{Text: t, Map: spec.RotationMap(t), Gametype: spec.RotationGametype(t)}
		if !g.mapExists(e.Map) {
			e.Rejected = "the map " + e.Map + " is not there"
		} else if strings.HasPrefix(e.Gametype, "file=") || !builtin(e.Gametype) {
			name := strings.TrimPrefix(e.Gametype, "file=")
			if !g.gametypeExists(name) {
				e.Rejected = "no gametype " + name
			}
		}
		out = append(out, e)
	}
	return out
}

var builtins = []string{"slayer", "team_slayer", "ctf", "oddball", "king", "race", "swat", "infection", "coop", "elimination"}

func builtin(name string) bool {
	for _, b := range builtins {
		if b == name {
			return true
		}
	}
	return false
}

func (g *game) gametypesDir() string {
	return filepath.Join(filepath.Dir(configPath()), "gametypes")
}

func (g *game) gametypeExists(name string) bool {
	for _, suffix := range []string{".toml", ".gametype"} {
		if _, err := os.Stat(filepath.Join(g.gametypesDir(), name+suffix)); err == nil {
			return true
		}
	}
	return false
}

// keep keeps a log line for /api/log and the RCON's log events.
func (g *game) keep(level, cat, text string) {
	g.ringMu.Lock()
	g.logN++
	line := logEntry{N: g.logN, Time: time.Now().UnixMilli(), Level: level, Cat: cat, Text: text}
	g.logs = append(g.logs, line)
	if len(g.logs) > 2048 {
		g.logs = g.logs[len(g.logs)-2048:]
	}
	group := "log"
	if cat == "chat" {
		group = "chat"
	}
	g.ringMu.Unlock()
	subscribers := g.subscriberList()
	for _, r := range subscribers {
		r.event(group, map[string]any{"type": "event", "time": line.Time, "event": "log", "n": line.N,
			"level": level, "cat": cat, "text": text})
	}
}

func (g *game) subscriberList() []*rcon {
	g.subMu.Lock()
	defer g.subMu.Unlock()
	list := make([]*rcon, 0, len(g.subscribers))
	for r := range g.subscribers {
		list = append(list, r)
	}
	return list
}

// emit sends an event (kept in the ring, with a seq) to the subscribers;
// call with g.mu held.
func (g *game) emit(group, name string, fields map[string]any) {
	g.seq++
	data := map[string]any{"type": "event", "seq": g.seq, "time": time.Now().UnixMilli(), "event": name}
	for k, v := range fields {
		data[k] = v
	}
	g.events = append(g.events, event{Seq: g.seq, Group: group, Data: data})
	if len(g.events) > 512 {
		g.events = g.events[len(g.events)-512:]
	}
	for _, r := range g.subscriberList() {
		go r.event(group, data)
	}
}

func (g *game) addresses(role string) bool {
	return role == "owner" || g.st.bool("server.log_addresses")
}

func (g *game) playerJSON(p *player, role string) map[string]any {
	out := map[string]any{"slot": p.Slot, "name": p.Name, "team": p.Team, "score": p.Score, "kills": p.Kills,
		"deaths": p.Deaths, "assists": 0, "ping": 40 + p.Slot, "machine": p.Machine, "joined": p.Joined, "muted": p.Muted}
	if g.addresses(role) {
		out["address"], out["hwid"] = p.Address, p.HWID
	}
	if options.capable {
		out["client"] = clientOf(p.Machine)
	}
	return out
}

// clientOf is a scripted player's game as it tells the server
// (server_status.c's client): the project's build, one in two with a mod,
// one in three made from a fork.
func clientOf(machine int) map[string]any {
	repository, mods := "OpenCommunityEdition/OpenCE", []string{}
	if machine%2 == 1 {
		mods = []string{"trollface 1.0"}
	}
	if machine%3 == 0 {
		repository = "someone/OpenCE"
	}
	return map[string]any{"build": 282, "flavor": "release", "version": "OpenCE Linux | build 282 (release)",
		"repository": repository, "mods": mods}
}

func (g *game) teams() bool {
	gt := g.current.Gametype
	return strings.HasPrefix(gt, "team_") || gt == "ctf" || gt == "swat"
}

func (g *game) entryJSON(e entry) map[string]any {
	var rejected any
	if e.Rejected != "" {
		rejected = e.Rejected
	}
	return map[string]any{"text": e.Text, "map": e.Map, "gametype": e.Gametype, "rules": map[string]any{}, "rejected": rejected}
}

func (g *game) nextEntry() (entry, int) {
	if g.override != nil {
		return *g.override, -1
	}
	n := len(g.rotation)
	for i := 1; i <= n; i++ {
		j := (g.index + i) % n
		if g.index < 0 {
			j = i - 1
		}
		if g.rotation[j].Rejected == "" {
			return g.rotation[j], j
		}
	}
	return entry{Text: "bloodgulch:slayer", Map: "bloodgulch", Gametype: "slayer"}, -1
}

// status is /api/status for a role; call with g.mu held.
func (g *game) status(role string) map[string]any {
	var scores any
	if g.teams() {
		scores = []int{g.scores[0], g.scores[1]}
	}
	next, _ := g.nextEntry()
	invite := ""
	if role == "admin" || role == "owner" {
		invite = "halo://join/fake-" + g.st.text("server.instance")
	}
	var masters []map[string]any
	if os.Getenv("HALO_SERVER_PUBLISHER_KEY") != "" && g.st.bool("server.public") {
		masters = append(masters, map[string]any{"url": "https://master.opencommunityedition.com", "listed": g.drain == nil,
			"probe": "ok", "code": "", "message": "", "updated": time.Now().Unix()})
	}
	frameMax := 4.0 + rand.Float64()
	if g.stalled() {
		frameMax = 60000
	}
	s := map[string]any{"ok": true, "time": time.Now().UnixMilli(), "name": g.st.text("server.name"), "state": g.state,
		"build": Build, "network_version": NetworkVersion, "uptime": int(time.Since(g.started).Seconds()), "games_hosted": g.games,
		"map": g.current.Map, "level": `levels\test\` + g.current.Map + `\` + g.current.Map, "gametype": g.current.Gametype,
		"variant": strings.ToUpper(g.current.Gametype[:1]) + g.current.Gametype[1:], "teams": g.teams(),
		"players": len(g.players), "max_players": g.st.int("server.max_players"), "machines": len(g.players),
		"countdown": g.countdown, "game_seconds": g.seconds, "team_scores": scores,
		"listing":      map[string]any{"online": true, "public": g.st.bool("server.public"), "password": g.st.text("server.password") != "", "invite": invite},
		"rotation":     map[string]any{"index": g.index, "count": len(g.rotation), "next": g.entryJSON(next)},
		"frame_ms_max": frameMax, "frame_ms_avg": 1.2, "tick_ms_avg": 1.2, "masters": masters,
		"update": map[string]any{"available": false, "build": ""}}
	if options.capable {
		s["pid"], s["started"], s["instance"], s["region"] = os.Getpid(), g.started.Unix(), g.st.text("server.instance"), g.st.text("server.region")
		s["tunnel_port"], s["web_port"], s["publisher_key_id"] = g.tunnelPort, g.webPort, ""
		s["capabilities"] = capabilities()
		if g.drain != nil {
			s["drain"] = map[string]any{"after": g.drain.After, "restart": g.drain.Restart, "since": g.drain.Since}
		} else {
			s["drain"] = nil
		}
	}
	return s
}

func capabilities() []string {
	list := []string{"events", "status", "rotation", "settings", "bans"}
	if options.capable {
		list = append(list, "owner_web", "ban_address", "stop", "event_addresses", "identity")
	}
	return list
}

func (g *game) stalled() bool {
	return options.stall > 0 && time.Since(g.started) > options.stall
}

// run plays the script until the server stops: its exit code.
func (g *game) run(signals chan os.Signal) int {
	if g.web != nil {
		go http.Serve(g.web, g.handler())
	}
	go g.tunnelLoop()
	go g.console()
	g.mu.Lock()
	started := map[string]any{"name": g.st.text("server.name"), "build": Build,
		"network_version": NetworkVersion, "rotation": g.rotationTexts()}
	if options.capable {
		started["pid"], started["started"], started["instance"] = os.Getpid(), g.started.Unix(), g.st.text("server.instance")
		started["region"], started["tunnel_port"], started["web_port"] = g.st.text("server.region"), g.tunnelPort, g.webPort
		started["publisher_key_id"] = ""
	}
	g.emit("server", "server_started", started)
	g.mu.Unlock()
	g.runInit()
	g.openLobby()
	if !options.noReady {
		listing := "public"
		if !g.st.bool("server.public") {
			listing = "private"
		}
		logLine("info", "server", fmt.Sprintf("ready (tunnel %d/udp, web %s:%d, listing %s)", g.tunnelPort,
			g.st.text("server.web_bind"), g.webPort, listing))
	}
	tick := time.Duration(options.tick * float64(time.Second))
	if tick < 10*time.Millisecond {
		tick = 10 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	status := time.NewTicker(2 * time.Second)
	defer status.Stop()
	var crash <-chan time.Time
	if options.crashAfter > 0 {
		crash = time.After(options.crashAfter)
	}
	terms := 0
	for {
		select {
		case code := <-g.exit:
			return g.shutdown(code, "command")
		case <-crash:
			logLine("error", "game", "a fatal error (fakehalo --crash-after)")
			return 1
		case sig := <-signals:
			switch sig {
			case syscall.SIGHUP:
				g.reload()
			case syscall.SIGUSR1:
				g.mu.Lock()
				data, _ := json.Marshal(g.status("owner"))
				g.mu.Unlock()
				logPrivate("info", "server", "status: "+string(data))
			default:
				terms++
				if terms > 1 {
					return 128 + int(sig.(syscall.Signal))
				}
				go func() {
					time.Sleep(5 * time.Second)
					os.Exit(128 + int(sig.(syscall.Signal)))
				}()
				return g.shutdown(0, "signal")
			}
		case <-ticker.C:
			if !g.stalled() {
				g.step()
			}
		case <-status.C:
			for _, r := range g.subscriberList() {
				go r.status()
			}
			if time.Since(g.lastHealth) >= time.Duration(60*options.tick*float64(time.Second)) {
				g.lastHealth = time.Now()
				g.mu.Lock()
				n := len(g.players)
				g.mu.Unlock()
				logLine("info", "health", fmt.Sprintf("players=%d frame_ms_max=4.1 frame_ms_avg=1.20 cpu=1.0%% rss=40MB", n))
			}
		}
	}
}

func (g *game) shutdown(code int, reason string) int {
	g.mu.Lock()
	g.state = "stopping"
	if g.drain != nil && reason == "command" {
		reason = "drained"
	}
	g.emit("server", "shutdown", map[string]any{"reason": reason, "code": code})
	g.mu.Unlock()
	logLine("info", "server", fmt.Sprintf("stopping (exit %d)", code))
	time.Sleep(300 * time.Millisecond)
	return code
}

func (g *game) rotationTexts() []string {
	var out []string
	for _, e := range g.rotation {
		out = append(out, e.Text)
	}
	return out
}

func (g *game) runInit() {
	path := g.st.text("server.init_file")
	if path == "" {
		return
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(g.dataRoot, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		logLine("info", "server", "no init file ("+path+"): the settings alone")
		return
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "//") {
			continue
		}
		n++
		g.command(line, "owner", "init")
	}
	logLine("info", "server", fmt.Sprintf("%d commands from %s", n, path))
}

// tunnelLoop answers the tunnel's packets (an echo: the masters' probe of a
// real server is beyond the fake).
func (g *game) tunnelLoop() {
	buffer := make([]byte, 1500)
	for {
		n, from, err := g.tunnel.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		g.tunnel.WriteToUDP(buffer[:n], from)
	}
}

// console reads the owner's console on standard input.
func (g *game) console() {
	if !g.st.bool("server.console") {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		shown := line
		if strings.Contains(strings.ToLower(line), "password") {
			shown = "(a password's command)"
		}
		logPrivate("info", "console", "> "+shown)
		lines, ok := g.command(line, "owner", "stdin")
		for _, l := range lines {
			level := "info"
			if !ok {
				level = "warn"
			}
			logPrivate(level, "console", l)
		}
	}
	if g.st.bool("server.console_exit_on_eof") {
		g.exit <- 0
	}
}

func (g *game) openLobby() {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, i := g.nextEntry()
	g.override = nil
	g.current, g.index = e, i
	g.state, g.countdown, g.seconds, g.scores = "lobby", -1, 0, [2]int{}
	g.emit("game", "lobby_opened", map[string]any{"map": e.Map, "gametype": e.Gametype, "variant": e.Gametype,
		"teams": g.teams(), "index": i})
	g.checkDrain()
}

func (g *game) playersJSON(role string) []map[string]any {
	out := []map[string]any{}
	for _, p := range g.players {
		out = append(out, g.playerJSON(p, role))
	}
	return out
}

// step is one scripted second.
func (g *game) step() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.drain == nil && len(g.players) < options.players && rand.IntN(3) == 0 && g.state != "stopping" {
		g.join()
	}
	switch g.state {
	case "lobby":
		if len(g.players) >= g.st.int("server.min_players") {
			if g.countdown < 0 {
				g.countdown = g.st.int("server.lobby_seconds")
			}
			g.countdown--
			g.emit("game", "countdown", map[string]any{"seconds": g.countdown})
			if g.countdown <= 0 {
				g.state, g.countdown = "loading", -1
			}
		}
	case "loading":
		g.state = "playing"
		var scores any
		if g.teams() {
			scores = []int{g.scores[0], g.scores[1]}
		}
		g.emit("game", "game_started", map[string]any{"map": g.current.Map, "gametype": g.current.Gametype,
			"variant": g.current.Gametype, "teams": g.teams(), "team_scores": scores, "players": g.playersJSON("viewer")})
	case "playing":
		g.seconds++
		if len(g.players) > 1 && rand.IntN(4) == 0 {
			killer, victim := g.players[rand.IntN(len(g.players))], g.players[rand.IntN(len(g.players))]
			if killer != victim {
				killer.Kills++
				killer.Score++
				victim.Deaths++
				if g.teams() && killer.Team >= 0 {
					g.scores[killer.Team]++
				}
				g.emit("game", "kill", map[string]any{"killer": killer.Slot, "victim": victim.Slot, "weapon": nil, "friendly_fire": false})
			}
		}
		if len(g.players) == 0 {
			g.empty++
		} else {
			g.empty = 0
		}
		empty := g.st.int("server.empty_seconds")
		switch {
		case g.seconds >= 60:
			g.endGame("time")
		case empty > 0 && g.empty >= empty:
			g.endGame("empty")
		}
	case "postgame":
		g.seconds++
		if g.seconds >= g.st.int("server.postgame_seconds") {
			g.mu.Unlock()
			g.openLobby()
			g.mu.Lock()
		}
	}
}

// endGame ends the game (call with g.mu held).
func (g *game) endGame(reason string) {
	var scores any
	if g.teams() {
		scores = []int{g.scores[0], g.scores[1]}
	}
	g.emit("game", "game_ended", map[string]any{"map": g.current.Map, "gametype": g.current.Gametype,
		"variant": g.current.Gametype, "teams": g.teams(), "reason": reason, "duration": g.seconds, "team_scores": scores,
		"players": g.playersJSON("viewer")})
	g.games++
	g.state, g.seconds = "postgame", 0
	g.emit("game", "postgame", map[string]any{"seconds": g.st.int("server.postgame_seconds")})
	logLine("info", "game", fmt.Sprintf("ended (%s): %s on %s", reason, g.current.Gametype, g.current.Map))
	if g.drain != nil && g.drain.After == "game" {
		g.finishDrain()
	}
}

var names = []string{"Milenko", "Pablo", "Chief", "Arbiter", "Noble6", "Kat", "Jorge", "Emile", "Carter", "Jun"}

// join brings a scripted player (call with g.mu held).
func (g *game) join() {
	slot := 0
	for taken := true; taken; slot++ {
		taken = false
		for _, p := range g.players {
			if p.Slot == slot {
				taken = true
			}
		}
		if !taken {
			break
		}
	}
	g.machines++
	address := fmt.Sprintf("203.0.113.%d", 10+g.machines%200)
	hwid := fmt.Sprintf("%016x", 0xfeed0000+g.machines)
	for _, b := range g.bans {
		if (b.IP != "" && b.IP == address) || (b.HWID != "" && b.HWID == hwid) {
			if b.Until == nil || *b.Until > time.Now().Unix() {
				logLine("info", "net", "refusing a machine: banned (bans.txt)")
				return
			}
		}
	}
	team := -1
	if g.teams() {
		team = slot % 2
	}
	p := &player{Slot: slot, Name: names[(g.machines-1)%len(names)], Team: team, Machine: g.machines, Address: address,
		HWID: hwid, Joined: time.Now().Unix()}
	g.players = append(g.players, p)
	fields := map[string]any{"slot": p.Slot, "name": p.Name, "team": p.Team, "machine": p.Machine}
	g.emitPlayer("player_joined", fields, p)
	logLine("info", "player", fmt.Sprintf("%s joined (slot %d, machine %d)", p.Name, p.Slot, p.Machine))
}

// emitPlayer sends a player's event, with the address to those who may
// read it (7.4).
func (g *game) emitPlayer(name string, fields map[string]any, p *player) {
	g.seq++
	data := map[string]any{"type": "event", "seq": g.seq, "time": time.Now().UnixMilli(), "event": name}
	for k, v := range fields {
		data[k] = v
	}
	e := event{Seq: g.seq, Group: "player", Data: data, Private: map[string]any{"address": p.Address, "hwid": p.HWID}}
	g.events = append(g.events, e)
	for _, r := range g.subscriberList() {
		go r.event("player", g.view(e, r.role))
	}
}

func (g *game) leave(p *player, reason string) {
	for i, q := range g.players {
		if q == p {
			g.players = append(g.players[:i], g.players[i+1:]...)
			break
		}
	}
	g.emitPlayer("player_left", map[string]any{"slot": p.Slot, "name": p.Name, "reason": reason}, p)
	logLine("info", "player", p.Name+" left ("+reason+")")
	g.checkDrain()
}

// checkDrain stops a drain's server once it may (call with g.mu held).
func (g *game) checkDrain() {
	if g.drain == nil {
		return
	}
	if g.drain.After == "empty" && len(g.players) == 0 {
		g.finishDrain()
	}
	if g.drain.After == "game" && (g.state == "lobby" && len(g.players) == 0) {
		g.finishDrain()
	}
}

func (g *game) finishDrain() {
	code := 0
	if g.drain.Restart {
		code = 5
	}
	select {
	case g.exit <- code:
	default:
	}
}

func (g *game) reload() {
	g.mu.Lock()
	before := strings.Join(g.rotationTexts(), ",")
	g.rotation = g.parseRotation(g.st.text("server.rotation"))
	changed := before != strings.Join(g.rotationTexts(), ",")
	g.emit("server", "reloaded", map[string]any{"rotation_changed": changed, "rejected": []any{}})
	g.mu.Unlock()
	logLine("info", "server", "reloaded")
}

func (g *game) bansPath() string { return filepath.Join(g.logRoot, "bans.txt") }

// readBans reads bans.txt: ip=… hwid=… until=… reason=… players=…
func (g *game) readBans() {
	data, err := os.ReadFile(g.bansPath())
	if err != nil {
		return
	}
	g.bans = nil
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// (the server's line: the time, then tab-separated fields, a field
		// not known being "none")
		fields := strings.Split(line, "\t")
		b := ban{When: fields[0]}
		for _, field := range fields[1:] {
			k, v, _ := strings.Cut(field, "=")
			if v == "none" {
				v = ""
			}
			switch k {
			case "ip":
				b.IP = v
			case "hwid":
				b.HWID = v
			case "until":
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					b.Until = &n
				}
			case "reason":
				b.Reason = v
			case "players":
				b.Players = v
			}
		}
		g.bans = append(g.bans, b)
	}
}

func (g *game) writeBans() {
	var b strings.Builder
	for _, x := range g.bans {
		b.WriteString(banLine(x) + "\n")
	}
	os.MkdirAll(g.logRoot, 0o750)
	os.WriteFile(g.bansPath(), []byte(b.String()), 0o640)
}

func banLine(b ban) string {
	none := func(s string) string {
		if s == "" {
			return "none"
		}
		return s
	}
	line := fmt.Sprintf("%s\tip=%s\thwid=%s\tdiscord_username=none\tdiscord_id=none\tplayers=%s\treason=%s", b.When,
		none(b.IP), none(b.HWID), none(b.Players), none(b.Reason))
	if b.Until != nil {
		line += "\tuntil=" + strconv.FormatInt(*b.Until, 10)
	}
	return line
}

func (g *game) bansJSON() []map[string]any {
	g.readBans()
	out := []map[string]any{}
	for i, b := range g.bans {
		var until any
		if b.Until != nil {
			until = *b.Until
		}
		out = append(out, map[string]any{"n": i, "when": b.When, "ip": b.IP, "hwid": b.HWID, "players": b.Players,
			"reason": b.Reason, "until": until})
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
