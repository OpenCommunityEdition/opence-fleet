// Package view is what the pages share: the page's common fields, the view
// models the pages and the live fragments render (a server's row, a node's
// card), the words of states and times, the components' classes, and
// rendering. The pages are templ components (view/parts: the shell and the
// components; view/pages: one file a page), from which `go tool templ
// generate` writes *_templ.go; the styles are Tailwind's, from style.css
// (tools/generate.sh writes static/app.css). Every page works without
// JavaScript; htmx and the server-sent events add liveness.
package view

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/OpenCommunityEdition/opence-fleet/internal/auth"
	"github.com/OpenCommunityEdition/opence-fleet/internal/httpx"
)

// Page is what every page's shell reads.
type Page struct {
	Title   string
	Section string
	Fleet   string
	Version string
	User    *auth.Principal
	CSRF    string
	// the theme the cookie chose ("": the system's)
	Theme string
	// an action's outcome, shown once
	Notice, Error string
	// the shell's counts: servers, players, open alerts
	Servers, Players, Alerts int
	// a release of the fleet out, for owners
	UpdateBanner string
	// the page follows the live stream
	Live bool
	// the path, for the navigation
	Path string
	Now  time.Time
	Zone *time.Location
}

// Can tells whether the page's user has a role.
func (p Page) Can(role string) bool { return p.User.Can(role) }

