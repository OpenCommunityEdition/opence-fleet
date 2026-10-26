package core

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/config"
	"github.com/OpenCommunityEdition/opence-fleet/internal/hub"
	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// JoinTokenTTL is how long a join token is good for.
const JoinTokenTTL = time.Hour

// Node is a node with its settings read.
type Node struct {
	db.Node
	Settings spec.Node
	Facts    agentproto.Hello
}

func readNode(n db.Node) Node {
	out := Node{Node: n, Settings: spec.DefaultNode()}
	json.Unmarshal([]byte(n.Settings), &out.Settings)
	json.Unmarshal([]byte(n.Facts), &out.Facts)
	return out
}

// Nodes lists the nodes.
func (f *Fleet) Nodes(ctx context.Context) ([]Node, error) {
	rows, err := f.Store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Node, len(rows))
	for i, r := range rows {
		out[i] = readNode(r)
	}
	return out, nil
}

// NodeByName finds a node.
func (f *Fleet) NodeByName(ctx context.Context, name string) (Node, error) {
	n, err := f.Store.GetNodeByName(ctx, name)
	if err != nil || n.Forgotten != 0 {
		return Node{}, NotFound("node " + name)
	}
	return readNode(n), nil
}

// NodeByID finds a node by its id.
func (f *Fleet) NodeByID(ctx context.Context, id int64) (Node, error) {
	n, err := f.Store.GetNode(ctx, id)
	if err != nil {
		return Node{}, NotFound("the node")
	}
	return readNode(n), nil
}

// JoinToken makes a one-use join token for a node (new, or enrolled
// again), good for an hour: the one-liner the Add a node page prints.
func (f *Fleet) JoinToken(ctx context.Context, actor, name string, settings spec.Node) (string, error) {
	name = config.NodeName(name)
	if name == "" {
		return "", Refuse(http.StatusBadRequest, "schema", "a node's name is a-z, 0-9 and '-'")
	}
	if err := settings.Check(); err != nil {
		return "", Refuse(http.StatusBadRequest, "schema", "%v", err)
	}
	token := Token(24)
	data, _ := json.Marshal(settings)
	if err := f.Store.CreateJoinToken(ctx, db.CreateJoinTokenParams{TokenHash: hub.TokenHash(token), NodeName: name,
		Settings: string(data), ExpiresAt: f.Now().Add(JoinTokenTTL).Unix(), CreatedBy: actor, CreatedAt: f.Now().Unix()}); err != nil {
		return "", err
	}
	f.Audit(ctx, actor, "node.join_token", name, nil)
	return token, nil
}

// JoinRequest is an agent's enrolment.
type JoinRequest struct {
	Token, Name, Hostname, Runtime, OS, Arch string
}

// Join takes a join token (once) and returns the node's name and token,
// whose SHA-256 alone is kept. A node enrolled again keeps its instances.
func (f *Fleet) Join(ctx context.Context, r JoinRequest) (string, string, error) {
	hash := hub.TokenHash(strings.TrimSpace(r.Token))
	joinToken, err := f.Store.GetJoinToken(ctx, hash)
	if err != nil {
		return "", "", Refuse(http.StatusUnauthorized, "denied", "the join token is not this fleet's (make one on the Add a node page)")
	}
	used, err := f.Store.UseJoinToken(ctx, db.UseJoinTokenParams{TokenHash: hash, ExpiresAt: f.Now().Unix()})
	if err != nil {
		return "", "", err
	}
	if used == 0 {
		return "", "", Refuse(http.StatusUnauthorized, "denied", "the join token was used, or is more than an hour old: make another")
	}
	name := joinToken.NodeName
	token := Token(32)
	var settings spec.Node
	json.Unmarshal([]byte(joinToken.Settings), &settings)
	if settings.TunnelPorts.First == 0 {
		settings = spec.DefaultNode()
	}
	if settings.Runtime == "" && (r.Runtime == "docker" || r.Runtime == "process") {
		settings.Runtime = r.Runtime
	}
	existing, err := f.Store.GetNodeByName(ctx, name)
	switch {
	case err == nil:
		if err := f.Store.UpdateNodeToken(ctx, db.UpdateNodeTokenParams{TokenHash: hash2(token), ID: existing.ID}); err != nil {
			return "", "", err
		}
		f.Hub.Disconnect(existing.ID, "the node was enrolled again")
	case store.NotFound(err):
		data, _ := json.Marshal(settings)
		if _, err := f.Store.CreateNode(ctx, db.CreateNodeParams{Name: name, TokenHash: hash2(token), Settings: string(data),
			CreatedAt: f.Now().Unix()}); err != nil {
			return "", "", err
		}
	default:
		return "", "", err
	}
	f.Audit(ctx, "agent:"+name, "node.enrol", name, map[string]string{"hostname": r.Hostname, "runtime": r.Runtime,
		"arch": r.Arch})
	f.Live.Notify(liveChange("node", name))
	return name, token, nil
}

