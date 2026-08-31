package main

import (
	"testing"
	"time"
)

// Wave-D PFR-04 observed, correctly, that NOTHING in the repo exercised
// storePriceReader: its `now func() time.Time` and `vwapFreshness
// time.Duration` seams — which exist for no reason other than to be
// injected by a test — were dead, so the CS-017 staleness rule had
// enforcement tier NONE beyond runtime.
//
// PFR-04's FAILURE SCENARIO does not survive: it claimed the ~250k
// dormant/delisted long tail would resume being served a months-old
// bucket with stale=false if the CS-017 term were lost. It cannot. The
// substance gate runs twelve lines earlier in LatestPrice, its window
// is trailing-24h, and a pair dormant for months yields volume=0 and
// fails the first comparison — so the read returns ErrPriceWithheld and
// the staleness expression is never evaluated. That gate is on by
// default and is itself tested (internal/pricingguard/substance_test.go).
//
// What survives is the coverage gap itself, and this closes the part of
// it that CAN be closed here. LatestPrice is NOT unit-testable from
// this package: storePriceReader.s is a concrete *timescale.Store with
// an unexported db field and no injectable constructor, so exercising
// the read end-to-end needs the testcontainers integration harness, not
// a fake. These tests pin the two seams the CS-017 fix actually
// parameterises — the default window and the clock — so the constant
// cannot be silently changed and the nil-fallbacks cannot rot.

// TestStorePriceReaderFreshnessDefault pins the CS-017 window.
//
// 15 minutes is a deliberate choice between two failure modes: well
// above the structural 1-2 minute closed-bucket floor, so an ACTIVE
// pair is never falsely marked stale; and decisive on a genuinely
// dormant one, which is the bug it was introduced for — a 200-day-old
// VWAP served with stale=false.
func TestStorePriceReaderFreshnessDefault(t *testing.T) {
	t.Parallel()

	var zero storePriceReader
	if got := zero.freshnessWindow(); got != defaultVWAPFreshness {
		t.Errorf("zero-value freshnessWindow() = %v, want the default %v", got, defaultVWAPFreshness)
	}
	if defaultVWAPFreshness != 15*time.Minute {
		t.Errorf("defaultVWAPFreshness = %v, want 15m — see the CS-017 rationale above "+
			"before changing it: too low falsely marks active pairs stale, too high "+
			"re-opens the 200-day-old-bucket bug", defaultVWAPFreshness)
	}
	// A configured window must win, or the seam is decorative.
	r := storePriceReader{vwapFreshness: 90 * time.Second}
	if got := r.freshnessWindow(); got != 90*time.Second {
		t.Errorf("freshnessWindow() = %v, want the configured 90s", got)
	}
	// Zero means "unset" — the documented sentinel — not "never stale".
	r = storePriceReader{vwapFreshness: 0}
	if got := r.freshnessWindow(); got != defaultVWAPFreshness {
		t.Errorf("freshnessWindow() with an explicit 0 = %v, want the default %v — 0 is the "+
			"unset sentinel; treating it as a zero-length window would mark EVERY "+
			"bucket stale", got, defaultVWAPFreshness)
	}
}

// TestStorePriceReaderClock pins the injectable clock. Without it the
// staleness comparison can only ever be tested against wall time, which
// is why it had no test at all.
func TestStorePriceReaderClock(t *testing.T) {
	t.Parallel()

	var zero storePriceReader
	before := time.Now()
	got := zero.clock()
	if got.Before(before) || got.After(time.Now()) {
		t.Errorf("zero-value clock() = %v, want a time.Now() reading between %v and now",
			got, before)
	}

	fixed := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	r := storePriceReader{now: func() time.Time { return fixed }}
	if c := r.clock(); !c.Equal(fixed) {
		t.Errorf("clock() = %v, want the injected %v", c, fixed)
	}
}

// TestStorePriceReaderStalenessBoundary states the rule the two seams
// exist to make testable, at its boundary.
//
// It mirrors LatestPrice's expression exactly, including the two
// details that are easy to get wrong:
//
//		stale := lowConfidence || r.clock().Sub(bucket.Add(time.Minute)) > r.freshnessWindow()
//
//	  - the age is measured from the bucket's CLOSE (start + 1 minute),
//	    not its start — a 1-minute CAGG bucket is not closed until its
//	    minute elapses, so measuring from the start would report every
//	    bucket a minute older than it is and shift the boundary;
//	  - `lowConfidence` short-circuits, so a low-confidence read is stale
//	    regardless of age.
//
// Reproduced rather than called: storePriceReader.s is a concrete
// *timescale.Store with an unexported db field and no injectable
// constructor, so the surrounding read needs the testcontainers
// harness. This pins the arithmetic and the boundary; the integration
// suite owns the wiring.
func TestStorePriceReaderStalenessBoundary(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	r := storePriceReader{now: func() time.Time { return now }}

	// stale mirrors LatestPrice's expression. bucketStart is the CAGG
	// row's `bucket` column; the +1m is the close.
	stale := func(bucketStart time.Time, lowConfidence bool) bool {
		return lowConfidence || r.clock().Sub(bucketStart.Add(time.Minute)) > r.freshnessWindow()
	}

	for _, tc := range []struct {
		name          string
		closeAge      time.Duration // age of the bucket's CLOSE
		lowConfidence bool
		wantStale     bool
	}{
		{"fresh bucket, one minute past close", time.Minute, false, false},
		{"just inside the window", defaultVWAPFreshness - time.Second, false, false},
		{"exactly at the window", defaultVWAPFreshness, false, false},
		{"just past the window", defaultVWAPFreshness + time.Second, false, true},
		{"the CS-017 bug: 200 days old", 200 * 24 * time.Hour, false, true},
		{"low confidence is stale however fresh", time.Minute, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bucketStart := now.Add(-tc.closeAge).Add(-time.Minute)
			if got := stale(bucketStart, tc.lowConfidence); got != tc.wantStale {
				t.Errorf("close %v old (lowConfidence=%v): stale = %v, want %v",
					tc.closeAge, tc.lowConfidence, got, tc.wantStale)
			}
		})
	}
}
