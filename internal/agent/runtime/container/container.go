// Package container is the agent's default runtime: each server is a
// container of the project's server image (deploy/docker in the game's
// repository), made and watched through the Docker Engine API. /data is the
// node's game data, read-only; /state the instance's folder; the tunnel
// port is published one to one on every address, the web port on the
// node's loopback alone; the container has no Docker restart policy (the
// agent restarts by the exit code), a read-only root, no capabilities, no
// new privileges, and its memory and CPU limits.
package container

import (
	"context"
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
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agent/runtime"
	"github.com/OpenCommunityEdition/opence-fleet/internal/dockerapi"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

// The labels of the fleet's containers.
const (
	LabelSlug       = "opence-fleet.slug"
	LabelGeneration = "opence-fleet.generation"
	LabelBuild      = "opence-fleet.build"
	LabelNode       = "opence-fleet.node"
)

// The web port inside the image. (The tunnel's is the instance's own, the
// same inside and out: the server tells the masters the port its socket
// has, which their probe and its players reach.)
const webPort = 8080

// CosignIdentity is the server image's signer: the game's image workflow.
const CosignIdentity = `^https://github\.com/OpenCommunityEdition/OpenCE/\.github/workflows/server-image\.yml@`

// Runtime runs servers as containers.
type Runtime struct {
	Docker *dockerapi.Client
	Node   string
	Log    *slog.Logger

	mu      sync.Mutex
	images  map[string]string // build → the reference pulled
	fetched map[string]time.Time
	stats   map[string]cpuSample
}

type cpuSample struct {
	total, system int64
}

// New makes the runtime.
func New(docker *dockerapi.Client, node string, log *slog.Logger) *Runtime {
	return &Runtime{Docker: docker, Node: node, Log: log, images: map[string]string{}, fetched: map[string]time.Time{},
		stats: map[string]cpuSample{}}
}

// Name is the runtime's name.
func (r *Runtime) Name() string { return "docker" }

// Describe is Docker's version.
func (r *Runtime) Describe(ctx context.Context) string {
	v, err := r.Docker.ServerVersion(ctx)
	if err != nil {
		return "docker: " + err.Error()
	}
	return "docker " + v.Version + " (API " + v.APIVersion + ")"
}

func (r *Runtime) reference(build string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.images[build]
}

// Ready tells whether a build's image is on the node.
func (r *Runtime) Ready(ctx context.Context, build string) bool {
	ref := r.reference(build)
	if ref == "" {
		return false
	}
	_, err := r.Docker.InspectImage(ctx, ref)
	return err == nil
}

// Fetch pulls a build's image by its digest (unless it is on the node),
// and checks its signature with cosign when the node has cosign.
func (r *Runtime) Fetch(ctx context.Context, image spec.Image, _ spec.Release, progress runtime.Progress) error {
	ref := image.Pull()
	if ref == "" {
		return fmt.Errorf("build %s has no image", image.Build)
	}
	r.mu.Lock()
	r.images[image.Build] = ref
	r.mu.Unlock()
	if _, err := r.Docker.InspectImage(ctx, ref); err == nil {
		return nil
	} else if !dockerapi.IsNotFound(err) {
		return err
	}
	layers := map[string][2]int64{}
	err := r.Docker.Pull(ctx, ref, func(p dockerapi.PullProgress) {
		if p.ID == "" || progress == nil {
			return
		}
		if p.ProgressDetail.Total > 0 && strings.HasPrefix(p.Status, "Downloading") {
			layers[p.ID] = [2]int64{p.ProgressDetail.Current, p.ProgressDetail.Total}
		} else if p.Status == "Download complete" || p.Status == "Pull complete" {
			l := layers[p.ID]
			layers[p.ID] = [2]int64{l[1], l[1]}
		}
		var done, total int64
		for _, l := range layers {
			done, total = done+l[0], total+l[1]
		}
		progress(done, total)
	})
	if err != nil {
		return err
	}
	if err := verify(ctx, ref); err != nil {
		_ = r.Docker.RemoveImage(context.Background(), ref)
		return err
	}
	r.mu.Lock()
	r.fetched[image.Build] = time.Now()
	r.mu.Unlock()
	return nil
}

// verify runs `cosign verify` on the project's images when cosign is on the
// node (the digest and the registry's TLS are the check otherwise).
func verify(ctx context.Context, ref string) error {
	if !strings.HasPrefix(ref, "ghcr.io/opencommunityedition/") || !strings.Contains(ref, "@sha256:") {
		return nil
	}
	cosign, err := exec.LookPath("cosign")
	if err != nil {
		return nil
	}
	check, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(check, cosign, "verify", ref, "--certificate-identity-regexp", CosignIdentity,
		"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com").CombinedOutput()
	if err != nil {
		return fmt.Errorf("cosign refused %s's signature: %s", ref, strings.TrimSpace(string(out)))
	}
	return nil
}

func containerName(slug string) string { return "opence-" + slug }

// user is who the container runs as: the agent's own user (its folders are
// the agent's), or the image's 10001 when the agent is root (the folder is
// then given to 10001).
func user(paths runtime.Paths) (string, error) {
	if os.Geteuid() != 0 {
		return fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid()), nil
	}
	err := filepath.Walk(paths.Instance, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, 10001, 10001)
	})
	return "10001:10001", err
}

