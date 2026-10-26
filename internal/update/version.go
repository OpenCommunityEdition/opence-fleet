package update

import (
	"strconv"
	"strings"
)

// parse reads a release version, v<major>.<minor>.<patch> (ok false for
// another, such as a development build's "dev" or a commit's).
func parse(v string) (parts [3]int, ok bool) {
	rest, found := strings.CutPrefix(v, "v")
	fields := strings.Split(rest, ".")
	if !found || len(fields) != 3 {
		return parts, false
	}
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 || f != strconv.Itoa(n) {
			return parts, false
		}
		parts[i] = n
	}
	return parts, true
}

// Valid tells whether v is a release version.
func Valid(v string) bool {
	_, ok := parse(v)
	return ok
}

// Newer tells whether release version a is newer than b (false if either
// is not a release version).
func Newer(a, b string) bool {
	x, ok := parse(a)
	y, ok2 := parse(b)
	if !ok || !ok2 {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}
