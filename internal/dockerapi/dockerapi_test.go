package dockerapi

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSplitReference(t *testing.T) {
	for in, want := range map[string][2]string{
		"ghcr.io/opencommunityedition/opence-server:build-211": {"ghcr.io/opencommunityedition/opence-server", "build-211"},
		"ghcr.io/o/opence-server@sha256:ab":                    {"ghcr.io/o/opence-server", "sha256:ab"},
		"localhost:5000/opence-server":                         {"localhost:5000/opence-server", ""},
		"localhost:5000/opence-server:dev":                     {"localhost:5000/opence-server", "dev"},
		"alpine":                                               {"alpine", ""},
	} {
		repository, tag := SplitReference(in)
		if repository != want[0] || tag != want[1] {
			t.Errorf("%s: %s %s", in, repository, tag)
		}
	}
}

// Against the machine's Docker, when it has one and the test asks for it
// (FLEET_TEST_DOCKER=1): a container made, attached, started, typed to,
// waited on and removed.
func TestDocker(t *testing.T) {
	if os.Getenv("FLEET_TEST_DOCKER") == "" {
		t.Skip("FLEET_TEST_DOCKER=1 runs the tests that use Docker")
	}
	c, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Skip("no Docker:", err)
	}
	image := "alpine:3.20"
	if _, err := c.InspectImage(ctx, image); IsNotFound(err) {
		if err := c.Pull(ctx, image, nil); err != nil {
			t.Fatal(err)
		}
	}
	timeout := 2
	config := ContainerConfig{Image: image, Cmd: []string{"sh", "-c", "read line; echo got $line; echo err >&2; exit 5"},
		OpenStdin: true, AttachStdin: true, AttachStdout: true, AttachStderr: true, StopTimeout: &timeout,
		Labels: map[string]string{"opence-fleet.test": "1"}}
	config.HostConfig.ReadonlyRootfs = true
	config.HostConfig.CapDrop = []string{"ALL"}
	config.HostConfig.LogConfig.Type = "json-file"
	config.HostConfig.RestartPolicy.Name = "no"
	id, err := c.Create(ctx, "", config)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Remove(context.Background(), id, true)
	stream, err := c.Attach(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- stream.Demux(&stdout, &stderr) }()
	if err := c.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	stream.Write([]byte("hello\n"))
	code, err := c.Wait(ctx, id, "not-running")
	if err != nil || code != 5 {
		t.Fatal(code, err)
	}
	<-done
	if !strings.Contains(stdout.String(), "got hello") || !strings.Contains(stderr.String(), "err") {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	list, err := c.List(ctx, "opence-fleet.test", true)
	if err != nil || len(list) == 0 {
		t.Fatal(list, err)
	}
	info, err := c.Inspect(ctx, id)
	if err != nil || info.State.ExitCode != 5 || !info.HostConfig.ReadonlyRootfs {
		t.Fatal(info, err)
	}
}
