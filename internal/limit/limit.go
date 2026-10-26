// Package limit holds token buckets keyed by a string (an address's key from
// netaddr.Key, a publisher key, a server), at most MaximumKeys of them.
package limit

import (
	"sort"
	"sync"
	"time"
)

// MaximumKeys is how many keys a limiter holds by default. Past it, buckets
// are dropped to make room, those that restrict least first: the full ones
// (dropping one changes nothing), then the fullest, the least recently used
// of equally full ones. A drained bucket goes only once no fuller one is
// left, so that a flood of new keys costs a bounded amount of memory without
// giving back the tokens of a key it drained.
const MaximumKeys = 65536

// MaximumPerGroup is how many keys of one group (Group) a limiter holds by
// default: the /64s of an IPv6 /48 make room among themselves, not by
// dropping the buckets of other addresses.
const MaximumPerGroup = MaximumKeys / 16

// Limiter allows a burst of requests per key, refilled one each interval.
type Limiter struct {
	mu       sync.Mutex
	interval time.Duration
	burst    float64
	buckets  map[string]*bucket
	groups   map[string]int
	now      func() time.Time
	swept    time.Time
	// the most keys held
	Maximum int
	// Group, when set, names a key's group ("" for none), of which at most
	// PerGroup keys are held
	Group    func(key string) string
	PerGroup int
}

type bucket struct {
	tokens float64
	last   time.Time
	group  string
}

// New makes a limiter of burst requests, refilled one each interval.
func New(burst int, interval time.Duration, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{interval: interval, burst: float64(burst), buckets: map[string]*bucket{},
		groups: map[string]int{}, now: now, Maximum: MaximumKeys, PerGroup: MaximumPerGroup}
}

// PerMinute makes a limiter of n requests a minute (a burst of n).
func PerMinute(n int, now func() time.Time) *Limiter {
	if n < 1 {
		n = 1
	}
	return New(n, time.Minute/time.Duration(n), now)
}

// Grouped sets the limiter's Group and returns it.
func (l *Limiter) Grouped(group func(key string) string) *Limiter {
	l.Group = group
	return l
}

// tokens is a bucket's tokens at a time, refilled.
func (l *Limiter) tokens(b *bucket, now time.Time) float64 {
	if l.interval <= 0 {
		return l.burst
	}
	tokens := b.tokens + float64(now.Sub(b.last))/float64(l.interval)
	if tokens > l.burst {
		tokens = l.burst
	}
	return tokens
}

// Allow takes a token for key; if none is left it returns false and how
// long until one is.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.swept) > time.Minute {
		l.sweep(now)
	}
	b := l.buckets[key]
	if b == nil {
		group := ""
		if l.Group != nil {
			group = l.Group(key)
		}
		if group != "" && l.PerGroup > 0 && l.groups[group] >= l.PerGroup {
			l.evict(now, group, l.groups[group]-l.PerGroup+batch(l.PerGroup))
		}
		if l.Maximum > 0 && len(l.buckets) >= l.Maximum {
			l.evict(now, "", len(l.buckets)-l.Maximum+batch(l.Maximum))
		}
		b = &bucket{tokens: l.burst, last: now, group: group}
		l.buckets[key] = b
		if group != "" {
			l.groups[group]++
		}
	}
	b.tokens = l.tokens(b, now)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) * float64(l.interval))
	return false, wait
}

// batch is how many buckets one eviction drops when n are held: a 64th, so
// that the sort an eviction makes is paid for by many new keys.
func batch(n int) int {
	return max(1, n/64)
}

func (l *Limiter) remove(key string, b *bucket) {
	delete(l.buckets, key)
	if b.group != "" {
		if l.groups[b.group]--; l.groups[b.group] <= 0 {
			delete(l.groups, b.group)
		}
	}
}

// evict drops n buckets of a group ("" for any), those that restrict least
// first: the fullest, and of equally full ones the least recently used.
func (l *Limiter) evict(now time.Time, group string, n int) {
	type candidate struct {
		key    string
		b      *bucket
		tokens float64
	}
	list := make([]candidate, 0, len(l.buckets))
	for key, b := range l.buckets {
		if group == "" || b.group == group {
			list = append(list, candidate{key, b, l.tokens(b, now)})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].tokens != list[j].tokens {
			return list[i].tokens > list[j].tokens
		}
		return list[i].b.last.Before(list[j].b.last)
	})
	for i := 0; i < n && i < len(list); i++ {
		l.remove(list[i].key, list[i].b)
	}
}

// sweep drops the buckets back to their full burst (none restricts).
func (l *Limiter) sweep(now time.Time) {
	l.swept = now
	for key, b := range l.buckets {
		if l.tokens(b, now) >= l.burst {
			l.remove(key, b)
		}
	}
}

// Available tells whether key has a token left, without taking it.
func (l *Limiter) Available(key string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		return true
	}
	return l.tokens(b, now) >= 1
}

// Len is how many keys the limiter holds.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
