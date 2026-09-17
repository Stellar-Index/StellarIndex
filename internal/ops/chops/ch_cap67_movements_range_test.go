package chops

import (
	"testing"
	"time"
)

// TestResolveStart pins the first-run clamp that unblocked the test nets'
// empty account_movements archive (2026-09-17): a floor of genesis=1
// against a lake that begins at ledger 2 — every net's lake, ledger 1 is
// never exported — must start at 2, because ContiguousWatermark reads a
// `from` the lake does not hold as a boundary hole and answers from-1
// forever. Red on the pre-fix code: start = floor = 1, and the daemon
// idled for months without deriving a row. A resumed run and a pubnet
// floor above the lake's start are unchanged.
func TestResolveStart(t *testing.T) {
	for _, tc := range []struct {
		name               string
		wm, floor, lakeMin uint32
		want               uint32
	}{
		{"test net first run: floor genesis, lake begins at 2", 0, 1, 2, 2},
		{"resumed run continues at wm+1 whatever the floor and lake", 64_476_027, 1, 2, 64_476_028},
		{"resumed run ignores a lake min above the watermark", 100, 1, 500, 101},
		{"pubnet first run: floor above the lake's start stays the floor", 0, 58_762_517, 2, 58_762_517},
		{"floor equal to the lake's first ledger", 0, 2, 2, 2},
		{"empty lake leaves the floor alone (idles until the lake reaches it)", 0, 1, 0, 1},
		{"partial lake beginning above genesis clamps up to it", 0, 1, 4_500_000, 4_500_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveStart(tc.wm, tc.floor, tc.lakeMin); got != tc.want {
				t.Fatalf("resolveStart(wm=%d, floor=%d, lakeMin=%d) = %d, want %d",
					tc.wm, tc.floor, tc.lakeMin, got, tc.want)
			}
		})
	}
}

// TestFollowTick pins the idle backoff: the base tick through
// cap67IdleBackoffAfter consecutive idle ticks (a live feed idles a few
// ticks per ledger and must not slow down), doubling past that up to
// cap67IdleMaxInterval, never below the base an operator asked for.
func TestFollowTick(t *testing.T) {
	const base = time.Second
	for _, tc := range []struct {
		name string
		base time.Duration
		idle int
		want time.Duration
	}{
		{"just worked", base, 0, base},
		{"a few idle ticks inside one ledger cadence", base, 4, base},
		{"at the threshold still the base", base, cap67IdleBackoffAfter, base},
		{"one past the threshold doubles", base, cap67IdleBackoffAfter + 1, 2 * base},
		{"two past the threshold doubles again", base, cap67IdleBackoffAfter + 2, 4 * base},
		{"five past the threshold is capped", base, cap67IdleBackoffAfter + 5, cap67IdleMaxInterval},
		{"a long stall stays capped", base, 10_000, cap67IdleMaxInterval},
		{"a base above the cap is never shortened", 2 * cap67IdleMaxInterval, 10_000, 2 * cap67IdleMaxInterval},
		{"a sub-second base backs off from its own value", 500 * time.Millisecond, cap67IdleBackoffAfter + 3, 4 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := followTick(tc.base, tc.idle); got != tc.want {
				t.Fatalf("followTick(%s, idle=%d) = %s, want %s", tc.base, tc.idle, got, tc.want)
			}
		})
	}
}