// Config is the container an instance's server runs in (OPENCE_FLEET_PLAN.md
// 12.2).
func Config(instance spec.Instance, node spec.Node, paths runtime.Paths, ref, runAs, nodeName string) dockerapi.ContainerConfig {
	env := map[string]string{}
	for k, v := range instance.Env {
		env[k] = v
	}
	env["HALO_CONFIG_FILE"] = "/state/config.toml"
	env["HALO_DATA_ROOT"] = "/data"
	env["HALO_SAVE_ROOT"] = "/state/saves"
	env["HALO_LOG_ROOT"] = "/state/logs"
	env["HOME"] = "/state"
	env["HALO_SERVER_INIT_FILE"] = "/state/init.txt"
	timeout := 15
	config := dockerapi.ContainerConfig{
		Image: ref, User: runAs, OpenStdin: true, AttachStdin: true, AttachStdout: true, AttachStderr: true,
		StopSignal: "SIGTERM", StopTimeout: &timeout,
		Labels: map[string]string{LabelSlug: instance.Slug, LabelGeneration: strconv.FormatInt(instance.Generation, 10),
			LabelBuild: instance.Build, LabelNode: nodeName},
	}
	h := &config.HostConfig
	h.Mounts = []dockerapi.Mount{
		{Type: "bind", Source: paths.Data, Target: "/data", ReadOnly: true},
		{Type: "bind", Source: paths.Instance, Target: "/state"},
	}
	if node.HostNetwork {
		// (the host's network: the instance's own ports, and its loopback
		// address for system link, as the process runtime has them)
		h.NetworkMode = "host"
		env["HALO_NET_TUNNEL_PORT"] = strconv.Itoa(instance.TunnelPort)
		env["HALO_SERVER_WEB_PORT"] = strconv.Itoa(instance.WebPort)
		env["HALO_SERVER_WEB_BIND"] = "127.0.0.1"
		env["HALO_NET_ADDRESS"] = fmt.Sprintf("127.0.0.%d", instance.Loopback)
		env["HALO_NET_BROADCAST"] = "127.0.0.1"
	} else {
		env["HALO_NET_TUNNEL_PORT"] = strconv.Itoa(instance.TunnelPort)
		env["HALO_SERVER_WEB_PORT"] = strconv.Itoa(webPort)
		env["HALO_SERVER_WEB_BIND"] = "0.0.0.0"
		udp, tcp := fmt.Sprintf("%d/udp", instance.TunnelPort), fmt.Sprintf("%d/tcp", webPort)
		config.ExposedPorts = map[string]struct{}{udp: {}, tcp: {}}
		h.PortBindings = map[string][]dockerapi.PortBinding{
			udp: {{HostPort: strconv.Itoa(instance.TunnelPort)}},
			tcp: {{HostIP: "127.0.0.1", HostPort: strconv.Itoa(instance.WebPort)}},
		}
	}
	if node.MemoryMB > 0 {
		h.Memory = int64(node.MemoryMB) << 20
		h.MemorySwap = h.Memory
	}
	if node.CPUs > 0 {
		h.NanoCPUs = int64(node.CPUs * 1e9)
	}
	pids := int64(256)
	h.PidsLimit = &pids
	h.ReadonlyRootfs = true
	h.Tmpfs = map[string]string{"/tmp": "rw,nosuid,nodev,noexec,size=64m"}
	h.CapDrop = []string{"ALL"}
	h.SecurityOpt = []string{"no-new-privileges:true"}
	h.LogConfig.Type = "json-file"
	h.LogConfig.Config = map[string]string{"max-size": "10m", "max-file": "1"}
	h.RestartPolicy.Name = "no"
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		config.Env = append(config.Env, k+"="+env[k])
	}
	return config
}

