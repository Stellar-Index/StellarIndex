// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// TestOrchestratorVWAPMaxAge_DerivesFromInterval pins #1294: VWAPMaxAge's
// "10 missed ticks at the default cadence" relationship existed only in
// a comment. An operator raising Config.Interval must get proportionally
// MORE missed-tick grace, not the flat cachekeys.VWAPMaxAge regardless of
// cadence — and a faster-than-default interval must not tighten the
// documented floor below cachekeys.VWAPMaxAge.
func TestOrchestratorVWAPMaxAge_DerivesFromInterval(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{"default 30s interval keeps the package default", cachekeys.VWAPMaxAge / 10, cachekeys.VWAPMaxAge},
		{"raised 60s interval widens the grace", 60 * time.Second, 10 * time.Minute},
		{"raised 5m interval widens the grace a lot", 5 * time.Minute, 50 * time.Minute},
		{"a faster-than-default interval is still floored", 5 * time.Second, cachekeys.VWAPMaxAge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := New(nil, nil, Config{Interval: tc.interval})
			if got := o.vwapMaxAge(); got != tc.want {
				t.Errorf("vwapMaxAge() with Interval=%v = %v, want %v", tc.interval, got, tc.want)
			}
		})
	}
}

// TestOrchestratorVWAPTTL_FlapsLessOnARaisedInterval: with the interval
// raised past what cachekeys.VWAPTTL(window) alone would grant, a long
// window's TTL must widen accordingly instead of expiring at the
// unwidened package default and flapping /v1/price between 200 and 404.
func TestOrchestratorVWAPTTL_FlapsLessOnARaisedInterval(t *testing.T) {
	o := New(nil, nil, Config{Interval: 60 * time.Second})
	window := 24 * time.Hour

	got := o.vwapTTL(window)
	if got != 10*time.Minute {
		t.Errorf("vwapTTL(24h) with Interval=60s = %v, want 10m (10 missed ticks)", got)
	}
	if unwidened := cachekeys.VWAPTTL(window); got == unwidened {
		t.Errorf("vwapTTL(24h) with a raised interval = %v, same as the unwidened package default %v — the interval must widen the grace", got, unwidened)
	}
}
