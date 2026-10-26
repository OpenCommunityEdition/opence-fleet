package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agent/runtime/process"
	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/update"
)

// JoinAnswer is the control plane's answer to an enrolment.
type JoinAnswer struct {
	OK    bool   `json:"ok"`
	Node  string `json:"node"`
	Token string `json:"token"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Join exchanges a join token (one use, an hour) for the node's token
// (POST /agent/join).
func Join(ctx context.Context, client *http.Client, url, joinToken, name, runtimeName string) (*JoinAnswer, error) {
	hostname, _ := os.Hostname()
	body, _ := json.Marshal(map[string]string{"token": joinToken, "name": name, "hostname": hostname,
		"runtime": runtimeName, "os": goruntime.GOOS, "arch": goruntime.GOARCH})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(url, "/")+"/agent/join",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	var answer JoinAnswer
	if err := json.Unmarshal(data, &answer); err != nil {
		return nil, fmt.Errorf("the control plane answered %s, not JSON", response.Status)
	}
	if !answer.OK || answer.Token == "" {
		if answer.Error != nil {
			return nil, fmt.Errorf("the enrolment was refused: %s (%s)", answer.Error.Message, answer.Error.Code)
		}
		return nil, fmt.Errorf("the enrolment was refused (%s)", response.Status)
	}
	return &answer, nil
}

// WriteEnv writes the agent's environment file (mode 0600: it holds the
// node token).
func WriteEnv(path string, values map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# opence-fleet agent: written by the enrolment (opence-fleet agent --join). The token is the node's\n")
	b.WriteString("# alone: keep this file 0600.\n")
	for _, k := range []string{"FLEET_AGENT_URL", "FLEET_AGENT_TOKEN", "FLEET_AGENT_NAME", "FLEET_AGENT_DIR",
		"FLEET_AGENT_RUNTIME", "FLEET_AGENT_DATA_ROOT", "FLEET_AGENT_CA_FILE"} {
		if v := values[k]; v != "" {
			b.WriteString(k + "=" + v + "\n")
		}
	}
	part := path + ".part"
	if err := os.WriteFile(part, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(part, path)
}

// handleUpdate installs a release of the agent the control plane offers,
// checked against the release key's signature, and restarts into it
// without stopping the servers: the containers are Docker's, and the
// children's pipes are handed on (the exec keeps the pid).
func (a *Agent) handleUpdate(ctx context.Context, r agentproto.UpdateAgent) {
	if r.Version == a.Version {
		a.reply(agentproto.Reply{ID: r.ID, OK: true, Lines: []string{"the agent runs " + r.Version + " already"}})
		return
	}
	u := &update.Updater{Current: a.Version, Dir: filepath.Join(a.Config.Dir, "bin"), Client: a.HTTP,
		Key: a.ReleaseKey, Log: a.Log}
	install, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	url := r.URL
	if strings.HasPrefix(url, "/") {
		url = a.Config.URL + url
	}
	binary, err := u.InstallSigned(install, r.Version, url, []byte(r.Sums), []byte(r.Sig))
	if err != nil {
		a.Log.Error("the agent's update was not installed", "version", r.Version, "err", err)
		a.reply(agentproto.Fail(r.ID, "failed", err.Error()))
		return
	}
	a.Log.Info("the agent's update is installed; restarting into it", "version", r.Version)
	a.reply(agentproto.Reply{ID: r.ID, OK: true, Lines: []string{"installed " + r.Version + "; restarting"}})
	time.Sleep(time.Second)
	a.saveState()
	if p, ok := a.Runtime.(*process.Runtime); ok {
		if err := p.Handoff(); err != nil {
			a.Log.Error("the children's pipes were not handed on: the update waits for the next start", "err", err)
			return
		}
	}
	a.Detach()
	if a.Restart != nil {
		a.Restart(binary)
	}
}

// discard is a logger that writes nothing (tests).
func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
