// fakehalo is the dedicated server without the game (OPENCE_FLEET_PLAN.md
// 9.1): it behaves as `halo --server` does at the edges the fleet's agent
// touches (docs/dedicated_server.md in the game's repository): the command
// line, the exit codes, the settings' precedence and pins, the standard
// output's JSON lines (the ready line, the health line), the owner's console
// on standard input, the HTTP API, the RCON and its events, and a scripted
// rotation of lobbies, games and players. It is the agent's and the control
// plane's test double; tools/fakehalo/Dockerfile makes it an image of the
// server image's shape.
//
//	fakehalo --server [--tick 0.1] [--players 4] [--crash-after 30s] [--stall 20s] [--no-ready]
//	fakehalo --version | --keygen | --sign <text> | --help
package main

import (
	"archive/zip"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Build is the fake's build, as the real server's --version names its own.
const Build = "build-900"

// NetworkVersion is the network version the fake says it speaks.
const NetworkVersion = 39

var options struct {
	server     bool
	tick       float64
	players    int
	crashAfter time.Duration
	stall      time.Duration
	noReady    bool
	slowRCON   bool
	capable    bool
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	set := flag.NewFlagSet("fakehalo", flag.ContinueOnError)
	version := set.Bool("version", false, "print the build")
	keygen := set.Bool("keygen", false, "make a publisher key")
	sign := set.String("sign", "", "sign a text with the publisher key")
	makeRelease := set.String("make-release", "", "write a release zip of this fake (halo, a wrapper that runs it quickly) to <file> and exit")
	healthz := set.Bool("healthz", false, "the image's health check: GET /healthz on HALO_SERVER_WEB_PORT, 0 on a 200")
	makeMaps := set.String("make-maps", "", "write synthetic maps (valid headers, no game) into <dir>/maps and exit")
	set.BoolVar(&options.server, "server", false, "the dedicated server")
	set.StringVar(new(string), "config", "", "the settings file (HALO_CONFIG_FILE wins)")
	set.Float64Var(&options.tick, "tick", 1, "real seconds a scripted second takes")
	set.IntVar(&options.players, "players", 3, "players the script brings")
	set.DurationVar(&options.crashAfter, "crash-after", 0, "exit 1 after this long")
	set.DurationVar(&options.stall, "stall", 0, "stop the frames after this long (/healthz: stalled)")
	set.BoolVar(&options.noReady, "no-ready", false, "never print the ready line")
	set.BoolVar(&options.slowRCON, "slow-rcon", false, "read the RCON's clients slowly")
	set.BoolVar(&options.capable, "capabilities", true, "the game's fleet changes (owner over the web, POST /api/bans, sv_stop, addresses in events, identity)")
	if err := set.Parse(args); err != nil {
		return 3
	}
	if env := os.Getenv("FAKEHALO_TICK"); env != "" {
		fmt.Sscan(env, &options.tick)
	}
	if env := os.Getenv("FAKEHALO_CRASH_AFTER"); env != "" {
		options.crashAfter, _ = time.ParseDuration(env)
	}
	if os.Getenv("FAKEHALO_OLD") != "" {
		options.capable = false
	}
	if os.Getenv("HALO_SERVER") == "1" {
		options.server = true
	}
	switch {
	case *healthz:
		return healthCheck()
	case *makeRelease != "":
		if err := writeRelease(*makeRelease); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case *makeMaps != "":
		if err := writeMaps(*makeMaps); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case *version:
		fmt.Printf("OpenCE %s (network version %d)\n", Build, NetworkVersion)
		return 0
	case *keygen:
		return keyCommand(true, "")
	case *sign != "":
		return keyCommand(false, *sign)
	case !options.server:
		fmt.Println("fakehalo: the dedicated server without the game; --server runs it (see the source)")
		return 0
	}
	settings, err := loadSettings()
	if err != nil {
		logLine("error", "server", err.Error())
		return 3
	}
	g, code := newGame(settings)
	if code != 0 {
		return code
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGUSR1)
	signal.Ignore(syscall.SIGPIPE)
	return g.run(signals)
}

// writeRelease is --make-release: a halo-linux-release.zip's shape around
// this binary (halo is a script that runs it with FAKEHALO_TICK 0.05, for
// tests that cannot wait for real minutes), for the process runtime.
func writeRelease(path string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	binary, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	z := zip.NewWriter(f)
	add := func(name string, mode os.FileMode, data []byte) error {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(mode)
		w, err := z.CreateHeader(h)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	wrapper := "#!/bin/sh\nexport FAKEHALO_TICK=${FAKEHALO_TICK:-0.05}\nexec \"$(dirname \"$0\")/fakehalo\" \"$@\"\n"
	for _, file := range []struct {
		name string
		mode os.FileMode
		data []byte
	}{{"fakehalo", 0o755, binary}, {"halo", 0o755, []byte(wrapper)}, {"libSDL3.so.0", 0o644, []byte("not a library")}} {
		if err := add(file.name, file.mode, file.data); err != nil {
			f.Close()
			return err
		}
	}
	if err := z.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// healthCheck is --healthz, the image's HEALTHCHECK (as the real image's
// healthz.sh: no web port, nothing to ask).
func healthCheck() int {
	port := os.Getenv("HALO_SERVER_WEB_PORT")
	if port == "" || port == "0" {
		return 0
	}
	client := http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// keyCommand is --keygen and --sign: the publisher key beside config.toml.
func keyCommand(make bool, text string) int {
	path := filepath.Join(filepath.Dir(configPath()), "publisher.key")
	data, err := os.ReadFile(path)
	var key ed25519.PrivateKey
	if err == nil {
		seed, err := hex.DecodeString(strings.TrimSpace(strings.Split(string(data), "#")[0]))
		if err != nil || len(seed) != 32 {
			fmt.Fprintln(os.Stderr, "publisher.key is not 64 hexadecimal digits")
			return 3
		}
		key = ed25519.NewKeyFromSeed(seed)
	} else if make {
		_, key, _ = ed25519.GenerateKey(rand.Reader)
		if err := os.WriteFile(path, []byte(hex.EncodeToString(key.Seed())+"\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 3
		}
	} else {
		fmt.Fprintln(os.Stderr, "no publisher key")
		return 3
	}
	if make {
		fmt.Printf("public key ed25519:%s\n", hex.EncodeToString(key.Public().(ed25519.PublicKey)))
		return 0
	}
	fmt.Println(hex.EncodeToString(ed25519.Sign(key, []byte(text))))
	return 0
}
