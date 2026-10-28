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

func TestFormat(t *testing.T) {
	cases := map[time.Duration]string{
		30 * 24 * time.Hour:    "30d",
		24 * time.Hour:         "1d",
		3 * time.Second:        "3s",
		90 * time.Minute:       "1h30m0s",
		36 * time.Hour:         "36h0m0s",
		0:                      "0s",
		150 * time.Millisecond: "150ms",
	}
	for d, want := range cases {
		if got := Format(d); got != want {
			t.Errorf("Format(%v) = %q, want %q", d, got, want)
		}
		if back, err := ParseDuration(Format(d)); err != nil || back != d {
			t.Errorf("round trip of %v: %v, %v", d, back, err)
		}
	}
}
