// Package process is the agent's fallback runtime, for a node without
// Docker: each server is a child process of the agent, the release's
// `halo --server` (halo-linux-release.zip, from the control plane, its
// SHA-256 checked), with the same files as a container's and its own
// loopback address for system link (127.0.0.<n>; Linux's lo answers all
// of 127/8). The children share the agent's user and sandbox. A self-update
// hands their pids and pipes to the new program (exec keeps the pid, so the
// children stay its own).
package process

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agent/runtime"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

// HandoffVariable carries the children to the program a self-update runs.
const HandoffVariable = "FLEET_AGENT_HANDOFF"

// Download fetches a file of the control plane into path, checking its
// SHA-256.
type Download func(ctx context.Context, url, path, sha256 string, progress runtime.Progress) error

// Runtime runs servers as child processes.
type Runtime struct {
	// releases/ in the node folder
	Dir      string
	Download Download
	Log      *slog.Logger

	mu       sync.Mutex
	children map[string]*handle
}

// New makes the runtime.
func New(dir string, download Download, log *slog.Logger) *Runtime {
	return &Runtime{Dir: dir, Download: download, Log: log, children: map[string]*handle{}}
}

// Name is the runtime's name.
func (r *Runtime) Name() string { return "process" }

// Describe is the runtime's line for the hello.
func (r *Runtime) Describe(context.Context) string { return "process (the release's halo --server)" }

func (r *Runtime) releaseDir(build string) string { return filepath.Join(r.Dir, build) }

// Ready tells whether a release is unpacked and checked.
func (r *Runtime) Ready(_ context.Context, build string) bool {
	_, err := os.Stat(filepath.Join(r.releaseDir(build), ".checked"))
	return err == nil
}

// Fetch downloads a release's zip from the control plane, checks it,
// unpacks it and runs its `halo --version`.
func (r *Runtime) Fetch(ctx context.Context, _ spec.Image, release spec.Release, progress runtime.Progress) error {
	if r.Ready(ctx, release.Build) {
		return nil
	}
	if release.URL == "" || release.SHA256 == "" {
		return fmt.Errorf("build %s has no halo-linux-release.zip on the control plane (fetch it on the Releases page)",
			release.Build)
	}
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return err
	}
	zipPath := filepath.Join(r.Dir, release.Build+".zip")
	if err := r.Download(ctx, release.URL, zipPath, release.SHA256, progress); err != nil {
		return err
	}
	defer os.Remove(zipPath)
	dir := r.releaseDir(release.Build)
	part := dir + ".part"
	os.RemoveAll(part)
	if err := Unzip(zipPath, part); err != nil {
		os.RemoveAll(part)
		return err
	}
	check, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(check, filepath.Join(part, "halo"), "--version")
	cmd.Env = []string{"LD_LIBRARY_PATH=" + part, "HOME=" + part, "PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), "OpenCE ") {
		os.RemoveAll(part)
		return fmt.Errorf("the release's halo --version did not answer (is the 32-bit C runtime, libc6:i386 "+
			"and libgcc-s1:i386, installed?): %v %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.WriteFile(filepath.Join(part, ".checked"), out, 0o644); err != nil {
		return err
	}
	os.RemoveAll(dir)
	return os.Rename(part, dir)
}

