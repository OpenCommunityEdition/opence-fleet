// opence-fleet is the OpenCE fleet: one binary with two jobs. `serve` is
// the control plane (the pages, the API, the agents' hub, SQLite);
// `agent` runs on each machine that hosts servers. The other commands are
// the control plane's own (a join token, the setup link, a password reset)
// and a client of its API for the command line.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	opencefleet "github.com/OpenCommunityEdition/opence-fleet"
	"github.com/OpenCommunityEdition/opence-fleet/internal/agent"
	"github.com/OpenCommunityEdition/opence-fleet/internal/agent/runtime"
	"github.com/OpenCommunityEdition/opence-fleet/internal/agent/runtime/container"
	"github.com/OpenCommunityEdition/opence-fleet/internal/agent/runtime/process"
	"github.com/OpenCommunityEdition/opence-fleet/internal/auth"
	"github.com/OpenCommunityEdition/opence-fleet/internal/config"
	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
	"github.com/OpenCommunityEdition/opence-fleet/internal/crypt"
	"github.com/OpenCommunityEdition/opence-fleet/internal/dockerapi"
	"github.com/OpenCommunityEdition/opence-fleet/internal/httpx"
	"github.com/OpenCommunityEdition/opence-fleet/internal/live"
	"github.com/OpenCommunityEdition/opence-fleet/internal/master"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
	"github.com/OpenCommunityEdition/opence-fleet/internal/update"
	"github.com/OpenCommunityEdition/opence-fleet/internal/web"
	"github.com/OpenCommunityEdition/opence-fleet/static"
)

// version is the release's (set with -ldflags "-X main.version=v1.2.3").
var version = "dev"

func init() {
	if version == "dev" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				if s.Key == "vcs.revision" && len(s.Value) >= 7 {
					version = "dev-" + s.Value[:7]
				}
			}
		}
	}
	core.Version = version
}

