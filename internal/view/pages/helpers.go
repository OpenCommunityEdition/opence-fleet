// Package pages is the fleet's pages, one templ file a page (or a family of
// them), built from the components of package parts.
package pages

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/serverapi"
)

func timeOf(ms int64) time.Time { return time.UnixMilli(ms) }

func secondsOf(n int64) time.Duration { return time.Duration(n) * time.Second }

func fmtFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func hexOf(b []byte) string { return hex.EncodeToString(b) }

// scores writes a game's team scores (JSON) as 3–1.
func scores(text string) string {
	var list []int
	if json.Unmarshal([]byte(text), &list) != nil || len(list) == 0 {
		return "—"
	}
	parts := make([]string, len(list))
	for i, n := range list {
		parts[i] = fmt.Sprint(n)
	}
	return strings.Join(parts, "–")
}

// clientBuildLabel is a player's game's build in a word: "build 282", or a
// local build.
func clientBuildLabel(c *serverapi.ClientBuild) string {
	if c.Build > 0 {
		return "build " + strconv.FormatInt(c.Build, 10)
	}
	return "local build"
}

// clientTitle is what a player's game told of itself, a line each: its
// label, the repository it is made from, its mods.
func clientTitle(c *serverapi.ClientBuild) string {
	lines := []string{c.Version}
	if c.Repository != "" {
		lines = append(lines, "from "+c.Repository)
	} else {
		lines = append(lines, "from a repository it does not name")
	}
	if len(c.Mods) > 0 {
		lines = append(lines, "mods: "+strings.Join(c.Mods, ", "))
	} else {
		lines = append(lines, "no mods")
	}
	return strings.Join(lines, "\n")
}

// networkSource is where a release's network version was read.
func networkSource(source string) string {
	switch source {
	case "label":
		return "the image's label"
	case "notes":
		return "the release's notes"
	case "server":
		return "its servers"
	}
	return source
}