func hash2(token string) []byte { return hub.TokenHash(token) }

// SetNodeSettings changes a node's settings (the agent is sent them).
func (f *Fleet) SetNodeSettings(ctx context.Context, actor, name string, settings spec.Node) error {
	n, err := f.NodeByName(ctx, name)
	if err != nil {
		return err
	}
	if err := settings.Check(); err != nil {
		return Refuse(http.StatusBadRequest, "schema", "%v", err)
	}
	instances, err := f.Store.ListInstancesByNode(ctx, n.ID)
	if err != nil {
		return err
	}
	for _, i := range instances {
		if !settings.TunnelPorts.Contains(int(i.TunnelPort)) || !settings.WebPorts.Contains(int(i.WebPort)) {
			return Refuse(http.StatusConflict, "in_use", "%s has ports outside the new ranges (tunnel %d, web %d): move it first",
				i.Slug, i.TunnelPort, i.WebPort)
		}
	}
	data, _ := json.Marshal(settings)
	if err := f.Store.UpdateNodeSettings(ctx, db.UpdateNodeSettingsParams{Settings: string(data), ID: n.ID}); err != nil {
		return err
	}
	f.Audit(ctx, actor, "node.settings", name, settings)
	// (the environment of its servers changes: each one needs a restart)
	for _, i := range instances {
		f.Store.BumpInstanceGeneration(ctx, i.Slug)
	}
	f.Push(n.ID)
	f.Live.Notify(liveChange("node", name))
	return nil
}

// ForgetNode forgets a node: its token is revoked at once; a node with
// instances is refused (remove or move them first).
func (f *Fleet) ForgetNode(ctx context.Context, actor, name string) error {
	n, err := f.NodeByName(ctx, name)
	if err != nil {
		return err
	}
	instances, _ := f.Store.ListInstancesByNode(ctx, n.ID)
	if len(instances) > 0 {
		return Refuse(http.StatusConflict, "in_use", "%s has %d servers: delete them first", name, len(instances))
	}
	if err := f.Store.ForgetNode(ctx, n.ID); err != nil {
		return err
	}
	f.Hub.Disconnect(n.ID, "the node was forgotten")
	f.Audit(ctx, actor, "node.forget", name, nil)
	f.Live.Notify(liveChange("node", name))
	return nil
}

// Push sends a node its spec again, a moment from now, so that a burst of
// changes is one spec.
func (f *Fleet) Push(node int64) {
	f.pushMu.Lock()
	defer f.pushMu.Unlock()
	if t := f.pushTimer[node]; t != nil {
		t.Reset(150 * time.Millisecond)
		return
	}
	f.pushTimer[node] = time.AfterFunc(150*time.Millisecond, func() {
		f.pushMu.Lock()
		delete(f.pushTimer, node)
		f.pushMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := f.Hub.Push(ctx, node); err != nil {
			f.Log.Warn("a spec was not sent", "node", node, "err", err)
		}
	})
}

// PushNow sends a node its spec at once.
func (f *Fleet) PushNow(ctx context.Context, node int64) error { return f.Hub.Push(ctx, node) }

// PushAll sends every node its spec.
func (f *Fleet) PushAll(ctx context.Context) {
	nodes, _ := f.Store.ListNodes(ctx)
	for _, n := range nodes {
		f.Push(n.ID)
	}
}

