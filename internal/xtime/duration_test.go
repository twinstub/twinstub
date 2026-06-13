package xtime

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{"150ms", 150 * time.Millisecond, false},
		{"30s", 30 * time.Second, false},
		{"30m", 30 * time.Minute, false},
		{"72h", 72 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"1.5d", 36 * time.Hour, false},
		{"0ms", 0, false},
		{"1d2h", 0, true},
		{"", 0, true},
		{"5", 0, true},
		{"abc", 0, true},
	}
	for _, c := range cases {
		got, err := ParseDuration(c.in)
		if c.err {
			if err == nil {
				t.Errorf("ParseDuration(%q): expected error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDuration(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
