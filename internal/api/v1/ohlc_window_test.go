package v1

import (
	"testing"
	"time"
)

func TestParseWindowDuration_DayOverflowRejected(t *testing.T) {
	for _, in := range []string{"213504d", "106752d", "-106752d", "9223372036854775807d"} {
		if d, err := parseWindowDuration(in); err == nil {
			t.Errorf("parseWindowDuration(%q) = %v, want error", in, d)
		}
	}
}

func TestParseWindowDuration_Valid(t *testing.T) {
	cases := map[string]time.Duration{
		"7d":      7 * 24 * time.Hour,
		"106751d": 106751 * 24 * time.Hour,
		"90m":     90 * time.Minute,
	}
	for in, want := range cases {
		got, err := parseWindowDuration(in)
		if err != nil || got != want {
			t.Errorf("parseWindowDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}