// Spec is a node's desired state (the hub's Sink).
func (f *Fleet) Spec(ctx context.Context, nodeID int64) (spec.Spec, error) {
	n, err := f.NodeByID(ctx, nodeID)
	if err != nil {
		return spec.Spec{}, err
	}
	rows, err := f.Store.ListInstancesByNode(ctx, nodeID)
	if err != nil {
		return spec.Spec{}, err
	}
	library, err := f.Store.ListLibrary(ctx)
	if err != nil {
		return spec.Spec{}, err
	}
	s := spec.Spec{Generation: f.Now().UnixNano(), Node: n.Settings, Images: []spec.Image{}, Releases: []spec.Release{},
		Library: []spec.LibraryItem{}, Instances: []spec.Instance{}}
	for _, item := range library {
		if item.Kind == "gametype" {
			continue
		}
		s.Library = append(s.Library, spec.LibraryItem{Kind: item.Kind, Name: item.Name, SHA256: item.Sha256, Size: item.Size})
	}
	builds := map[string]bool{}
	for _, row := range rows {
		if row.Kind != "managed" {
			continue
		}
		instance, err := f.RenderInstance(ctx, row, n, library)
		if err != nil {
			f.Log.Warn("an instance was not rendered", "slug", row.Slug, "err", err)
			continue
		}
		s.Instances = append(s.Instances, instance)
		builds[instance.Build] = true
	}
	f.prefetchMu.Lock()
	for build := range f.prefetching[nodeID] {
		builds[build] = true
	}
	f.prefetchMu.Unlock()
	for build := range builds {
		release, err := f.Store.GetRelease(ctx, build)
		if err != nil {
			continue
		}
		s.Images = append(s.Images, spec.Image{Build: build, Ref: release.Image, Digest: release.Digest})
		if release.ZipSha256 != "" {
			s.Releases = append(s.Releases, spec.Release{Build: build, SHA256: release.ZipSha256, Size: release.ZipSize,
				URL: "/agent/releases/" + build + ".zip"})
		}
	}
	sort.Slice(s.Images, func(i, j int) bool { return s.Images[i].Build < s.Images[j].Build })
	sort.Slice(s.Releases, func(i, j int) bool { return s.Releases[i].Build < s.Releases[j].Build })
	return s, nil
}

// Authenticate finds a node by its token's hash (the hub's Sink).
func (f *Fleet) Authenticate(ctx context.Context, hash []byte) (int64, string, bool) {
	n, err := f.Store.GetNodeByToken(ctx, hash)
	if err != nil {
		return 0, "", false
	}
	return n.ID, n.Name, true
}

// Hello keeps a node's facts (the hub's Sink).
func (f *Fleet) Hello(node int64, name string, hello agentproto.Hello) {
	facts := hello
	facts.Instances, facts.LastSeq = nil, nil
	data, _ := json.Marshal(facts)
	ctx := context.Background()
	f.Store.UpdateNodeSeen(ctx, db.UpdateNodeSeenParams{Facts: string(data), AgentVersion: hello.Version,
		LastSeen: ptrInt(f.Now().Unix()), ID: node})
	// (only its own instances: a slug not on this node is not this node's to report)
	own := map[string]bool{}
	rows, _ := f.Store.ListInstancesByNode(ctx, node)
	for _, r := range rows {
		own[r.Slug] = true
	}
	var mine []agentproto.InstanceReport
	for _, i := range hello.Instances {
		if own[i.Slug] {
			mine = append(mine, i)
		}
	}
	hello.Instances = mine
	f.Live.Connected(node, name, hello)
	f.resolveAlert("node_offline", name)
	// (the fleet bans that waited for its servers, once they are up)
	go func() {
		time.Sleep(30 * time.Second)
		f.ApplyPendingBans(context.Background(), node)
	}()
	if hello.Arch != "amd64" && hello.Arch != "" && len(rows) > 0 {
		f.Log.Warn("a node that is not amd64 runs no game", "node", name, "arch", hello.Arch)
	}
}

// Disconnected marks a node offline (the hub's Sink).
func (f *Fleet) Disconnected(node int64) {
	f.Live.Disconnected(node)
	f.Store.TouchNode(context.Background(), db.TouchNodeParams{LastSeen: ptrInt(f.Now().Unix()), ID: node})
}

// NodeOnline tells whether a node is connected.
func (f *Fleet) NodeOnline(id int64) bool { return f.Hub.Online(id) }
