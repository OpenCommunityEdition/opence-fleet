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
