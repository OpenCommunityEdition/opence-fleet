package view

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// Window is a graph's span, which the page's ?window= names.
type Window struct {
	Name, Label string
	Span        time.Duration
}

// Windows is the spans a node's graphs offer.
var Windows = []Window{{"1h", "1 hour", time.Hour}, {"24h", "24 hours", 24 * time.Hour}, {"7d", "7 days", 7 * 24 * time.Hour}}

// WindowOf is the window a name names (24 hours by default).
func WindowOf(name string) Window {
	for _, w := range Windows {
		if w.Name == name {
			return w
		}
	}
	return Windows[1]
}

// The graphs' box (the SVG's viewBox; drawn stretched, the strokes not).
const (
	ChartWidth  = 600
	ChartHeight = 120
	// at most this many columns, each a mean of its samples (a sample a
	// minute: an hour is drawn whole)
	chartColumns = 120
)

// Chart is a graph of a node's health over a window: its series' paths
// in the ChartWidth × ChartHeight box, a gap where there was no sample (the
// node offline) or the value was not known (an older agent), and a column
// a hover names.
type Chart struct {
	Title string
	// the latest value, in words; the axis's top
	Latest, Top string
	Series      []ChartSeries
	Columns     []ChartColumn
	From, To    string
	// no value at all in the window
	Empty bool
}

// ChartSeries is a line of a chart.
type ChartSeries struct {
	Name string
	// its colour's class (text-series-1, …: the path strokes currentColor)
	Class string
	Path  string
	// its latest value, in words
	Latest string
}

// ChartColumn is a column's hover: where it is, and its values in words.
type ChartColumn struct {
	X, Width string
	Title    string
}

// series is a chart's values before it is drawn: one per column, NaN
// where none.
type series struct {
	name, class string
	value       func(db.NodeSample) (float64, bool)
	words       func(float64) string
}

func percentOf(used, total int64) (float64, bool) {
	if total <= 0 {
		return 0, false
	}
	return float64(used) * 100 / float64(total), true
}

func percentWords(v float64) string { return strconv.FormatFloat(v, 'f', 0, 64) + " %" }

// Rate writes bytes a second shortly.
func Rate(v float64) string { return Bytes(int64(math.Round(v))) + "/s" }

// NodeCharts is a node's graphs over a window ending now: its CPU, its
// memory and its disk (shares of the whole, 0 to 100 %) and its network,
// in and out on one axis of bytes a second.
func NodeCharts(samples []db.NodeSample, now time.Time, w Window, zone *time.Location) []Chart {
	pointer := func(p *float64) (float64, bool) {
		if p == nil {
			return 0, false
		}
		return *p, true
	}
	cpu := series{name: "CPU", class: "text-series-1", value: func(s db.NodeSample) (float64, bool) { return pointer(s.Cpu) },
		words: percentWords}
	memory := series{name: "Memory", class: "text-series-1", words: percentWords,
		value: func(s db.NodeSample) (float64, bool) { return percentOf(s.MemoryUsed, s.MemoryTotal) }}
	disk := series{name: "Disk", class: "text-series-1", words: percentWords,
		value: func(s db.NodeSample) (float64, bool) { return percentOf(s.DiskUsed, s.DiskTotal) }}
	in := series{name: "in", class: "text-series-1", words: Rate,
		value: func(s db.NodeSample) (float64, bool) { return pointer(s.NetIn) }}
	out := series{name: "out", class: "text-series-2", words: Rate,
		value: func(s db.NodeSample) (float64, bool) { return pointer(s.NetOut) }}
	charts := []Chart{
		chartOf("CPU", samples, now, w, zone, 100, cpu),
		chartOf("Memory", samples, now, w, zone, 100, memory),
		chartOf("Disk", samples, now, w, zone, 100, disk),
		chartOf("Network", samples, now, w, zone, 0, in, out),
	}
	// (the latest memory and disk in bytes too)
	if n := len(samples); n > 0 {
		last := samples[n-1]
		if last.MemoryTotal > 0 && !charts[1].Empty {
			charts[1].Latest += " · " + Bytes(last.MemoryUsed) + " of " + Bytes(last.MemoryTotal)
		}
		if last.DiskTotal > 0 && !charts[2].Empty {
			charts[2].Latest += " · " + Bytes(last.DiskUsed) + " of " + Bytes(last.DiskTotal)
		}
	}
	return charts
}

