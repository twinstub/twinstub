// Package xtime extends time.ParseDuration with a day unit, which the
// standard library does not support but the DSL needs (e.g. "7d").
package xtime

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

var dayRe = regexp.MustCompile(`^(\d+(?:\.\d+)?)d$`)

// ParseDuration accepts everything time.ParseDuration accepts, plus a pure
// day form like "7d" or "1.5d". Mixed forms ("1d2h") are not supported.
func ParseDuration(s string) (time.Duration, error) {
	if m := dayRe.FindStringSubmatch(s); m != nil {
		days, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (examples: 150ms, 30s, 30m, 72h, 7d)", s)
	}
	return d, nil
}
