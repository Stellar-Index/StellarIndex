package cachekeys_test

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// TestVWAPTTL_BoundedBySilenceGrace pins the F034 contract: a published
// VWAP may not outlive its publisher by more than the silence grace.
//
// The aggregator recomputes and re-writes EVERY configured (pair,
// window) on every tick, so the window is an aggregation span, not a
// freshness claim. Keying the TTL to the window let `vwap:<pair>:86400`
// survive 24 h after the aggregator stopped, and `/v1/price?window=86400`
// serves that key with `observed_at` stamped at request time and no
// stale flag — i.e. a day-old price asserted as current, undetectable
// from the payload.
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

	// The grace itself is load-bearing: 10 missed ticks at the 30 s
	// default cadence, the same number FreezeTTL uses for the same
	// reason, and comfortably above the 120 s staleness the serving
	// path already alarms on.
	if cachekeys.VWAPMaxAge != 5*time.Minute {
		t.Errorf("VWAPMaxAge = %v, want 5m (10 missed ticks at the 30s default cadence)",
			cachekeys.VWAPMaxAge)
	}
}

// TestVWAPTTLWithMaxAge_DerivesFromCaller pins #1294: VWAPMaxAge's
// "10 missed ticks" relationship to the tick interval existed only in
// prose. A caller whose interval differs from the package default must
// be able to pass its OWN derived grace rather than being stuck with
// VWAPMaxAge regardless of its cadence.
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

// TestConfidenceTTL_TracksVWAPTTLUnderTheBound — the confidence score is
// an attribute of the VWAP it scored, so it must expire with it. Before
// the bound both were "the window" and agreed by accident; they have to
// agree by construction, or a 24 h-lived score outlives the value and
// gets attached to whatever the next publish writes.
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
