package agent

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

// State is agent.state.json: the last spec received and what each running
// server was made from, so that a restart of the agent, or a control plane
// that is down, changes nothing on the node.
type State struct {
	Spec spec.Spec `json:"spec"`
	// what each running server runs: the spec it was started from, with the
	// live changes made since
	Applied map[string]spec.Instance `json:"applied"`
	// the library items in data/, by their path
	Library map[string]string `json:"library"`
}

// setApplied keeps what an instance's server runs (the state's map is the
// agent's, under its lock).
func (a *Agent) setApplied(slug string, i spec.Instance) {
	a.mu.Lock()
	a.state.Applied[slug] = i
	a.mu.Unlock()
}

func (a *Agent) statePath() string { return filepath.Join(a.Config.Dir, "agent.state.json") }

func (a *Agent) loadState() State {
	s := State{Applied: map[string]spec.Instance{}, Library: map[string]string{}}
	if data, err := os.ReadFile(a.statePath()); err == nil {
		if err := json.Unmarshal(data, &s); err != nil {
			a.Log.Warn("the state file does not parse: starting from nothing", "err", err)
		}
	}
	if s.Applied == nil {
		s.Applied = map[string]spec.Instance{}
	}
	if s.Library == nil {
		s.Library = map[string]string{}
	}
	return s
}

// saveState writes the state file (0600: it holds the instances' passwords
// and the publisher key), replacing it by a rename.
func (a *Agent) saveState() {
	a.mu.Lock()
	data, err := json.Marshal(a.state)
	a.mu.Unlock()
	if err != nil {
		return
	}
	part := a.statePath() + ".part"
	if err := os.WriteFile(part, data, 0o600); err != nil {
		a.Log.Error("the state file was not written", "err", err)
		return
	}
	if err := os.Rename(part, a.statePath()); err != nil {
		a.Log.Error("the state file was not written", "err", err)
	}
}
