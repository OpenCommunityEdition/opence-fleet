// Package dockerapi is a thin client of the Docker Engine API (JSON over
// HTTP on the daemon's unix socket): what the agent's container runtime
// needs, images pulled by digest, containers made, attached, started,
// waited on, stopped, inspected and removed. It is a few hundred lines
// rather than the docker/docker module and its dependencies.
package dockerapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Version is the API version asked for (Docker 20.10 and later speak it).
const Version = "v1.41"

// Client talks to a Docker daemon.
type Client struct {
	// unix:///var/run/docker.sock, or tcp://host:port
	Host string
	http *http.Client
	dial func(ctx context.Context) (net.Conn, error)
	base string
}

// New makes a client of a daemon's address.
func New(host string) (*Client, error) {
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	c := &Client{Host: host}
	switch {
	case strings.HasPrefix(host, "unix://"):
		path := strings.TrimPrefix(host, "unix://")
		c.dial = func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}
		c.base = "http://docker"
	case strings.HasPrefix(host, "tcp://"):
		address := strings.TrimPrefix(host, "tcp://")
		c.dial = func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", address)
		}
		c.base = "http://" + address
	default:
		return nil, fmt.Errorf("%q: the Docker host is unix:///path or tcp://host:port", host)
	}
	c.http = &http.Client{Transport: &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return c.dial(ctx) },
		MaxIdleConnsPerHost: 8,
	}}
	return c, nil
}

// Error is the daemon's refusal.
type Error struct {
	Status  int
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("docker: %s (%d)", e.Message, e.Status) }

// IsNotFound tells whether err is a 404 (no such image or container).
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// IsConflict tells whether err is a 409 (a name taken, a container
// running).
func IsConflict(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusConflict
}

func (c *Client) request(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	target := c.base + "/" + Version + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 400 {
		defer response.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		e := &Error{Status: response.StatusCode}
		if json.Unmarshal(data, e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
		}
		return nil, e
	}
	return response, nil
}

func (c *Client) call(ctx context.Context, method, path string, query url.Values, body, out any) error {
	response, err := c.request(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	return json.NewDecoder(response.Body).Decode(out)
}

// Ping checks the daemon answers.
func (c *Client) Ping(ctx context.Context) error {
	return c.call(ctx, http.MethodGet, "/_ping", nil, nil, nil)
}

// VersionInfo is GET /version's answer.
type VersionInfo struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion"`
	Os         string `json:"Os"`
	Arch       string `json:"Arch"`
}

// ServerVersion asks the daemon's version.
func (c *Client) ServerVersion(ctx context.Context) (VersionInfo, error) {
	var v VersionInfo
	return v, c.call(ctx, http.MethodGet, "/version", nil, nil, &v)
}

// PullProgress is a line of a pull's progress.
type PullProgress struct {
	Status         string `json:"status"`
	ID             string `json:"id"`
	Error          string `json:"error"`
	ProgressDetail struct {
		Current int64 `json:"current"`
		Total   int64 `json:"total"`
	} `json:"progressDetail"`
}

