package coingecko

import (
	"testing"
	"time"

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