// Unzip unpacks a release's zip into dir, refusing a path that leaves it.
func Unzip(path, dir string) error {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer archive.Close()
	for _, f := range archive.File {
		name := filepath.Clean(f.Name)
		if name == "." || strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("the zip's %q leaves its folder", f.Name)
		}
		target := filepath.Join(dir, name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if f.FileInfo().Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		base := filepath.Base(name)
		if f.FileInfo().Mode()&0o111 != 0 || base == "halo" || base == "halo-server" || base == "map_validate" {
			mode = 0o755
		}
		in, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
		if err != nil {
			in.Close()
			return err
		}
		_, err = io.Copy(out, io.LimitReader(in, 1<<30))
		in.Close()
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Environment is a child's environment: the fleet's settings and the
// instance's paths, ports and loopback address.
func Environment(instance spec.Instance, paths runtime.Paths, release string) []string {
	env := map[string]string{
		"PATH":            "/usr/local/bin:/usr/bin:/bin",
		"LD_LIBRARY_PATH": release,
	}
	for k, v := range instance.Env {
		env[k] = v
	}
	env["HALO_CONFIG_FILE"] = filepath.Join(paths.Instance, "config.toml")
	env["HALO_DATA_ROOT"] = paths.Data
	env["HALO_SAVE_ROOT"] = filepath.Join(paths.Instance, "saves")
	env["HALO_LOG_ROOT"] = filepath.Join(paths.Instance, "logs")
	env["HOME"] = paths.Instance
	env["HALO_SERVER_INIT_FILE"] = filepath.Join(paths.Instance, "init.txt")
	env["HALO_NET_TUNNEL_PORT"] = strconv.Itoa(instance.TunnelPort)
	env["HALO_SERVER_WEB_PORT"] = strconv.Itoa(instance.WebPort)
	env["HALO_SERVER_WEB_BIND"] = "127.0.0.1"
	env["HALO_NET_ADDRESS"] = fmt.Sprintf("127.0.0.%d", instance.Loopback)
	env["HALO_NET_BROADCAST"] = "127.0.0.1"
	if instance.OwnerPassword != "" {
		env["HALO_SERVER_WEB_OWNER_PASSWORD"] = instance.OwnerPassword
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// Start runs the instance's server as a child.
func (r *Runtime) Start(ctx context.Context, instance spec.Instance, _ spec.Node, paths runtime.Paths) (runtime.Handle, error) {
	release := r.releaseDir(instance.Build)
	if !r.Ready(ctx, instance.Build) {
		return nil, fmt.Errorf("release %s is not on the node", instance.Build)
	}
	cmd := exec.Command(filepath.Join(release, "halo"), "--server")
	cmd.Dir = paths.Instance
	cmd.Env = Environment(instance, paths, release)
	// (its own process group: a terminal's ^C to the agent is not the games')
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// (pipes of our own: the parent's ends are *os.File, which a self-update
	// hands on)
	var parent, child [3]*os.File
	for i := range parent {
		r, w, err := os.Pipe()
		if err != nil {
			for j := 0; j < i; j++ {
				parent[j].Close()
				child[j].Close()
			}
			return nil, err
		}
		if i == 0 {
			parent[i], child[i] = w, r
		} else {
			parent[i], child[i] = r, w
		}
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = child[0], child[1], child[2]
	err := cmd.Start()
	for _, f := range child {
		f.Close()
	}
	if err != nil {
		for _, f := range parent {
			f.Close()
		}
		return nil, err
	}
	stdin, stdout, stderr := parent[0], parent[1], parent[2]
	h := &handle{r: r, slug: instance.Slug, generation: instance.Generation, build: instance.Build,
		started: time.Now(), process: cmd.Process, done: make(chan struct{}),
		stdin: stdin, stdout: stdout, stderr: stderr,
		web: fmt.Sprintf("http://127.0.0.1:%d", instance.WebPort), loopback: instance.Loopback, webPort: instance.WebPort}
	go h.reap()
	r.mu.Lock()
	r.children[instance.Slug] = h
	r.mu.Unlock()
	return h, nil
}

// handoff is a child as the self-update hands it on.
type handoff struct {
	Slug       string `json:"slug"`
	Pid        int    `json:"pid"`
	Stdin      int    `json:"stdin"`
	Stdout     int    `json:"stdout"`
	Stderr     int    `json:"stderr"`
	Generation int64  `json:"generation"`
	Build      string `json:"build"`
	Started    int64  `json:"started"`
	WebPort    int    `json:"web_port"`
	Loopback   int    `json:"loopback"`
}

// Handoff makes the children's pipes survive an exec and writes them in the
// environment the new program reads (Adopt).
func (r *Runtime) Handoff() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var list []handoff
	for _, h := range r.children {
		select {
		case <-h.done:
			continue
		default:
		}
		for _, f := range []*os.File{h.stdin, h.stdout, h.stderr} {
			if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_SETFD, 0); errno != 0 {
				return errno
			}
		}
		list = append(list, handoff{Slug: h.slug, Pid: h.process.Pid, Stdin: int(h.stdin.Fd()), Stdout: int(h.stdout.Fd()),
			Stderr: int(h.stderr.Fd()), Generation: h.generation, Build: h.build, Started: h.started.Unix(),
			WebPort: h.webPort, Loopback: h.loopback})
	}
	data, _ := json.Marshal(list)
	return os.Setenv(HandoffVariable, string(data))
}

// Adopt takes the children a self-update handed on.
func (r *Runtime) Adopt(context.Context) (map[string]runtime.Handle, []string, error) {
	text := os.Getenv(HandoffVariable)
	os.Unsetenv(HandoffVariable)
	out := map[string]runtime.Handle{}
	if text == "" {
		return out, nil, nil
	}
	var list []handoff
	if err := json.Unmarshal([]byte(text), &list); err != nil {
		return out, nil, fmt.Errorf("%s does not parse: %w", HandoffVariable, err)
	}
	for _, c := range list {
		process, err := os.FindProcess(c.Pid)
		if err != nil || process.Signal(syscall.Signal(0)) != nil {
			continue
		}
		h := &handle{r: r, slug: c.Slug, generation: c.Generation, build: c.Build, started: time.Unix(c.Started, 0),
			process: process, done: make(chan struct{}), stdin: os.NewFile(uintptr(c.Stdin), "stdin"),
			stdout: os.NewFile(uintptr(c.Stdout), "stdout"), stderr: os.NewFile(uintptr(c.Stderr), "stderr"),
			web: fmt.Sprintf("http://127.0.0.1:%d", c.WebPort), webPort: c.WebPort, loopback: c.Loopback}
		for _, f := range []*os.File{h.stdin, h.stdout, h.stderr} {
			syscall.CloseOnExec(int(f.Fd()))
		}
		go h.reap()
		r.mu.Lock()
		r.children[c.Slug] = h
		r.mu.Unlock()
		out[c.Slug] = h
	}
	return out, nil, nil
}

// Remove forgets an ended child.
func (r *Runtime) Remove(_ context.Context, slug string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h := r.children[slug]; h != nil {
		select {
		case <-h.done:
			delete(r.children, slug)
		default:
		}
	}
	return nil
}

// Prune removes the releases no instance names, but the newest other.
func (r *Runtime) Prune(_ context.Context, keep []string) error {
	kept := map[string]bool{}
	for _, b := range keep {
		kept[b] = true
	}
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	type release struct {
		name string
		when time.Time
	}
	var list []release
	for _, e := range entries {
		if !e.IsDir() || kept[e.Name()] || strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		info, err := e.Info()
		if err == nil {
			list = append(list, release{e.Name(), info.ModTime()})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].when.After(list[j].when) })
	for i, rel := range list {
		if i > 0 {
			os.RemoveAll(filepath.Join(r.Dir, rel.name))
		}
	}
	return nil
}

type handle struct {
	r                     *Runtime
	slug                  string
	generation            int64
	build                 string
	started               time.Time
	process               *os.Process
	stdin, stdout, stderr *os.File
	web                   string
	webPort, loopback     int
	done                  chan struct{}
	code                  int
	usageMu               sync.Mutex
	lastTicks             int64
	lastSample            time.Time
}

func (h *handle) reap() {
	state, err := h.process.Wait()
	switch {
	case err != nil:
		h.code = 1
	case state.Exited():
		h.code = state.ExitCode()
	default:
		if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			h.code = 128 + int(status.Signal())
		} else {
			h.code = 1
		}
	}
	h.stdin.Close()
	close(h.done)
}

func (h *handle) ID() string         { return strconv.Itoa(h.process.Pid) }
func (h *handle) Slug() string       { return h.slug }
func (h *handle) Generation() int64  { return h.generation }
func (h *handle) Build() string      { return h.build }
func (h *handle) Started() time.Time { return h.started }
func (h *handle) Stdin() io.Writer   { return h.stdin }
func (h *handle) Web() string        { return h.web }

func (h *handle) Output(stdout, stderr io.Writer) error {
	errs := make(chan error, 2)
	go func() { _, err := io.Copy(stderr, h.stderr); errs <- err }()
	_, err := io.Copy(stdout, h.stdout)
	if err2 := <-errs; err == nil {
		err = err2
	}
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

func (h *handle) Wait(ctx context.Context) (int, error) {
	select {
	case <-h.done:
		return h.code, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (h *handle) Stop(ctx context.Context, grace time.Duration) error {
	select {
	case <-h.done:
		return nil
	default:
	}
	_ = h.process.Signal(syscall.SIGTERM)
	select {
	case <-h.done:
		return nil
	case <-time.After(grace):
		_ = h.process.Signal(syscall.SIGKILL)
	case <-ctx.Done():
		_ = h.process.Signal(syscall.SIGKILL)
	}
	<-h.done
	return nil
}

func (h *handle) Signal(_ context.Context, signal string) error {
	switch strings.ToUpper(strings.TrimPrefix(strings.ToUpper(signal), "SIG")) {
	case "HUP":
		return h.process.Signal(syscall.SIGHUP)
	case "USR1":
		return h.process.Signal(syscall.SIGUSR1)
	case "TERM":
		return h.process.Signal(syscall.SIGTERM)
	case "KILL":
		return h.process.Signal(syscall.SIGKILL)
	}
	return fmt.Errorf("signal %s: hup, usr1, term or kill", signal)
}

// Usage reads /proc: the resident memory, and the CPU since the last call.
func (h *handle) Usage(context.Context) (int64, float64) {
	pid := h.process.Pid
	statm, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0, 0
	}
	var size, resident int64
	fmt.Sscan(string(statm), &size, &resident)
	rss := resident * int64(os.Getpagesize())
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return rss, 0
	}
	// (the fields after the command's closing parenthesis; utime and stime
	// are the 12th and 13th of them)
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
	if len(fields) < 13 {
		return rss, 0
	}
	utime, _ := strconv.ParseInt(fields[11], 10, 64)
	stime, _ := strconv.ParseInt(fields[12], 10, 64)
	ticks := utime + stime
	h.usageMu.Lock()
	defer h.usageMu.Unlock()
	now := time.Now()
	cpu := 0.0
	if !h.lastSample.IsZero() {
		// (100 ticks a second on Linux)
		cpu = float64(ticks-h.lastTicks) / 100 / now.Sub(h.lastSample).Seconds() * 100
	}
	h.lastTicks, h.lastSample = ticks, now
	return rss, cpu
}

func (h *handle) Detach() error { return nil }

// SHA256File is a file's SHA-256 in hexadecimal.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
