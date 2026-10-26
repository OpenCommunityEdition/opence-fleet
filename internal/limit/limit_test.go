package limit

import (
	"fmt"
	"testing"
	"time"
)

func TestBurstAndRefill(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(2, 5*time.Second, func() time.Time { return now })
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("s"); !ok {
			t.Fatalf("request %d refused", i)
		}
	}
	ok, wait := l.Allow("s")
	if ok || wait <= 0 || wait > 5*time.Second {
		t.Fatalf("third: %v %v", ok, wait)
	}
	if ok, _ := l.Allow("other"); !ok {
		t.Fatal("keys share a bucket")
	}
	now = now.Add(5 * time.Second)
	if ok, _ := l.Allow("s"); !ok {
		t.Fatal("no refill")
	}
	if ok, _ := l.Allow("s"); ok {
		t.Fatal("refilled more than one")
	}
	if l.Available("s") {
		t.Fatal("available with none left")
	}
}

func TestPerMinute(t *testing.T) {
	now := time.Unix(1000, 0)
	l := PerMinute(60, func() time.Time { return now })
	for i := 0; i < 60; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d refused", i)
		}
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("61st allowed")
	}
	now = now.Add(time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("not one a second")
	}
}

func TestMaximumKeys(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(2, time.Minute, func() time.Time { return now })
	l.Maximum = 100
	for i := 0; i < 1000; i++ {
		l.Allow(fmt.Sprint("key", i))
		if l.Len() > 100 {
			t.Fatalf("%d keys held", l.Len())
		}
	}
	// a drained key stays drained while the buckets refilled are dropped
	l = New(2, time.Hour, func() time.Time { return now })
	l.Maximum = 10
	for i := 0; i < 9; i++ {
		l.Allow(fmt.Sprint("old", i))
	}
	now = now.Add(3 * time.Hour)
	l.Allow("drained")
	l.Allow("drained")
	for i := 0; i < 5; i++ {
		l.Allow(fmt.Sprint("new", i))
	}
	if ok, _ := l.Allow("drained"); ok || l.Len() > 10 {
		t.Fatalf("drained key reset (%v), %d keys held", ok, l.Len())
	}
}

// A flood of new keys does not drop the buckets it drained (nor anyone
// else's), however many keys it brings: the fuller buckets go first.
func TestFloodKeepsDrainedBuckets(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(5, time.Second, func() time.Time { return now })
	l.Maximum = 64
	// full buckets (idle long enough to refill, not yet swept) are dropped
	// before any other
	for i := 0; i < 10; i++ {
		l.Allow(fmt.Sprint("idle", i))
	}
	now = now.Add(10 * time.Second)
	for i := 0; i < 5; i++ {
		l.Allow("drained")
	}
	if ok, _ := l.Allow("drained"); ok {
		t.Fatal("not drained")
	}
	// (two tokens taken: more restricted than the flood's buckets, one each)
	l.Allow("used")
	l.Allow("used")
	idle := func() int {
		l.mu.Lock()
		defer l.mu.Unlock()
		n := 0
		for j := 0; j < 10; j++ {
			if _, ok := l.buckets[fmt.Sprint("idle", j)]; ok {
				n++
			}
		}
		return n
	}
	for i := 0; i < 10000; i++ {
		l.Allow(fmt.Sprint("flood", i))
		if l.Len() > 64 {
			t.Fatalf("%d keys held", l.Len())
		}
		// (64 held from the 52nd of the flood: 10 idle, drained, used and 52
		// of the flood; each next one makes room by dropping an idle one)
		if want := min(10, max(0, 61-i)); i < 70 && idle() != want {
			t.Fatalf("after %d of the flood, %d idle buckets held, not %d", i+1, idle(), want)
		}
	}
	if ok, _ := l.Allow("drained"); ok {
		t.Fatal("the flood gave a drained key its tokens back")
	}
	l.mu.Lock()
	_, used := l.buckets["used"]
	l.mu.Unlock()
	if !used {
		t.Fatal("a bucket more restricted than the flood's own was dropped")
	}
}

// With every bucket drained, room is made by dropping the least recently
// used.
func TestEvictLeastRecentlyUsedWhenAllDrained(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(1, time.Hour, func() time.Time { return now })
	l.Maximum = 4
	for i := 0; i < 4; i++ {
		l.Allow(fmt.Sprint("k", i))
		now = now.Add(time.Second)
	}
	l.Allow("new")
	l.mu.Lock()
	_, oldest := l.buckets["k0"]
	_, newest := l.buckets["k3"]
	l.mu.Unlock()
	if oldest || !newest || l.Len() != 4 {
		t.Fatalf("oldest kept %v, newest kept %v, %d held", oldest, newest, l.Len())
	}
}

// One group's keys (an IPv6 /48's /64s) make room among themselves.
func TestPerGroup(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(5, time.Hour, func() time.Time { return now }).Grouped(func(key string) string {
		if len(key) > 6 && key[:6] == "flood/" {
			return "flood"
		}
		return ""
	})
	l.Maximum, l.PerGroup = 1000, 50
	l.Allow("other")
	for i := 0; i < 5; i++ {
		l.Allow("flood/drained")
	}
	for i := 0; i < 5000; i++ {
		l.Allow(fmt.Sprint("flood/", i))
		l.mu.Lock()
		n := l.groups["flood"]
		l.mu.Unlock()
		if n > 50 {
			t.Fatalf("%d keys of the group held", n)
		}
	}
	if l.Len() > 51 {
		t.Fatalf("%d keys held", l.Len())
	}
	if ok, _ := l.Allow("flood/drained"); ok {
		t.Fatal("the group's own flood reset its drained key")
	}
	l.mu.Lock()
	_, other := l.buckets["other"]
	l.mu.Unlock()
	if !other {
		t.Fatal("the group's flood dropped another key's bucket")
	}
}
