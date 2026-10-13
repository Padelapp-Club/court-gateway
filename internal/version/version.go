// Package version compares release versions of the form MAJOR.MINOR.PATCH
// with an optional leading "v". Anything else (for example "dev") sorts
// below every release.
package version

import (
	"strconv"
	"strings"
)

// Parse returns the three numeric parts and whether s is a release version.
func Parse(s string) ([3]int, bool) {
	var out [3]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// Valid reports whether s is a release version.
func Valid(s string) bool {
	_, ok := Parse(s)
	return ok
}

// Compare returns -1, 0 or 1 as a is older than, equal to or newer than b.
func Compare(a, b string) int {
	pa, oka := Parse(a)
	pb, okb := Parse(b)
	switch {
	case !oka && !okb:
		return 0
	case !oka:
		return -1
	case !okb:
		return 1
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Newer reports whether candidate is strictly newer than current.
func Newer(candidate, current string) bool {
	return Valid(candidate) && Compare(candidate, current) > 0
}
