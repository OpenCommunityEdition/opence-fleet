package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"sync"

	"github.com/OpenCommunityEdition/opence-fleet/internal/core"
	"github.com/OpenCommunityEdition/opence-fleet/internal/httpx"
	"github.com/OpenCommunityEdition/opence-fleet/internal/hub"
	"github.com/OpenCommunityEdition/opence-fleet/internal/update"
)

// agentGate lets an agent's WebSocket through over TLS or from loopback.
func (s *Server) agentGate(h *hub.Hub) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.protected(r) {
			s.plainHTTP(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// nodeOf authenticates a node's request by its token.
func (s *Server) nodeOf(r *http.Request) (int64, string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return 0, "", false
	}
	return s.Fleet.Authenticate(r.Context(), hub.TokenHash(strings.TrimSpace(token)))
}

// join is an agent's enrolment: a join token for the node's token.
func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	if !s.protected(r) {
		s.plainHTTP(w, r)
		return
	}
	key := s.Clients.Of(r).Key()
	if ok, _ := s.anonymous.Allow("join:" + key); !ok {
		httpx.Fail(w, http.StatusTooManyRequests, "rate_limited", "too many enrolments from this address", "")
		return
	}
	var in core.JoinRequest
	var body struct {
		Token    string `json:"token"`
		Name     string `json:"name"`
		Hostname string `json:"hostname"`
		Runtime  string `json:"runtime"`
		OS       string `json:"os"`
		Arch     string `json:"arch"`
	}
	if err := readJSON(r, &body); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "schema", err.Error(), "")
		return
	}
	in = core.JoinRequest{Token: body.Token, Name: body.Name, Hostname: body.Hostname, Runtime: body.Runtime, OS: body.OS, Arch: body.Arch}
	name, token, err := s.Fleet.Join(r.Context(), in)
	if err != nil {
		e, _ := core.AsError(err)
		status, code := http.StatusBadRequest, "failed"
		if e != nil {
			status, code = e.Status, e.Code
		}
		httpx.Fail(w, status, code, err.Error(), "")
		return
	}
	s.Log.Info("a node enrolled", "node", name, "hostname", body.Hostname, "runtime", body.Runtime)
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true, "node": name, "token": token})
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// blob serves a library item to an enrolled node (Range: resumable).
func (s *Server) blob(w http.ResponseWriter, r *http.Request) {
	if !s.protected(r) {
		s.plainHTTP(w, r)
		return
	}
	if _, _, ok := s.nodeOf(r); !ok {
		httpx.Fail(w, http.StatusUnauthorized, "denied", "the library is the fleet's nodes' alone", "")
		return
	}
	sha := r.PathValue("sha")
	if !sha256Pattern.MatchString(sha) {
		http.NotFound(w, r)
		return
	}
	if _, err := s.Fleet.Store.GetLibrary(r.Context(), sha); err != nil {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(s.Fleet.Blobs().Path(sha))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, _ := file.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, sha, info.ModTime(), file)
}

var zipName = regexp.MustCompile(`^([a-zA-Z0-9][a-zA-Z0-9._-]{0,63})\.zip$`)

// releaseZip serves a release's zip (the process runtime's) to a node.
func (s *Server) releaseZip(w http.ResponseWriter, r *http.Request) {
	if !s.protected(r) {
		s.plainHTTP(w, r)
		return
	}
	if _, _, ok := s.nodeOf(r); !ok {
		httpx.Fail(w, http.StatusUnauthorized, "denied", "a node token is needed", "")
		return
	}
	file := r.PathValue("file")
	m := zipName.FindStringSubmatch(file)
	if m == nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(s.Fleet.ZipPath(m[1]))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	w.Header().Set("Content-Type", "application/zip")
	http.ServeContent(w, r, file, info.ModTime(), f)
}

var (
	selfOnce sync.Once
	selfSum  string
)

// agentScript serves agent.sh with the fleet's URL, version and release key
// filled in.
func (s *Server) agentScript(w http.ResponseWriter, r *http.Request) {
	script := string(s.AgentScript)
	if script == "" {
		http.NotFound(w, r)
		return
	}
	key := update.ReleaseKey
	if s.Updater != nil && len(s.Updater.Key) == 32 {
		key = hex.EncodeToString(s.Updater.Key)
	}
	selfOnce.Do(func() {
		if exe, err := os.Executable(); err == nil {
			if f, err := os.Open(exe); err == nil {
				h := sha256.New()
				io.Copy(h, f)
				f.Close()
				selfSum = hex.EncodeToString(h.Sum(nil))
			}
		}
	})
	replacer := strings.NewReplacer("@FLEET_URL@", s.publicURL(r), "@FLEET_VERSION@", s.Version, "@RELEASE_KEY@", key,
		"@SELF_ARCH@", goruntime.GOARCH, "@SELF_SHA256@", selfSum)
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, replacer.Replace(script))
}

// agentBinary serves the control plane's own binary (a development build's
// agent, for a node of the same machine kind; a release's comes from the
// release, checked against its signed sums).
func (s *Server) agentBinary(w http.ResponseWriter, r *http.Request) {
	if !s.protected(r) {
		s.plainHTTP(w, r)
		return
	}
	if r.PathValue("arch") != goruntime.GOARCH || goruntime.GOOS != "linux" {
		httpx.Fail(w, http.StatusNotFound, "not_found", "this control plane is "+goruntime.GOOS+"/"+goruntime.GOARCH+
			": take the agent for your machine from the release", "")
		return
	}
	exe, err := os.Executable()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(exe)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base("opence-fleet")+`"`)
	http.ServeContent(w, r, "opence-fleet", info.ModTime(), f)
}