// Render writes a page, rendered whole first, so that a failure answers an
// error rather than half a page.
func Render(w http.ResponseWriter, r *http.Request, status int, component templ.Component) {
	var buffer bytes.Buffer
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	if err := component.Render(ctx, &buffer); err != nil {
		http.Error(w, "the page failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	httpx.Secure(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buffer.Bytes())
}

// String renders a component to text (a fragment of the live stream).
func String(ctx context.Context, component templ.Component) string {
	var buffer bytes.Buffer
	if err := component.Render(ctx, &buffer); err != nil {
		return ""
	}
	return buffer.String()
}

// Tone is a status colour.
type Tone int

// The tones.
const (
	Neutral Tone = iota
	OK
	Warn
	Bad
	Info
	Accent
)

// Dot is a tone's dot's classes.
func Dot(t Tone) string {
	base := "inline-block size-1.5 shrink-0 rounded-full "
	switch t {
	case OK:
		return base + "bg-ok"
	case Warn:
		return base + "bg-warn"
	case Bad:
		return base + "bg-bad"
	case Info:
		return base + "bg-info"
	case Accent:
		return base + "bg-accent"
	}
	return base + "bg-faint"
}

// Badge is a tone's badge's classes.
func Badge(t Tone) string {
	base := "inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-xs font-medium whitespace-nowrap "
	switch t {
	case OK:
		return base + "border-ok/35 text-ok"
	case Warn:
		return base + "border-warn/40 text-warn"
	case Bad:
		return base + "border-bad/45 text-bad"
	case Info:
		return base + "border-info/40 text-info"
	case Accent:
		return base + "border-accent/50 text-ink"
	}
	return base + "border-line text-muted"
}

// The components' classes (Tailwind finds them here).
const (
	button = "inline-flex cursor-pointer items-center justify-center gap-2 whitespace-nowrap rounded-md px-3 py-1.5 " +
		"text-sm font-medium transition focus-visible:outline-2 focus-visible:outline-offset-2 " +
		"focus-visible:outline-accent disabled:pointer-events-none disabled:opacity-50"
	// Primary is the main action: white (the ink), filled.
	Primary = button + " border border-ink bg-ink text-ground hover:bg-ink/85"
	// Secondary is a ghost button.
	Secondary = button + " border border-line text-ink hover:bg-raised"
	// Danger is a destructive action, in a confirmation alone.
	Danger = button + " border border-bad/60 bg-bad/15 text-bad hover:bg-bad/25"
	// Small shrinks a button.
	Small = " !px-2 !py-1 !text-xs"
	// Input is a text input.
	Input = "w-full rounded-md border border-line bg-transparent px-3 py-2 text-sm placeholder:text-faint " +
		"focus:border-accent focus:outline-none focus:ring-0 disabled:opacity-60"
	// InputInline is an input in a row.
	InputInline = "rounded-md border border-line bg-transparent px-2.5 py-1.5 text-sm placeholder:text-faint " +
		"focus:border-accent focus:outline-none"
	// Select is a select.
	Select = "w-full rounded-md border border-line bg-panel px-3 py-2 text-sm focus:border-accent focus:outline-none"
	// TextArea is a text area.
	TextArea = "w-full rounded-md border border-line bg-transparent px-3 py-2 font-mono text-xs leading-5 " +
		"focus:border-accent focus:outline-none"
	// Check is a checkbox.
	Check = "size-4 rounded border-line accent-accent"
	// Panel is a card.
	Panel = "rounded-lg border border-line bg-panel"
	// Link is a styled link.
	Link = "text-accent underline-offset-2 hover:underline"
	// Label is a field's label.
	Label = "block text-sm font-medium"
	// Hint is a line of help.
	Hint = "text-xs text-muted"
	// Th is a table's header cell.
	Th = "px-4 py-2 text-left text-xs font-medium tracking-wide text-faint uppercase whitespace-nowrap"
	// Td is a table's cell.
	Td = "px-4 py-2.5 align-middle"
	// Tr is a table's row.
	Tr = "border-t border-line transition-colors hover:bg-raised"
)

// Since writes how long ago a time was, shortly.
func Since(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	return Duration(d) + " ago"
}

// Duration writes a duration shortly: 45 s, 12 min, 3 h, 2d 4h.
func Duration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}

// Uptime writes how long since a start ("—" for none).
func Uptime(since time.Time, now time.Time) string {
	if since.IsZero() {
		return "—"
	}
	return Duration(now.Sub(since))
}

// Clock writes a time of day in a zone.
func Clock(t time.Time, zone *time.Location) string {
	if zone == nil {
		zone = time.UTC
	}
	return t.In(zone).Format("15:04")
}

// When writes a date and time in a zone ("never" for the zero time).
func When(t time.Time, zone *time.Location) string {
	if t.IsZero() {
		return "never"
	}
	if zone == nil {
		zone = time.UTC
	}
	return t.In(zone).Format("2006-01-02 15:04")
}

// Unix writes a Unix time as When does.
func Unix(seconds int64, zone *time.Location) string {
	if seconds == 0 {
		return "never"
	}
	return When(time.Unix(seconds, 0), zone)
}

// UnixPtr writes an optional Unix time.
func UnixPtr(seconds *int64, zone *time.Location) string {
	if seconds == nil {
		return "never"
	}
	return Unix(*seconds, zone)
}

// Bytes writes a size shortly.
func Bytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Plural is one or many, by n.
func Plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Count writes n and its noun.
func Count(n int, one, many string) string { return strconv.Itoa(n) + " " + Plural(n, one, many) }

// Itoa is strconv's.
func Itoa(n int) string { return strconv.Itoa(n) }

// I64 writes an int64.
func I64(n int64) string { return strconv.FormatInt(n, 10) }

// Percent is n of total in percent, 0 to 100 (for an SVG's width).
func Percent(n, total int) string {
	if total <= 0 {
		return "0"
	}
	p := math.Min(100, math.Max(0, float64(n)*100/float64(total)))
	return strconv.FormatFloat(p, 'f', 1, 64)
}

// Short is the first n characters of a digest or a hash.
func Short(text string, n int) string {
	text = strings.TrimPrefix(text, "sha256:")
	if len(text) <= n {
		return text
	}
	return text[:n]
}

// SparkPoints is a sparkline's polyline points for values, in a 120×32 box.
func SparkPoints(values []float64) string {
	if len(values) < 2 {
		return ""
	}
	top := 1.0
	for _, v := range values {
		top = math.Max(top, v)
	}
	var b strings.Builder
	for i, v := range values {
		x := float64(i) * 120 / float64(len(values)-1)
		y := 30 - v/top*28
		fmt.Fprintf(&b, "%.1f,%.1f ", x, y)
	}
	return strings.TrimSpace(b.String())
}

// MapTitle is a map's display name (the Xbox's multiplayer maps'; another
// is its name).
func MapTitle(name string) string {
	if t, ok := mapTitles[strings.ToLower(name)]; ok {
		return t
	}
	return name
}

var mapTitles = map[string]string{
	"beavercreek": "Battle Creek", "bloodgulch": "Blood Gulch", "boardingaction": "Boarding Action",
	"carousel": "Derelict", "chillout": "Chill Out", "damnation": "Damnation", "dangercanyon": "Danger Canyon",
	"deathisland": "Death Island", "gephyrophobia": "Gephyrophobia", "hangemhigh": "Hang 'Em High",
	"icefields": "Ice Fields", "infinity": "Infinity", "longest": "Longest", "prisoner": "Prisoner",
	"putput": "Chiron TL-34", "ratrace": "Rat Race", "sidewinder": "Sidewinder", "timberland": "Timberland",
	"wizard": "Wizard", "a10": "Pillar of Autumn", "a30": "Halo", "a50": "Truth and Reconciliation",
	"b30": "The Silent Cartographer", "b40": "Assault on the Control Room", "c10": "343 Guilty Spark",
	"c20": "The Library", "c40": "Two Betrayals", "d20": "Keyes", "d40": "The Maw",
}

// Sorted returns a map's keys, sorted.
func Sorted[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Join joins words.
func Join(list []string, sep string) string { return strings.Join(list, sep) }

// Deref is an optional string's text.
func Deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
