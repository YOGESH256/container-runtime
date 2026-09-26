// Package units parses and formats human-friendly sizes.
package units

import (
	"fmt"
	"strconv"
	"strings"
)

var suffixes = map[string]int64{
	"": 1, "b": 1,
	"k": 1 << 10, "kb": 1 << 10, "kib": 1 << 10,
	"m": 1 << 20, "mb": 1 << 20, "mib": 1 << 20,
	"g": 1 << 30, "gb": 1 << 30, "gib": 1 << 30,
}

// ParseBytes turns "64m", "1.5g", "512KiB" or "1048576" into bytes.
// Suffixes are binary (k = 1024), matching docker's --memory flag.
func ParseBytes(s string) (int64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	i := strings.IndexFunc(t, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	num, suffix := t, ""
	if i >= 0 {
		num, suffix = t[:i], strings.TrimSpace(t[i:])
	}
	mult, ok := suffixes[suffix]
	if !ok || num == "" {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(f * float64(mult)), nil
}

// FormatBytes renders n with a binary suffix, e.g. 67108864 -> "64M".
func FormatBytes(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%dG", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%dM", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%dK", n>>10)
	}
	return fmt.Sprintf("%dB", n)
}