const usage = `opence-fleet: many OpenCE dedicated servers, on many machines, from one page.

The control plane:
  opence-fleet serve [options]           the pages, the API and the agents' hub (options: FLEET_*)
  opence-fleet join-token <node>         a node's join token and one-liner (on the control plane's machine;
                                         --runtime, --tunnel-ports, --web-ports: its settings)
  opence-fleet setup-link                the setup link again, while the fleet has no owner
  opence-fleet reset-password <user>     a new password for a user (from FLEET_PASSWORD, else asked)
  opence-fleet library add <folder>      a game data folder's maps into the library (with FLEET_URL/FLEET_TOKEN)

A node:
  opence-fleet agent [options]           the node's agent (options: FLEET_AGENT_*)
  opence-fleet agent --join <url> --token <join token> [--env-file /etc/opence-fleet/agent.env]

The command line (FLEET_URL, FLEET_TOKEN):
  opence-fleet servers | nodes | jobs [--json]
  opence-fleet start | stop | restart <server> [--drain]
  opence-fleet say [--group <g>] <text>
  opence-fleet command <server> <line>
  opence-fleet update [--channel latest] [--group <g>] [--after game|empty|now]
  opence-fleet export [--keys] > fleet.json · opence-fleet import < fleet.json

  opence-fleet version | health | help
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "agent":
		err = runAgent(os.Args[2:])
	case "version", "--version":
		fmt.Printf("opence-fleet %s (agent protocol 1, %s/%s)\n", version, goruntime.GOOS, goruntime.GOARCH)
	case "health":
		err = health(os.Args[2:])
	case "join-token":
		err = joinToken(os.Args[2:])
	case "setup-link":
		err = setupLink(os.Args[2:])
	case "reset-password":
		err = resetPassword(os.Args[2:])
	case "release-zip":
		err = releaseZip(os.Args[2:])
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		err = client(os.Args[1], os.Args[2:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "opence-fleet:", err)
		var bad usageError
		if errors.As(err, &bad) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// usageError is a command line that means nothing (exit 2).
type usageError string

func (e usageError) Error() string { return string(e) }

func logger(level string) *slog.Logger {
	l := slog.LevelInfo
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

// openFleet opens the control plane's database and key (serve and the
// commands that run beside it).
func openFleet(args []string, name string) (*config.ServeConfig, *store.Store, *crypt.Key, error) {
	set := flag.NewFlagSet(name, flag.ExitOnError)
	values := config.Flags(set, config.Serve)
	set.Parse(args)
	c, err := config.LoadServe(values)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := os.MkdirAll(c.Data, 0o700); err != nil {
		return nil, nil, nil, err
	}
	s, err := store.Open(c.DB)
	if err != nil {
		return nil, nil, nil, err
	}
	key, _, err := crypt.Load(c.SecretKey)
	if err != nil {
		s.Close()
		return nil, nil, nil, err
	}
	return c, s, key, nil
}

func httpClient(caFile string, timeout time.Duration) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no PEM certificate", caFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

func serve(args []string) error {
	set := flag.NewFlagSet("serve", flag.ExitOnError)
	values := config.Flags(set, config.Serve)
	set.Usage = func() { fmt.Fprint(os.Stderr, "opence-fleet serve [options]\n\n"+config.Usage(config.Serve)) }
	set.Parse(args)
	c, err := config.LoadServe(values)
	if err != nil {
		return err
	}
	log := logger(c.LogLevel)
	binDir := filepath.Join(c.Data, "bin")
	if c.Updates != update.Off {
		update.Launch(binDir, version, log)
	}
	if err := os.MkdirAll(c.Data, 0o700); err != nil {
		return err
	}
	if err := update.BackupDatabase(c.DB, version, log); err != nil {
		return err
	}
	s, err := store.Open(c.DB)
	if err != nil {
		return fmt.Errorf("the database %s: %w", c.DB, err)
	}
	defer s.Close()
	key, made, err := crypt.Load(c.SecretKey)
	if err != nil {
		return err
	}
	if made {
		log.Warn("a new secret key is made: back it up with the database (it opens the secrets kept in it)", "key", c.SecretKey)
	}
	masters, err := master.LoadMasters(c.MastersFile)
	if err != nil {
		return fmt.Errorf("FLEET_MASTERS_FILE: %w", err)
	}
	client, err := httpClient(c.CAFile, 30*time.Second)
	if err != nil {
		return fmt.Errorf("FLEET_CA_FILE: %w", err)
	}
	l := live.New()
	fleet := core.New(s, key, c, log, l)
	fleet.Masters, fleet.HTTP = masters, client
	accounts := auth.New(s, c.SessionHours)
	clients := &httpx.Clients{Proxies: c.TrustedProxies, Insecure: c.InsecureHTTP}
	if c.InsecureHTTP {
		log.Warn("FLEET_INSECURE_HTTP: sign-ins, changes and agents are taken over plain HTTP from anywhere; " +
			"passwords, session cookies and node tokens cross the network unencrypted")
	}
	server := web.New(fleet, accounts, clients, log, version)
	server.AgentScript = opencefleet.AgentScript
	web.Static = static.Files
	fleet.Hub.Allowed = func(r *http.Request) bool { return clients.Of(r).Protected() }

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	releaseKey, _ := hex.DecodeString(update.ReleaseKey)
	if c.UpdateKey != "" {
		if releaseKey, err = hex.DecodeString(c.UpdateKey); err != nil || len(releaseKey) != ed25519.PublicKeySize {
			return errors.New("FLEET_UPDATE_KEY is 64 hexadecimal digits")
		}
	}
	listener := update.Listener()
	if listener == nil {
		ln, err := net.Listen("tcp", c.Listen)
		if err != nil {
			return fmt.Errorf("FLEET_LISTEN %s: %w", c.Listen, err)
		}
		listener = fileOf(ln)
	}
	ln, err := net.FileListener(listener)
	if err != nil {
		return err
	}
	updater := &update.Updater{Current: version, Mode: c.Updates, Source: c.UpdateURL, Dir: binDir, Client: client,
		Key: ed25519.PublicKey(releaseKey), Log: log,
		Available: func(r update.Release) { fleet.FleetUpdate(r.Version) },
		Started:   func(v, actor string) { fleet.Audit(context.Background(), actor, "fleet.update_start", v, nil) },
		Failed:    func(v string, err error) { log.Error("the fleet's update failed", "version", v, "err", err) },
		Restart: func(binary string) {
			stop()
			time.Sleep(time.Second)
			if err := update.Exec(binary, listener); err != nil {
				log.Error("the update did not run", "err", err)
			}
		}}
	server.Updater = updater
	go updater.Run(ctx, 6*time.Hour)

	if code, err := accounts.SetupCode(ctx); err == nil && code != "" {
		base := c.PublicURL
		if base == "" {
			base = "http://" + c.Listen
		}
		// (printed, never logged as JSON: it makes the owner)
		fmt.Fprintf(os.Stderr, "\nThe fleet has no owner yet. Set it up at:\n\n  %s/setup?code=%s\n\n", base, code)
	}
	go fleet.Run(ctx)
	fleet.WatchExternals(ctx)
	go func() {
		time.Sleep(10 * time.Minute)
		update.MarkGood(binDir, version)
	}()
	httpServer := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	errs := make(chan error, 1)
	go func() {
		if c.TLSCert != "" {
			errs <- httpServer.ServeTLS(ln, c.TLSCert, c.TLSKey)
		} else {
			errs <- httpServer.Serve(ln)
		}
	}()
	log.Info("serving", "listen", c.Listen, "version", version, "db", c.DB, "masters", len(masters))
	select {
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpServer.Shutdown(shutdown)
	log.Info("stopped")
	return nil
}

// fileOf is a listener's file (handed to an update's new process).
func fileOf(ln net.Listener) *os.File {
	f, err := ln.(*net.TCPListener).File()
	if err != nil {
		panic(err)
	}
	ln.Close()
	return f
}

func runAgent(args []string) error {
	set := flag.NewFlagSet("agent", flag.ExitOnError)
	values := config.Flags(set, config.Agent)
	join := set.String("join", "", "enrol: the control plane's URL")
	token := set.String("token", "", "enrol: the join token")
	envFile := set.String("env-file", "", "enrol: the environment file to write (/etc/opence-fleet/agent.env)")
	set.Usage = func() {
		fmt.Fprint(os.Stderr, "opence-fleet agent [options]\nopence-fleet agent --join <url> --token <join token>\n\n"+config.Usage(config.Agent))
	}
	set.Parse(args)
	c, err := config.LoadAgent(values)
	if err != nil {
		return err
	}
	log := logger(c.LogLevel)
	client, err := agent.HTTPClient(c.CAFile)
	if err != nil {
		return fmt.Errorf("FLEET_AGENT_CA_FILE: %w", err)
	}
	if *join != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		answer, err := agent.Join(ctx, client, *join, *token, c.Name, c.Runtime)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "enrolled as node %s\n", answer.Node)
		env := map[string]string{"FLEET_AGENT_URL": strings.TrimRight(*join, "/"), "FLEET_AGENT_TOKEN": answer.Token,
			"FLEET_AGENT_NAME": answer.Node, "FLEET_AGENT_DIR": c.Dir, "FLEET_AGENT_RUNTIME": c.Runtime,
			"FLEET_AGENT_DATA_ROOT": c.DataRoot, "FLEET_AGENT_CA_FILE": c.CAFile}
		if *envFile == "" {
			for _, k := range []string{"FLEET_AGENT_URL", "FLEET_AGENT_TOKEN", "FLEET_AGENT_NAME"} {
				fmt.Printf("%s=%s\n", k, env[k])
			}
			return nil
		}
		return agent.WriteEnv(*envFile, env)
	}
	if err := os.MkdirAll(c.Dir, 0o750); err != nil {
		return err
	}
	update.Launch(filepath.Join(c.Dir, "bin"), version, log)
	if goruntime.GOARCH != "amd64" {
		log.Warn("this machine is not amd64: the game is 32-bit x86, and a node of another kind runs no server", "arch", goruntime.GOARCH)
	}
	var rt runtime.Runtime
	switch c.Runtime {
	case "docker":
		docker, err := dockerapi.New(c.DockerHost)
		if err != nil {
			return err
		}
		ping, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = docker.Ping(ping)
		cancel()
		if err != nil {
			return fmt.Errorf("Docker does not answer at %s (%v): is it installed and is the agent's user in the docker group? "+
				"(FLEET_AGENT_RUNTIME=process runs the servers without Docker)", c.DockerHost, err)
		}
		rt = container.New(docker, c.Name, log)
	default:
		// (the process runtime fetches its releases from the control plane,
		// through the agent's client and token)
		var a *agent.Agent
		rt = process.New(filepath.Join(c.Dir, "releases"),
			func(ctx context.Context, url, path, sha string, progress runtime.Progress) error {
				return a.Download(ctx, url, path, sha, progress)
			}, log)
		a = agent.New(c, version, log, rt, client)
		return run(a, log)
	}
	return run(agent.New(c, version, log, rt, client), log)
}

func dbAudit(actor, action, target string) db.InsertAuditParams {
	return db.InsertAuditParams{Time: time.Now().Unix(), Actor: actor, Action: action, Target: target}
}

func run(a *agent.Agent, log *slog.Logger) error {
	key, _ := hex.DecodeString(update.ReleaseKey)
	if k := os.Getenv("FLEET_UPDATE_KEY"); k != "" {
		key, _ = hex.DecodeString(k)
	}
	a.ReleaseKey = ed25519.PublicKey(key)
	a.Restart = func(binary string) {
		if err := update.Exec(binary, nil); err != nil {
			log.Error("the agent's update did not run", "err", err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("the agent starts", "node", a.Config.Name, "runtime", a.Runtime.Name(), "url", a.Config.URL, "version", version)
	return a.Run(ctx)
}

func health(args []string) error {
	set := flag.NewFlagSet("health", flag.ExitOnError)
	values := config.Flags(set, config.Serve)
	set.Parse(args)
	c, err := config.LoadServe(values)
	if err != nil {
		return err
	}
	scheme := "http"
	if c.TLSCert != "" {
		scheme = "https"
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	response, err := client.Get(scheme + "://" + c.Listen + "/healthz")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: %s", response.Status)
	}
	fmt.Println("healthy")
	return nil
}

// joinToken makes a node's join token beside the control plane (the
// installer's first node): the token alone with --quiet.
func joinToken(args []string) error {
	set := flag.NewFlagSet("join-token", flag.ExitOnError)
	values := config.Flags(set, config.Serve)
	defaults := spec.DefaultNode()
	runtimeName := set.String("runtime", "docker", "docker or process")
	tunnelPorts := set.String("tunnel-ports", defaults.TunnelPorts.String(), "the node's tunnel ports (UDP), first-last")
	webPorts := set.String("web-ports", defaults.WebPorts.String(), "the node's web ports (TCP, on loopback), first-last")
	loopbackBase := set.Int("loopback-base", defaults.LoopbackBase, "the process runtime's first loopback address, 127.0.0.<n>")
	publicAddress := set.String("public-address", "", "the node's public address, when it is not the one the masters see")
	quiet := set.Bool("quiet", false, "print the token alone")
	set.Parse(args)
	if set.NArg() != 1 {
		return usageError("usage: opence-fleet join-token [--runtime docker|process] [--tunnel-ports 2302-2399] " +
			"[--web-ports 8101-8199] [--loopback-base 2] [--quiet] <node name>")
	}
	c, err := config.LoadServe(values)
	if err != nil {
		return err
	}
	s, err := store.Open(c.DB)
	if err != nil {
		return err
	}
	defer s.Close()
	key, _, err := crypt.Load(c.SecretKey)
	if err != nil {
		return err
	}
	f := core.New(s, key, c, slog.New(slog.NewTextHandler(io.Discard, nil)), live.New())
	settings := defaults
	settings.Runtime = *runtimeName
	settings.LoopbackBase, settings.PublicAddress = *loopbackBase, *publicAddress
	if settings.TunnelPorts, err = spec.ParsePortRange(*tunnelPorts); err != nil {
		return fmt.Errorf("--tunnel-ports: %w", err)
	}
	if settings.WebPorts, err = spec.ParsePortRange(*webPorts); err != nil {
		return fmt.Errorf("--web-ports: %w", err)
	}
	token, err := f.JoinToken(context.Background(), "cli", set.Arg(0), settings)
	if err != nil {
		return err
	}
	if *quiet {
		fmt.Println(token)
		return nil
	}
	url := c.PublicURL
	if url == "" {
		url = "http://" + c.Listen
	}
	fmt.Printf("curl -fsSL %s/agent.sh | sudo sh -s -- --join %s --token %s\n", url, url, token)
	return nil
}

// releaseZip keeps a halo-linux-release.zip of the operator's own (a build
// for the process runtime), beside the control plane; --image names the
// container image of the same build (one the nodes have).
func releaseZip(args []string) error {
	set := flag.NewFlagSet("release-zip", flag.ExitOnError)
	values := config.Flags(set, config.Serve)
	latest := set.Bool("latest", false, "make it latest")
	image := set.String("build-image", "", "the build's image (on the nodes, or a registry's)")
	set.Parse(args)
	if set.NArg() != 2 {
		return errors.New("usage: opence-fleet release-zip [--latest] [--build-image ref] <build> <halo-linux-release.zip>")
	}
	c, err := config.LoadServe(values)
	if err != nil {
		return err
	}
	s, err := store.Open(c.DB)
	if err != nil {
		return err
	}
	defer s.Close()
	key, _, err := crypt.Load(c.SecretKey)
	if err != nil {
		return err
	}
	f := core.New(s, key, c, slog.New(slog.NewTextHandler(io.Discard, nil)), live.New())
	ctx := context.Background()
	build := set.Arg(0)
	if *image != "" {
		if err := f.AddLocalRelease(ctx, "cli", build, *image, *latest); err != nil {
			return err
		}
	}
	if err := f.AddZip(ctx, "cli", build, set.Arg(1)); err != nil {
		return err
	}
	if *latest {
		if err := f.SetLatest(ctx, "cli", build); err != nil {
			return err
		}
	}
	fmt.Printf("release %s is kept (restart the control plane, or wait for its next change, for the nodes to get it)\n", build)
	return nil
}

func setupLink(args []string) error {
	c, s, _, err := openFleet(args, "setup-link")
	if err != nil {
		return err
	}
	defer s.Close()
	code, err := auth.New(s, c.SessionHours).SetupCode(context.Background())
	if err != nil {
		return err
	}
	if code == "" {
		return errors.New("the fleet has users: sign in, or reset a password (opence-fleet reset-password <user>)")
	}
	base := c.PublicURL
	if base == "" {
		base = "http://" + c.Listen
	}
	fmt.Printf("%s/setup?code=%s\n", base, code)
	return nil
}

func resetPassword(args []string) error {
	set := flag.NewFlagSet("reset-password", flag.ExitOnError)
	values := config.Flags(set, config.Serve)
	set.Parse(args)
	if set.NArg() != 1 {
		return errors.New("usage: opence-fleet reset-password <user> (the password from FLEET_PASSWORD, or typed)")
	}
	c, err := config.LoadServe(values)
	if err != nil {
		return err
	}
	s, err := store.Open(c.DB)
	if err != nil {
		return err
	}
	defer s.Close()
	password := os.Getenv("FLEET_PASSWORD")
	if password == "" {
		fmt.Fprint(os.Stderr, "the new password: ")
		var line string
		fmt.Scanln(&line)
		password = line
	}
	if err := auth.New(s, c.SessionHours).ResetPassword(context.Background(), set.Arg(0), password); err != nil {
		return err
	}
	s.InsertAudit(context.Background(), dbAudit("cli", "user.reset_password", set.Arg(0)))
	fmt.Println("the password is set; the user's sessions are ended")
	return nil
}