// Start makes the instance's container afresh from its spec and starts it,
// attached first so that no line is missed.
func (r *Runtime) Start(ctx context.Context, instance spec.Instance, node spec.Node, paths runtime.Paths) (runtime.Handle, error) {
	ref := r.reference(instance.Build)
	if ref == "" {
		return nil, fmt.Errorf("build %s is not on the node", instance.Build)
	}
	runAs, err := user(paths)
	if err != nil {
		return nil, err
	}
	name := containerName(instance.Slug)
	if err := r.Docker.Remove(ctx, name, true); err != nil {
		return nil, err
	}
	id, err := r.Docker.Create(ctx, name, Config(instance, node, paths, ref, runAs, r.Node))
	if err != nil {
		return nil, err
	}
	stream, err := r.Docker.Attach(ctx, id)
	if err != nil {
		_ = r.Docker.Remove(context.Background(), id, true)
		return nil, err
	}
	if err := r.Docker.Start(ctx, id); err != nil {
		stream.Close()
		_ = r.Docker.Remove(context.Background(), id, true)
		return nil, describePortError(err, instance)
	}
	return r.handle(id, instance.Slug, instance.Generation, instance.Build, time.Now(), stream, instance, node), nil
}

// describePortError names the port Docker could not publish (the server's
// exit code 4 would).
func describePortError(err error, instance spec.Instance) error {
	text := err.Error()
	if strings.Contains(text, "address already in use") || strings.Contains(text, "port is already allocated") {
		port := "the tunnel port " + strconv.Itoa(instance.TunnelPort) + "/udp"
		if strings.Contains(text, ":"+strconv.Itoa(instance.WebPort)) {
			port = "the web port " + strconv.Itoa(instance.WebPort) + "/tcp"
		}
		return &PortError{Port: port, Err: err}
	}
	return err
}

// PortError is a port Docker could not publish: the agent treats it as the
// server's exit 4.
type PortError struct {
	Port string
	Err  error
}

func (e *PortError) Error() string { return e.Port + " is in use: " + e.Err.Error() }

func (r *Runtime) handle(id, slug string, generation int64, build string, started time.Time, stream *dockerapi.Stream,
	instance spec.Instance, node spec.Node) *handle {
	web := fmt.Sprintf("http://127.0.0.1:%d", instance.WebPort)
	return &handle{r: r, id: id, slug: slug, generation: generation, build: build, started: started, stream: stream, web: web}
}

