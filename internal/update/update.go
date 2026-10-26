// Package update keeps the fleet up to date (the master server's package,
// re-homed): it checks the project's releases, tells the owners of a newer
// one (the Settings page, an alert), and on their word (or on its own, in
// auto mode) downloads the release's binary, checks it against the
// release's SHA256SUMS and their signature by the release key, installs it
// beside the database (bin/), and restarts into it. The listening socket is
// handed to the new process, so the restart drops no connection. The
// agents are updated the same way, by the control plane's offer, into
// their node folder's bin/.
//
// At each start, Launch runs the newest binary installed if it is newer
// than the program itself (a container's image is read-only; its /data is
// not), and goes back to the last good one after a binary failed to start
// three times.
package update

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// The modes (FLEET_UPDATES).
const (
	// no checks
	Off = "off"
	// tell the owners; they update with a button
	Notify = "notify"
	// install a newer release at once
	Auto = "auto"
)

// DefaultSource is the project's latest release, on GitHub's API.
const DefaultSource = "https://api.github.com/repos/OpenCommunityEdition/opence-fleet/releases/latest"

// the limits of what is downloaded
const (
	maximumRelease = 1 << 20
	maximumSums    = 64 << 10
	maximumBinary  = 200 << 20
)

// Release is a release of the fleet.
type Release struct {
	Version string
	// its page, and its notes (markdown)
	URL, Notes string
	// its files' download URLs, by name
	Assets map[string]string
}

// Status is what the Settings page shows of the updates.
type Status struct {
	Current, Mode string
	// the latest release, once checked (nil before)
	Latest  *Release
	Checked time.Time
	// the last check's or update's failure
	Error string
	// a release newer than Current is out
	Available bool
	Updating  bool
}

// Updater checks the releases and installs them.
type Updater struct {
	// the running version, and the mode
	Current, Mode string
	// the latest release's URL (GitHub's API)
	Source string
	// where the binaries are installed (bin/ beside the database)
	Dir    string
	Client *http.Client
	Key    ed25519.PublicKey
	Log    *slog.Logger
	// told of a release newer than Current, once a version
	Available func(Release)
	// told that an update started (by actor), and that it failed
	Started func(version, actor string)
	Failed  func(version string, err error)
	// restarts into an installed binary (set by serve; nil: the update is
	// installed and runs at the next start)
	Restart func(binary string)

	mu       sync.Mutex
	ctx      context.Context
	latest   *Release
	checked  time.Time
	failure  string
	updating bool
	told     string
}

// AssetName is the release binary's name for this machine:
// opence-fleet-<os>-<arch>, with the ARM version for 32-bit ARM
// (opence-fleet-linux-armv7).
func AssetName() string {
	name := "opence-fleet-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOARCH == "arm" {
		version := "7"
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				if s.Key == "GOARM" && s.Value != "" {
					version, _, _ = strings.Cut(s.Value, ",")
				}
			}
		}
		name += "v" + version
	}
	return name
}

// Status returns the updates' state.
func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := Status{Current: u.Current, Mode: u.Mode, Checked: u.checked, Error: u.failure, Updating: u.updating}
	if u.latest != nil {
		latest := *u.latest
		s.Latest = &latest
		s.Available = Newer(latest.Version, u.Current)
	}
	return s
}

// Run checks the releases a minute after the start, then every interval,
// until ctx ends; in auto mode it installs a newer one.
func (u *Updater) Run(ctx context.Context, interval time.Duration) {
	u.mu.Lock()
	u.ctx = ctx
	u.mu.Unlock()
	if u.Mode == Off {
		return
	}
	if !Valid(u.Current) {
		u.Log.Info("update: no release checks for a development build", "version", u.Current)
		return
	}
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = interval
		if _, _, err := u.CheckNow(ctx, "auto"); err != nil {
			u.Log.Warn("update: the releases were not checked", "err", err)
		}
	}
}

// CheckNow reads the latest release at once (an owner's button, or Run's
// turn) and, in auto mode, installs a newer one as Run would: the release,
// and whether it is newer than the running one.
func (u *Updater) CheckNow(ctx context.Context, actor string) (Release, bool, error) {
	if u.Mode == Off {
		return Release{}, false, errors.New("the fleet's updates are off (FLEET_UPDATES=off): set it to notify or auto to check")
	}
	if !Valid(u.Current) {
		return Release{}, false, fmt.Errorf("this fleet is a development build (%s), which does not update itself: only a vX.Y.Z release does", u.Current)
	}
	release, err := u.Check(ctx)
	if err != nil {
		return release, false, err
	}
	newer := Newer(release.Version, u.Current)
	if newer && u.Mode == Auto {
		if err := u.Start(actor); err != nil {
			u.Log.Warn("update: not started", "err", err)
		}
	}
	return release, newer, nil
}

func (u *Updater) get(ctx context.Context, url string, limit int64, accept string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		request.Header.Set("Accept", accept)
	}
	request.Header.Set("User-Agent", "opence-fleet/"+u.Current)
	response, err := u.Client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("%s is larger than %d bytes", url, limit)
	}
	return data, err
}

// Check reads the latest release.
func (u *Updater) Check(ctx context.Context) (Release, error) {
	release, err := u.check(ctx)
	u.mu.Lock()
	u.checked = time.Now()
	if err != nil {
		u.failure = err.Error()
		u.mu.Unlock()
		return release, err
	}
	u.latest, u.failure = &release, ""
	tell := Newer(release.Version, u.Current) && u.told != release.Version
	if tell {
		u.told = release.Version
	}
	u.mu.Unlock()
	if tell {
		u.Log.Info("update: a newer release is out", "version", release.Version, "running", u.Current)
		if u.Available != nil {
			u.Available(release)
		}
	}
	return release, nil
}

