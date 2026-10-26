package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
)

func TestLogRing(t *testing.T) {
	var r logRing
	for i := 1; i <= ringSize+10; i++ {
		line := r.add(agentproto.Log{Cat: "server", Text: fmt.Sprint(i)})
		if line.N != int64(i) {
			t.Fatalf("line %d numbered %d", i, line.N)
		}
	}
	if len(r.lines) != ringSize || r.lines[0].N != 11 {
		t.Fatalf("%d lines from %d", len(r.lines), r.lines[0].N)
	}
	// after: the lines past n, at most limit
	got := r.after(ringSize+5, 0)
	if len(got) != 5 || got[0].N != ringSize+6 {
		t.Fatalf("%+v", got)
	}
	if got := r.after(0, 3); len(got) != 3 || got[0].N != 11 || got[2].N != 13 {
		t.Fatalf("%+v", got)
	}
	if got := r.after(ringSize+10, 10); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
	if got := r.after(0, 1<<20); len(got) != ringSize {
		t.Fatalf("%d", len(got))
	}

	// tail: the last n, without the private ones
	var small logRing
	small.add(agentproto.Log{Cat: "server", Text: "one"})
	small.add(agentproto.Log{Cat: "chat", Text: "a secret", Private: true})
	small.add(agentproto.Log{Cat: "game", Text: "two"})
	if got := small.tail(10); !reflect.DeepEqual(got, []string{"server: one", "game: two"}) {
		t.Fatalf("%q", got)
	}
	if got := small.tail(1); !reflect.DeepEqual(got, []string{"game: two"}) {
		t.Fatalf("%q", got)
	}
	if got := small.tail(2); !reflect.DeepEqual(got, []string{"game: two"}) {
		t.Fatalf("%q", got)
	}
}

func TestEventRing(t *testing.T) {
	var r eventRing
	for i := 1; i <= 600; i++ {
		r.add(agentproto.Event{Seq: int64(i)})
	}
	all := r.all()
	if len(all) != 512 || all[0].Seq != 89 || all[511].Seq != 600 {
		t.Fatalf("%d from %d", len(all), all[0].Seq)
	}
	// (a copy)
	all[0].Seq = 0
	if r.all()[0].Seq != 89 {
		t.Fatal("all is not a copy")
	}
}

func TestLineWriter(t *testing.T) {
	var lines []string
	w := &lineWriter{line: func(s string) { lines = append(lines, s) }}
	w.Write([]byte("one\r\ntw"))
	w.Write([]byte("o\n\nthree"))
	if !reflect.DeepEqual(lines, []string{"one", "two"}) {
		t.Fatalf("%q", lines)
	}
	w.flush()
	if !reflect.DeepEqual(lines, []string{"one", "two", "three"}) {
		t.Fatalf("%q", lines)
	}
	w.flush()
	if len(lines) != 3 {
		t.Fatal("an empty flush made a line")
	}
	// a line without its end past 64 KiB is cut
	lines = nil
	long := strings.Repeat("x", 64<<10+1)
	if n, err := w.Write([]byte(long)); n != len(long) || err != nil {
		t.Fatal(n, err)
	}
	if len(lines) != 1 || lines[0] != long || w.buffer.Len() != 0 {
		t.Fatalf("%d lines", len(lines))
	}
}

func TestRotatingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stdout.txt")
	read := func(name string) string {
		data, err := os.ReadFile(name)
		if err != nil {
			return "(none)"
		}
		return string(data)
	}
	f := &rotatingFile{path: path, max: 10, keep: 2}
	f.WriteString("aaaaa\n")
	f.WriteString("bbb\n")
	if got := read(path); got != "aaaaa\nbbb\n" {
		t.Fatalf("%q", got)
	}
	// past max: rotated
	f.WriteString("ccccc\n")
	if read(path) != "ccccc\n" || read(path+".1") != "aaaaa\nbbb\n" {
		t.Fatalf("%q %q", read(path), read(path+".1"))
	}
	f.WriteString("ddddd\n")
	f.WriteString("eeeee\n")
	// keep 2: the oldest is gone
	if read(path) != "eeeee\n" || read(path+".1") != "ddddd\n" || read(path+".2") != "ccccc\n" ||
		read(path+".3") != "(none)" {
		t.Fatalf("%q %q %q %q", read(path), read(path+".1"), read(path+".2"), read(path+".3"))
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal("a second close:", err)
	}

	// opened again: appended to, counting what is there
	g := &rotatingFile{path: path, max: 10, keep: 2}
	g.WriteString("ff\n")
	if read(path) != "eeeee\nff\n" {
		t.Fatalf("%q", read(path))
	}
	g.WriteString("gg\n")
	if read(path) != "gg\n" || read(path+".1") != "eeeee\nff\n" || read(path+".2") != "ddddd\n" {
		t.Fatalf("%q %q %q", read(path), read(path+".1"), read(path+".2"))
	}
	g.Close()

	// a folder that is not there: the lines are dropped, not an error
	h := &rotatingFile{path: filepath.Join(dir, "nowhere", "x.txt"), max: 10, keep: 2}
	if n, err := h.Write([]byte("x\n")); n != 2 || err != nil {
		t.Fatal(n, err)
	}
}
