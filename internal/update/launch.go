package update

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The environment a restart hands to the new process.
const (
	// set: the process is the one Launch or a restart ran (it launches
	// nothing)
	envLaunched = "OPENCE_FLEET_LAUNCHED"
	// the listening socket's file descriptor
	envListener = "OPENCE_FLEET_LISTEN_FD"
)

// tries is how many starts an installed binary has before Launch goes back
// to the last good one.
const tries = 3

// state is bin/state.json: the binary Launch runs, the last one that ran
// well, and the starts of the current one since it was installed.
type state struct {
	Current string `json:"current"`
	Good    string `json:"good"`
	Starts  int    `json:"starts"`
}

func readState(dir string) state {
	var s state
	if data, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	return s
}

// writeState replaces state.json (by a rename: the directory's owner may
// replace a file another user wrote, as the command line's root).
func writeState(dir string, s state) error {
	data, _ := json.Marshal(s)
	part := filepath.Join(dir, "state.json.part")
	if err := os.WriteFile(part, data, 0o644); err != nil {
		return err
	}
	return os.Rename(part, filepath.Join(dir, "state.json"))
}

func binaryPath(dir, version string) string { return filepath.Join(dir, "opence-fleet-"+version) }

// install makes an installed version the current one (the good one stays
// the fallback; none: the program itself), and removes the binaries
// neither current nor good.
func install(dir, version string) error {
	s := readState(dir)
	s.Current, s.Starts = version, 0
	if err := writeState(dir, s); err != nil {
		return err
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		v, ok := strings.CutPrefix(e.Name(), "opence-fleet-")
		if ok && v != s.Current && v != s.Good {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	return nil
}

// Launch runs the installed binary in this process's stead when it is newer
// than builtIn (the program's own version): it returns only when there is
// none to run. A binary that failed to start three times gives way to the
// last good one, or to the program.
func Launch(dir, builtIn string, log *slog.Logger) {
	if os.Getenv(envLaunched) != "" {
		return
	}
	s := readState(dir)
	if !Newer(s.Current, builtIn) {
		// (none, or the program is as new: a newer image, say)
		return
	}
	if s.Current != s.Good {
		if s.Starts >= tries {
			log.Error("update: the installed release failed to start; going back", "version", s.Current,
				"to", s.Good)
			s.Current, s.Starts = s.Good, 0
		} else {
			s.Starts++
		}
		_ = writeState(dir, s)
		if !Newer(s.Current, builtIn) {
			return
		}
	}
	binary := binaryPath(dir, s.Current)
	if _, err := os.Stat(binary); err != nil {
		return
	}
	log.Info("update: running the installed release", "version", s.Current, "program", builtIn)
	if err := Exec(binary, nil); err != nil {
		log.Error("update: the installed release did not run", "version", s.Current, "err", err)
	}
}

// MarkGood records that version runs well (serve calls it once it has
// served a while): Launch keeps it as the fallback.
func MarkGood(dir, version string) {
	s := readState(dir)
	if s.Current != version || s.Good == version {
		return
	}
	s.Good, s.Starts = version, 0
	_ = writeState(dir, s)
}

// Listener returns the listening socket's file a restart handed over (nil
// for none), and forgets it.
func Listener() *os.File {
	text := os.Getenv(envListener)
	_ = os.Unsetenv(envListener)
	fd, err := strconv.Atoi(text)
	if text == "" || err != nil || fd < 3 {
		return nil
	}
	return os.NewFile(uintptr(fd), "listener")
}

// environment is the process's environment for the binary it execs, with
// the listener's descriptor.
func environment(listener *os.File) []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, envLaunched+"=") && !strings.HasPrefix(e, envListener+"=") {
			env = append(env, e)
		}
	}
	env = append(env, envLaunched+"=1")
	if listener != nil {
		env = append(env, fmt.Sprintf("%s=%d", envListener, listener.Fd()))
	}
	return env
}