// chartOf draws series over a window: top is the axis's (0: the values'
// own, rounded up).
func chartOf(title string, samples []db.NodeSample, now time.Time, w Window, zone *time.Location, top float64,
	all ...series) Chart {
	columns := int(min(chartColumns, w.Span/time.Minute))
	from := now.Add(-w.Span)
	step := w.Span / time.Duration(columns)
	values := make([][]float64, len(all))
	for k, s := range all {
		sums, counts := make([]float64, columns), make([]int, columns)
		for _, sample := range samples {
			at := time.Unix(sample.Time, 0)
			if at.Before(from) || !at.Before(now) {
				continue
			}
			v, ok := s.value(sample)
			if !ok {
				continue
			}
			column := min(columns-1, int(at.Sub(from)/step))
			sums[column] += v
			counts[column]++
		}
		values[k] = make([]float64, columns)
		for c := range values[k] {
			values[k][c] = math.NaN()
			if counts[c] > 0 {
				values[k][c] = sums[c] / float64(counts[c])
			}
		}
	}
	chart := Chart{Title: title, Empty: true}
	highest := 0.0
	for k := range all {
		for _, v := range values[k] {
			if !math.IsNaN(v) {
				chart.Empty = false
				highest = math.Max(highest, v)
			}
		}
	}
	if top == 0 {
		top = niceTop(highest)
	}
	chart.Top = all[0].words(top)
	layout := "15:04"
	if w.Span > 24*time.Hour {
		layout = "Mon 15:04"
	}
	if zone == nil {
		zone = time.UTC
	}
	chart.From, chart.To = from.In(zone).Format(layout), "now"
	var latest []string
	for k, s := range all {
		line := ChartSeries{Name: s.name, Class: s.class, Path: path(values[k], top), Latest: "—"}
		for c := columns - 1; c >= 0; c-- {
			if !math.IsNaN(values[k][c]) {
				line.Latest = s.words(values[k][c])
				break
			}
		}
		chart.Series = append(chart.Series, line)
		if len(all) > 1 {
			latest = append(latest, s.name+" "+line.Latest)
		} else {
			latest = append(latest, line.Latest)
		}
	}
	chart.Latest = strings.Join(latest, " · ")
	width := float64(ChartWidth) / float64(columns)
	for c := 0; c < columns; c++ {
		var words []string
		for k, s := range all {
			if v := values[k][c]; !math.IsNaN(v) {
				name := ""
				if len(all) > 1 {
					name = s.name + " "
				}
				words = append(words, name+s.words(v))
			}
		}
		if len(words) == 0 {
			continue
		}
		at := from.Add(time.Duration(c) * step).In(zone).Format(layout)
		chart.Columns = append(chart.Columns, ChartColumn{X: coordinate(float64(c) * width), Width: coordinate(width),
			Title: at + " · " + strings.Join(words, ", ")})
	}
	return chart
}

// path is a line through the columns' values (NaN: a gap), each at its
// column's middle; a value alone is a dot (a short segment, its round caps
// drawn).
func path(values []float64, top float64) string {
	var b strings.Builder
	width := float64(ChartWidth) / float64(len(values))
	open := false
	for c, v := range values {
		if math.IsNaN(v) {
			open = false
			continue
		}
		x := (float64(c) + 0.5) * width
		y := ChartHeight - 2 - math.Min(v, top)/top*(ChartHeight-6)
		next := c+1 < len(values) && !math.IsNaN(values[c+1])
		switch {
		case open:
			fmt.Fprintf(&b, "L%s %s ", coordinate(x), coordinate(y))
		case next:
			fmt.Fprintf(&b, "M%s %s ", coordinate(x), coordinate(y))
		default:
			fmt.Fprintf(&b, "M%s %s h0.1 ", coordinate(x), coordinate(y))
		}
		open = next
	}
	return strings.TrimSpace(b.String())
}

func coordinate(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

// niceTop is a rate axis's top above a value: 1, 2 or 5 times a power of
// ten of KB, MB or GB a second (1 KB/s at least, so that a quiet line
// stays low).
func niceTop(v float64) float64 {
	if v <= 1024 {
		return 1024
	}
	unit := math.Pow(1024, math.Floor(math.Log(v)/math.Log(1024)))
	scaled := v / unit
	power := math.Pow(10, math.Floor(math.Log10(scaled)))
	for _, m := range []float64{1, 2, 5} {
		if m*power >= scaled {
			return m * power * unit
		}
	}
	return 10 * power * unit
}
