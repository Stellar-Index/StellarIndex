package cachekeys_test

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// TestVWAPTTL_BoundedBySilenceGrace: a published VWAP may not outlive its publisher by more than the silence grace. The
// aggregator rewrites every (pair, window) each tick, so the window is a span, not freshness; a window-keyed TTL would let
// `vwap:<pair>:86400` serve a day-old price as current (observed_at stamped at request time, no stale flag).
func TestVWAPTTL_BoundedBySilenceGrace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window time.Duration
		want   time.Duration
	}{
		// The two windows /v1/price accepts that exceed the grace —
		// these are the ones that carried the defect.
		{"24h window expires at the grace", 24 * time.Hour, cachekeys.VWAPMaxAge},
		{"1h window expires at the grace", time.Hour, cachekeys.VWAPMaxAge},
		// At and below the grace the window is already the tighter
		// bound and must be preserved byte-for-byte.
		{"5m window keeps its window", 5 * time.Minute, 5 * time.Minute},
		{"1m window keeps its window", time.Minute, time.Minute},
		{"zero window means do not cache", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cachekeys.VWAPTTL(tc.window); got != tc.want {
				t.Errorf("VWAPTTL(%v) = %v, want %v", tc.window, got, tc.want)
			}
		})
	}

	// The grace is load-bearing: 10 missed ticks at the 30 s default cadence (the same as FreezeTTL), comfortably above the 120 s
	// staleness the serving path already alarms on.
	if cachekeys.VWAPMaxAge != 5*time.Minute {
		t.Errorf("VWAPMaxAge = %v, want 5m (10 missed ticks at the 30s default cadence)",
			cachekeys.VWAPMaxAge)
	}
}

// TestVWAPTTLWithMaxAge_DerivesFromCaller: a caller whose tick interval differs from the package
// default must be able to pass its OWN derived grace rather than being stuck with VWAPMaxAge
// ("10 missed ticks") regardless of its cadence.
func TestVWAPTTLWithMaxAge_DerivesFromCaller(t *testing.T) {
	raisedGrace := 10 * time.Minute // e.g. a 60s interval: 10 missed ticks = 10m
	for _, tc := range []struct {
		name   string
		window time.Duration
		maxAge time.Duration
		want   time.Duration
	}{
		{"window above the raised grace is bounded by it", 24 * time.Hour, raisedGrace, raisedGrace},
		{"window below the raised grace keeps its window", 5 * time.Minute, raisedGrace, 5 * time.Minute},
		{"a tighter maxAge than the package default still bounds", time.Hour, time.Minute, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cachekeys.VWAPTTLWithMaxAge(tc.window, tc.maxAge); got != tc.want {
				t.Errorf("VWAPTTLWithMaxAge(%v, %v) = %v, want %v", tc.window, tc.maxAge, got, tc.want)
			}
		})
	}

	// VWAPTTL must be exactly VWAPTTLWithMaxAge at the package default —
	// no separate code path to drift out of step with it.
	for _, window := range []time.Duration{0, time.Minute, 5 * time.Minute, time.Hour, 24 * time.Hour} {
		if got, want := cachekeys.VWAPTTL(window), cachekeys.VWAPTTLWithMaxAge(window, cachekeys.VWAPMaxAge); got != want {
			t.Errorf("VWAPTTL(%v) = %v, want VWAPTTLWithMaxAge(_, VWAPMaxAge) = %v", window, got, want)
		}
	}
}

// TestConfidenceTTL_TracksVWAPTTLUnderTheBound: the confidence score is an attribute of the VWAP
// it scored, so it must expire with it by construction, or a 24 h-lived score outlives the value
// and gets attached to whatever the next publish writes.
func TestConfidenceTTL_TracksVWAPTTLUnderTheBound(t *testing.T) {
	for _, window := range []time.Duration{
		time.Minute, 5 * time.Minute, time.Hour, 24 * time.Hour,
	} {
		if got, want := cachekeys.ConfidenceTTL(window), cachekeys.VWAPTTL(window); got != want {
			t.Errorf("ConfidenceTTL(%v) = %v, want VWAPTTL = %v", window, got, want)
		}
	}
	if got := cachekeys.ConfidenceTTL(24 * time.Hour); got != cachekeys.VWAPMaxAge {
		t.Errorf("ConfidenceTTL(24h) = %v, want the %v bound", got, cachekeys.VWAPMaxAge)
	}
}
