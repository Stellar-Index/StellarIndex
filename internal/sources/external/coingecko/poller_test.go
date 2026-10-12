package coingecko

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

func TestPollInterval_Default(t *testing.T) {
	p := NewPoller()
	if p.PollInterval() != DefaultPollInterval {
		t.Errorf("default = %v want %v", p.PollInterval(), DefaultPollInterval)
	}
	// Pin the demo-tier-safe 300s default so a future cadence drop
	// past 300s — which would push burn rate back toward the 10K/day
	// ceiling on a shared IP — fails this test.
	if DefaultPollInterval != 300*time.Second {
		t.Errorf("DefaultPollInterval = %v; expected 300s — dropping this below 300s risks tripping CoinGecko's 10K/day demo cap (F-0030)", DefaultPollInterval)
	}
}

func TestFloatToScaledInt_Precision(t *testing.T) {
	// 0.17582 at 10^8 should round to 17_582_000 (±1).
	got, err := scale.FloatToScaledInt(0.17582, 8)
	if err != nil {
		t.Fatalf("floatToScaledInt: %v", err)
	}
	n := got.Int64()
	if n < 17_581_999 || n > 17_582_001 {
		t.Errorf("0.17582 → %d, expected ≈17582000", n)
	}
	// Negative rejected.
	if _, err := scale.FloatToScaledInt(-1, 8); err == nil {
		t.Error("expected error for negative")
	}
}

// decimalStringToScaledInt converts decimal strings to scaled integers without float rounding; mirrors the exchangeratesapi/CMC
// helper.

func TestDecimalStringToScaledInt_edges(t *testing.T) {
	cases := []struct {
		in        string
		decimals  int
		want      string
		wantError bool
	}{
		{"1.0", 8, "100000000", false},
		{"-0.5", 8, "-50000000", false},
		{".25", 8, "25000000", false},
		{"1.999999999", 8, "199999999", false},
		{"42", 0, "42", false},
		{"", 8, "", true},
		{"1e3", 8, "", true},
		{"abc", 8, "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := scale.DecimalStringToScaledInt(c.in, c.decimals)
			if c.wantError {
				if err == nil {
					t.Errorf("expected error for %q, got %v", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.String() != c.want {
				t.Errorf("got %s, want %s", got.String(), c.want)
			}
		})
	}
}

// Coingecko is an aggregator, not an exchange: ClassAggregator keeps it out of the VWAP filter so upstream trades are not
// double-counted.

func TestPoller_NameAndClass(t *testing.T) {
	p := NewPoller()
	if got := p.Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
	if got := p.Class(); got != external.ClassAggregator {
		t.Errorf("Class() = %v, want ClassAggregator (CoinGecko is an index, not an exchange)", got)
	}
}

func TestPoller_PollInterval_defaultAndOverride(t *testing.T) {
	p := NewPoller()
	p.Interval = 0
	if got := p.PollInterval(); got != DefaultPollInterval {
		t.Errorf("PollInterval(zero) = %v, want %v", got, DefaultPollInterval)
	}
	p.Interval = 7 * time.Second
	if got := p.PollInterval(); got != 7*time.Second {
		t.Errorf("PollInterval(7s) = %v, want 7s", got)
	}
}