// Pull pulls an image (repository:tag or repository@sha256:…), telling
// progress as it goes.
func (c *Client) Pull(ctx context.Context, reference string, progress func(PullProgress)) error {
	repository, tag := SplitReference(reference)
	query := url.Values{"fromImage": {repository}}
	if tag != "" {
		query.Set("tag", tag)
	}
	response, err := c.request(ctx, http.MethodPost, "/images/create", query, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	for {
		var p PullProgress
		if err := decoder.Decode(&p); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if p.Error != "" {
			return fmt.Errorf("docker: pulling %s: %s", reference, p.Error)
		}
		if progress != nil {
			progress(p)
		}
	}
}

// SplitReference splits a reference into its repository and its tag or
// digest (a port in the registry's host is not a tag).
func SplitReference(reference string) (string, string) {
	if at := strings.Index(reference, "@"); at >= 0 {
		return reference[:at], reference[at+1:]
	}
	slash := strings.LastIndexByte(reference, '/')
	if colon := strings.LastIndexByte(reference, ':'); colon > slash {
		return reference[:colon], reference[colon+1:]
	}
	return reference, ""
}

// Image is GET /images/{name}/json's answer.
type Image struct {
	ID          string   `json:"Id"`
	RepoTags    []string `json:"RepoTags"`
	RepoDigests []string `json:"RepoDigests"`
	Created     string   `json:"Created"`
	Size        int64    `json:"Size"`
	Config      struct {
		User   string            `json:"User"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

// InspectImage asks an image (IsNotFound: it is not on the machine).
func (c *Client) InspectImage(ctx context.Context, name string) (*Image, error) {
	var image Image
	return &image, c.call(ctx, http.MethodGet, "/images/"+name+"/json", nil, nil, &image)
}

// ImageSummary is an image of GET /images/json.
type ImageSummary struct {
	ID          string            `json:"Id"`
	RepoTags    []string          `json:"RepoTags"`
	RepoDigests []string          `json:"RepoDigests"`
	Created     int64             `json:"Created"`
	Size        int64             `json:"Size"`
	Labels      map[string]string `json:"Labels"`
}

// Images lists the images whose reference matches (repository, without a
// tag).
func (c *Client) Images(ctx context.Context, reference string) ([]ImageSummary, error) {
	query := url.Values{}
	if reference != "" {
		filters, _ := json.Marshal(map[string][]string{"reference": {reference}})
		query.Set("filters", string(filters))
	}
	var out []ImageSummary
	return out, c.call(ctx, http.MethodGet, "/images/json", query, nil, &out)
}

// RemoveImage removes an image (one a container uses is refused, 409).
func (c *Client) RemoveImage(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodDelete, "/images/"+id, nil, nil, nil)
}

// PortBinding is a published port's host side.
type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// Mount is a bind mount.
type Mount struct {
	Type     string `json:"Type"`
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly"`
}

// HostConfig is a container's host side.
type HostConfig struct {
	Mounts         []Mount                  `json:"Mounts,omitempty"`
	PortBindings   map[string][]PortBinding `json:"PortBindings,omitempty"`
	NetworkMode    string                   `json:"NetworkMode,omitempty"`
	Memory         int64                    `json:"Memory,omitempty"`
	MemorySwap     int64                    `json:"MemorySwap,omitempty"`
	NanoCPUs       int64                    `json:"NanoCpus,omitempty"`
	PidsLimit      *int64                   `json:"PidsLimit,omitempty"`
	ReadonlyRootfs bool                     `json:"ReadonlyRootfs"`
	Tmpfs          map[string]string        `json:"Tmpfs,omitempty"`
	CapDrop        []string                 `json:"CapDrop,omitempty"`
	SecurityOpt    []string                 `json:"SecurityOpt,omitempty"`
	LogConfig      struct {
		Type   string            `json:"Type"`
		Config map[string]string `json:"Config,omitempty"`
	} `json:"LogConfig"`
	RestartPolicy struct {
		Name string `json:"Name"`
	} `json:"RestartPolicy"`
	Init *bool `json:"Init,omitempty"`
}

// ContainerConfig is a container's configuration (POST /containers/create).
type ContainerConfig struct {
	Image        string              `json:"Image"`
	Env          []string            `json:"Env,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	User         string              `json:"User,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	OpenStdin    bool                `json:"OpenStdin"`
	StdinOnce    bool                `json:"StdinOnce"`
	AttachStdin  bool                `json:"AttachStdin"`
	AttachStdout bool                `json:"AttachStdout"`
	AttachStderr bool                `json:"AttachStderr"`
	Tty          bool                `json:"Tty"`
	StopSignal   string              `json:"StopSignal,omitempty"`
	StopTimeout  *int                `json:"StopTimeout,omitempty"`
	HostConfig   HostConfig          `json:"HostConfig"`
}

// Create makes a container; its id.
func (c *Client) Create(ctx context.Context, name string, config ContainerConfig) (string, error) {
	var answer struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	query := url.Values{}
	if name != "" {
		query.Set("name", name)
	}
	err := c.call(ctx, http.MethodPost, "/containers/create", query, config, &answer)
	return answer.ID, err
}

// Start starts a container.
func (c *Client) Start(ctx context.Context, id string) error {
	err := c.call(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
	var e *Error
	// (304: started already)
	if errors.As(err, &e) && e.Status == http.StatusNotModified {
		return nil
	}
	return err
}

// Stop stops a container: its stop signal, then SIGKILL after the timeout.
func (c *Client) Stop(ctx context.Context, id string, timeout time.Duration) error {
	query := url.Values{"t": {strconv.Itoa(int(timeout / time.Second))}}
	return c.call(ctx, http.MethodPost, "/containers/"+id+"/stop", query, nil, nil)
}

// Kill sends a container's main process a signal (HUP, USR1, KILL).
func (c *Client) Kill(ctx context.Context, id, signal string) error {
	return c.call(ctx, http.MethodPost, "/containers/"+id+"/kill", url.Values{"signal": {signal}}, nil, nil)
}

// Remove removes a container (force: a running one too).
func (c *Client) Remove(ctx context.Context, id string, force bool) error {
	query := url.Values{}
	if force {
		query.Set("force", "1")
	}
	err := c.call(ctx, http.MethodDelete, "/containers/"+id, query, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// Wait waits for a container to stop (condition: next-exit, not-running);
// its exit code.
func (c *Client) Wait(ctx context.Context, id, condition string) (int, error) {
	var answer struct {
		StatusCode int `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	err := c.call(ctx, http.MethodPost, "/containers/"+id+"/wait", url.Values{"condition": {condition}}, nil, &answer)
	if err == nil && answer.Error != nil && answer.Error.Message != "" {
		err = errors.New("docker: " + answer.Error.Message)
	}
	return answer.StatusCode, err
}

// ContainerState is a container's state.
type ContainerState struct {
	Status     string `json:"Status"`
	Running    bool   `json:"Running"`
	Pid        int    `json:"Pid"`
	ExitCode   int    `json:"ExitCode"`
	OOMKilled  bool   `json:"OOMKilled"`
	Error      string `json:"Error"`
	StartedAt  string `json:"StartedAt"`
	FinishedAt string `json:"FinishedAt"`
}

// Container is GET /containers/{id}/json's answer.
type Container struct {
	ID      string         `json:"Id"`
	Name    string         `json:"Name"`
	Image   string         `json:"Image"`
	Created string         `json:"Created"`
	State   ContainerState `json:"State"`
	Config  struct {
		Image  string            `json:"Image"`
		Env    []string          `json:"Env"`
		Labels map[string]string `json:"Labels"`
		User   string            `json:"User"`
	} `json:"Config"`
	HostConfig      HostConfig `json:"HostConfig"`
	NetworkSettings struct {
		Ports map[string][]PortBinding `json:"Ports"`
	} `json:"NetworkSettings"`
	Mounts []struct {
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

// Inspect asks a container.
func (c *Client) Inspect(ctx context.Context, id string) (*Container, error) {
	var container Container
	return &container, c.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &container)
}

// Started is when the container's process started.
func (s ContainerState) Started() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s.StartedAt)
	return t
}

// ContainerSummary is a container of GET /containers/json.
type ContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
}

// List lists the containers (all: stopped ones too) that have a label.
func (c *Client) List(ctx context.Context, label string, all bool) ([]ContainerSummary, error) {
	query := url.Values{}
	if all {
		query.Set("all", "1")
	}
	if label != "" {
		filters, _ := json.Marshal(map[string][]string{"label": {label}})
		query.Set("filters", string(filters))
	}
	var out []ContainerSummary
	return out, c.call(ctx, http.MethodGet, "/containers/json", query, nil, &out)
}

// Stats is a container's memory and CPU use, once.
type Stats struct {
	Memory int64
	// the share of one CPU since the previous sample, in percent
	CPU float64
}

// ContainerStats asks a container's use once.
func (c *Client) ContainerStats(ctx context.Context, id string) (Stats, error) {
	var answer struct {
		MemoryStats struct {
			Usage int64            `json:"usage"`
			Stats map[string]int64 `json:"stats"`
		} `json:"memory_stats"`
		CPUStats struct {
			CPUUsage struct {
				TotalUsage int64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemUsage int64 `json:"system_cpu_usage"`
			OnlineCPUs  int64 `json:"online_cpus"`
		} `json:"cpu_stats"`
		PreCPUStats struct {
			CPUUsage struct {
				TotalUsage int64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemUsage int64 `json:"system_cpu_usage"`
		} `json:"precpu_stats"`
	}
	err := c.call(ctx, http.MethodGet, "/containers/"+id+"/stats", url.Values{"stream": {"0"}, "one-shot": {"0"}}, nil, &answer)
	if err != nil {
		return Stats{}, err
	}
	s := Stats{Memory: answer.MemoryStats.Usage - answer.MemoryStats.Stats["inactive_file"]}
	cpu := float64(answer.CPUStats.CPUUsage.TotalUsage - answer.PreCPUStats.CPUUsage.TotalUsage)
	system := float64(answer.CPUStats.SystemUsage - answer.PreCPUStats.SystemUsage)
	if system > 0 && cpu > 0 {
		cpus := float64(answer.CPUStats.OnlineCPUs)
		if cpus == 0 {
			cpus = 1
		}
		s.CPU = cpu / system * cpus * 100
	}
	return s, nil
}

// Stream is an attached container's standard input, output and error.
type Stream struct {
	conn   net.Conn
	reader *bufio.Reader
}

// Attach attaches to a container's standard streams (a hijacked
// connection): stdin is written to Write, and Demux reads stdout and stderr
// (Tty off: the daemon frames them).
func (c *Client) Attach(ctx context.Context, id string) (*Stream, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	query := url.Values{"stream": {"1"}, "stdin": {"1"}, "stdout": {"1"}, "stderr": {"1"}}
	request := fmt.Sprintf("POST /%s/containers/%s/attach?%s HTTP/1.1\r\nHost: docker\r\n"+
		"Connection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n", Version, id, query.Encode())
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	if _, err := io.WriteString(conn, request); err != nil {
		conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if response.StatusCode != http.StatusSwitchingProtocols && response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		conn.Close()
		return nil, &Error{Status: response.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	conn.SetDeadline(time.Time{})
	return &Stream{conn: conn, reader: reader}, nil
}

// Write writes to the container's standard input.
func (s *Stream) Write(p []byte) (int, error) { return s.conn.Write(p) }

// CloseWrite ends the container's standard input.
func (s *Stream) CloseWrite() error {
	if cw, ok := s.conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// Close closes the attachment (the container goes on).
func (s *Stream) Close() error { return s.conn.Close() }

// Demux copies the framed output to stdout and stderr until the stream
// ends.
func (s *Stream) Demux(stdout, stderr io.Writer) error {
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(s.reader, header); err != nil {
			if err == io.EOF || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		size := int64(binary.BigEndian.Uint32(header[4:8]))
		target := stdout
		if header[0] == 2 {
			target = stderr
		}
		if _, err := io.CopyN(target, s.reader, size); err != nil {
			return err
		}
	}
}