func (u *Updater) check(ctx context.Context) (Release, error) {
	data, err := u.get(ctx, u.Source, maximumRelease, "application/vnd.github+json")
	if err != nil {
		return Release{}, err
	}
	var answer struct {
		Tag    string `json:"tag_name"`
		URL    string `json:"html_url"`
		Body   string `json:"body"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		return Release{}, fmt.Errorf("the release is not JSON: %w", err)
	}
	if !Valid(answer.Tag) {
		return Release{}, fmt.Errorf("the latest release's tag %q is not a version (v1.2.3)", answer.Tag)
	}
	release := Release{Version: answer.Tag, URL: answer.URL, Notes: answer.Body, Assets: map[string]string{}}
	for _, a := range answer.Assets {
		release.Assets[a.Name] = a.URL
	}
	return release, nil
}

// Start installs the latest release and restarts into it, on its own
// goroutine; it refuses when there is no newer release, or an update runs.
func (u *Updater) Start(actor string) error {
	u.mu.Lock()
	latest, ctx := u.latest, u.ctx
	switch {
	case u.updating:
		u.mu.Unlock()
		return errors.New("an update is running already")
	case latest == nil || !Newer(latest.Version, u.Current):
		u.mu.Unlock()
		return fmt.Errorf("no release newer than %s is known", u.Current)
	}
	u.updating = true
	u.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	if u.Started != nil {
		u.Started(latest.Version, actor)
	}
	u.Log.Info("update: started", "version", latest.Version, "by", actor)
	go func() {
		binary, err := u.Install(ctx, *latest)
		if err != nil {
			u.mu.Lock()
			u.updating, u.failure = false, err.Error()
			u.mu.Unlock()
		}
		// (else it stays updating until the restart)
		if err != nil {
			u.Log.Error("update: failed", "version", latest.Version, "err", err)
			if u.Failed != nil {
				u.Failed(latest.Version, err)
			}
			return
		}
		u.Log.Info("update: installed; restarting", "version", latest.Version, "binary", binary)
		if u.Restart != nil {
			u.Restart(binary)
		}
	}()
	return nil
}

// Install downloads a release's binary for this machine, checks it, and
// makes it the one Launch runs; it returns its path.
func (u *Updater) Install(ctx context.Context, r Release) (string, error) {
	name := AssetName()
	binaryURL, sumsURL, signatureURL := r.Assets[name], r.Assets["SHA256SUMS"], r.Assets["SHA256SUMS.sig"]
	if binaryURL == "" {
		return "", fmt.Errorf("release %s has no %s (this machine's binary)", r.Version, name)
	}
	if sumsURL == "" || signatureURL == "" {
		return "", fmt.Errorf("release %s has no signed SHA256SUMS", r.Version)
	}
	sums, err := u.get(ctx, sumsURL, maximumSums, "")
	if err != nil {
		return "", err
	}
	signature, err := u.get(ctx, signatureURL, 1024, "")
	if err != nil {
		return "", err
	}
	return u.InstallSigned(ctx, r.Version, binaryURL, sums, signature)
}

// InstallSigned installs a release's binary for this machine from its URL,
// with the release's SHA256SUMS and their signature in hand (the agents'
// update: the control plane passes them on); it returns its path.
func (u *Updater) InstallSigned(ctx context.Context, version, binaryURL string, sums, signature []byte) (string, error) {
	name := AssetName()
	r := Release{Version: version}
	raw, err := hex.DecodeString(strings.TrimSpace(string(signature)))
	if err != nil || len(u.Key) != ed25519.PublicKeySize || !ed25519.Verify(u.Key, sums, raw) {
		return "", fmt.Errorf("release %s's SHA256SUMS is not signed by the release key: not installed", r.Version)
	}
	want := ""
	scanner := bufio.NewScanner(strings.NewReader(string(sums)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			want = strings.ToLower(fields[0])
		}
	}
	if want == "" {
		return "", fmt.Errorf("release %s's SHA256SUMS has no %s", r.Version, name)
	}
	if err := os.MkdirAll(u.Dir, 0o755); err != nil {
		return "", err
	}
	binary := filepath.Join(u.Dir, "opence-fleet-"+r.Version)
	part := binary + ".part"
	if err := u.download(ctx, binaryURL, part, want); err != nil {
		_ = os.Remove(part)
		return "", err
	}
	// (it runs here: not a binary for another machine, nor on a noexec
	// file system)
	check, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(check, part, "version").Output()
	if err != nil || !strings.Contains(string(out), " "+r.Version+" ") {
		_ = os.Remove(part)
		return "", fmt.Errorf("the downloaded %s does not run here (%v): %q", name, err, strings.TrimSpace(string(out)))
	}
	if err := os.Rename(part, binary); err != nil {
		return "", err
	}
	if err := install(u.Dir, r.Version); err != nil {
		return "", err
	}
	return binary, nil
}

func (u *Updater) download(ctx context.Context, url, path, want string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "opence-fleet/"+u.Current)
	response, err := u.Client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, response.Status)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, maximumBinary+1))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	switch {
	case err != nil:
		return err
	case n > maximumBinary:
		return fmt.Errorf("the binary is larger than %d bytes", maximumBinary)
	case hex.EncodeToString(hash.Sum(nil)) != want:
		return errors.New("the binary's SHA-256 is not the signed SHA256SUMS': not installed")
	}
	return os.Chmod(path, 0o755)
}