// Adopt finds the fleet's running containers, attaches to them, and
// returns the stopped ones' slugs (to be removed or started again).
func (r *Runtime) Adopt(ctx context.Context) (map[string]runtime.Handle, []string, error) {
	list, err := r.Docker.List(ctx, LabelSlug, true)
	if err != nil {
		return nil, nil, err
	}
	out := map[string]runtime.Handle{}
	var stopped []string
	for _, c := range list {
		slug := c.Labels[LabelSlug]
		if slug == "" || (c.Labels[LabelNode] != "" && c.Labels[LabelNode] != r.Node) {
			continue
		}
		if c.State != "running" {
			stopped = append(stopped, slug)
			continue
		}
		info, err := r.Docker.Inspect(ctx, c.ID)
		if err != nil {
			return nil, nil, err
		}
		stream, err := r.Docker.Attach(ctx, c.ID)
		if err != nil {
			return nil, nil, err
		}
		generation, _ := strconv.ParseInt(c.Labels[LabelGeneration], 10, 64)
		build := c.Labels[LabelBuild]
		if build != "" {
			r.mu.Lock()
			if r.images[build] == "" {
				r.images[build] = info.Config.Image
			}
			r.mu.Unlock()
		}
		var instance spec.Instance
		instance.Slug = slug
		// (the web port is what Docker published on the loopback)
		for port, bindings := range info.HostConfig.PortBindings {
			if strings.HasSuffix(port, "/tcp") && len(bindings) > 0 {
				instance.WebPort, _ = strconv.Atoi(bindings[0].HostPort)
			}
		}
		if info.HostConfig.NetworkMode == "host" {
			for _, e := range info.Config.Env {
				if v, ok := strings.CutPrefix(e, "HALO_SERVER_WEB_PORT="); ok {
					instance.WebPort, _ = strconv.Atoi(v)
				}
			}
		}
		out[slug] = r.handle(c.ID, slug, generation, build, info.State.Started(), stream, instance, spec.Node{})
	}
	return out, stopped, nil
}

// Remove removes an instance's container.
func (r *Runtime) Remove(ctx context.Context, slug string) error {
	return r.Docker.Remove(ctx, containerName(slug), true)
}

// Prune removes the images of builds no instance names, but the newest of
// them (a rollback's), from the images this agent fetched.
func (r *Runtime) Prune(ctx context.Context, keep []string) error {
	kept := map[string]bool{}
	for _, b := range keep {
		kept[b] = true
	}
	r.mu.Lock()
	type candidate struct {
		build, ref string
		when       time.Time
	}
	var list []candidate
	for build, ref := range r.images {
		if !kept[build] {
			list = append(list, candidate{build, ref, r.fetched[build]})
		}
	}
	r.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].when.After(list[j].when) })
	var errs []error
	for i, c := range list {
		if i == 0 {
			continue
		}
		if err := r.Docker.RemoveImage(ctx, c.ref); err != nil && !dockerapi.IsNotFound(err) && !dockerapi.IsConflict(err) {
			errs = append(errs, err)
			continue
		}
		r.mu.Lock()
		delete(r.images, c.build)
		delete(r.fetched, c.build)
		r.mu.Unlock()
	}
	return errors.Join(errs...)
}

type handle struct {
	r          *Runtime
	id, slug   string
	generation int64
	build      string
	started    time.Time
	stream     *dockerapi.Stream
	web        string
}

func (h *handle) ID() string         { return h.id }
func (h *handle) Slug() string       { return h.slug }
func (h *handle) Generation() int64  { return h.generation }
func (h *handle) Build() string      { return h.build }
func (h *handle) Started() time.Time { return h.started }
func (h *handle) Stdin() io.Writer   { return h.stream }
func (h *handle) Web() string        { return h.web }

func (h *handle) Output(stdout, stderr io.Writer) error { return h.stream.Demux(stdout, stderr) }

func (h *handle) Wait(ctx context.Context) (int, error) {
	code, err := h.r.Docker.Wait(ctx, h.id, "not-running")
	if err != nil && dockerapi.IsNotFound(err) {
		return 137, nil
	}
	return code, err
}

func (h *handle) Stop(ctx context.Context, grace time.Duration) error {
	err := h.r.Docker.Stop(ctx, h.id, grace)
	if dockerapi.IsNotFound(err) {
		return nil
	}
	return err
}

func (h *handle) Signal(ctx context.Context, signal string) error {
	return h.r.Docker.Kill(ctx, h.id, strings.ToUpper(strings.TrimPrefix(strings.ToUpper(signal), "SIG")))
}

func (h *handle) Usage(ctx context.Context) (int64, float64) {
	s, err := h.r.Docker.ContainerStats(ctx, h.id)
	if err != nil {
		return 0, 0
	}
	return s.Memory, s.CPU
}

func (h *handle) Detach() error { return h.stream.Close() }
