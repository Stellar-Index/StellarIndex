package wiring

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// StorePriceReader's `now func() time.Time` and `vwapFreshness time.Duration` seams exist only to
// be injected by a test, so the staleness rule needs a test that drives them.
//
// Losing the staleness term would not, on its own, serve the dormant long tail as a months-old
// bucket with stale=false: the substance gate runs earlier in LatestPrice over a trailing-24h
// window, a dormant pair has volume=0, and the read returns ErrPriceWithheld before the
// staleness expression is evaluated (that gate is tested in
// internal/pricingguard/substance_test.go).
//
// LatestPrice itself is not unit-testable here: StorePriceReader.S is a concrete
// *timescale.Store with no injectable constructor, so the end-to-end read needs the
// testcontainers harness. These tests pin the two seams the rule parameterises, the default
// window and the clock, so the constant cannot be silently changed and the nil-fallbacks cannot
// rot.

// TestStorePriceReaderFreshnessDefault pins the freshness window.
//
// 15 minutes sits between two failure modes: well above the 1-2 minute closed-bucket floor, so an
// active pair is never falsely marked stale, and decisive on a dormant one, which would
// otherwise serve a months-old VWAP with stale=false.
func TestStorePriceReaderFreshnessDefault(t *testing.T) {
	t.Parallel()

	var zero StorePriceReader
	if got := zero.freshnessWindow(); got != defaultVWAPFreshness {
		t.Errorf("zero-value freshnessWindow() = %v, want the default %v", got, defaultVWAPFreshness)
	}
	if defaultVWAPFreshness != 15*time.Minute {
		t.Errorf("defaultVWAPFreshness = %v, want 15m — see the CS-017 rationale above "+
			"before changing it: too low falsely marks active pairs stale, too high "+
			"re-opens the 200-day-old-bucket bug", defaultVWAPFreshness)
	}
	// A configured window must win, or the seam is decorative.
	r := StorePriceReader{VWAPFreshness: 90 * time.Second}
	if got := r.freshnessWindow(); got != 90*time.Second {
		t.Errorf("freshnessWindow() = %v, want the configured 90s", got)
	}
	// Zero means "unset" — the documented sentinel — not "never stale".
	r = StorePriceReader{VWAPFreshness: 0}
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

	var zero StorePriceReader
	before := time.Now()
	got := zero.clock()
	if got.Before(before) || got.After(time.Now()) {
		t.Errorf("zero-value clock() = %v, want a time.Now() reading between %v and now",
			got, before)
	}

	fixed := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	r := StorePriceReader{Now: func() time.Time { return fixed }}
	if c := r.clock(); !c.Equal(fixed) {
		t.Errorf("clock() = %v, want the injected %v", c, fixed)
	}
}

// TestStorePriceReaderStalenessBoundary states the rule the two seams
// exist to make testable, at its boundary.
//
// The rule has two details that are easy to get wrong:
//
//		stale := lowConfidence || r.clock().Sub(bucket.Add(time.Minute)) > r.freshnessWindow()
//
//	  - the age is measured from the bucket's close (start + 1 minute), not its start; a 1-minute
//	    CAGG bucket is not closed until its minute elapses, so measuring from the start would
//	    report every bucket a minute older than it is and shift the boundary;
//	  - `lowConfidence` short-circuits, so a low-confidence read is stale regardless of age.
//
// This pins the arithmetic and the boundary; the integration suite owns the surrounding read.
func TestStorePriceReaderStalenessBoundary(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	r := StorePriceReader{Now: func() time.Time { return now }}

	// Calls the real rule. Re-implementing the expression locally would certify nothing: deleting
	// the `> r.freshnessWindow()` term from LatestPrice would leave the test green while
	// /v1/price served months-old buckets with stale=false. The rule lives in
	// StorePriceReader.bucketIsStale so a unit test can reach it; LatestPrice calls the same method.
	stale := r.bucketIsStale

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

// A guard-substituted bucket is served stale even inside the freshness
// window: the response holds an older value in place of the current
// minute, and without the flag it reads the same as a quiet market.
func TestStorePriceReaderGuardedSnapshotFlagsSubstitutionStale(t *testing.T) {
	t.Parallel()

	base, quote := canonical.NativeAsset(), mustFiatUSD(t)
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	var store headlineVWAPStore
	for i := 3; i < 16; i++ {
		store.trailing = append(store.trailing, headlineRow(i, "1.0"))
	}
	now := headlineRow(0, "1.0").Bucket.Add(time.Minute)
	r := StorePriceReader{Now: func() time.Time { return now }}

	for _, tc := range []struct {
		name            string
		candidate       string
		wantPrice       string
		wantStale       bool
		wantSubstituted bool
	}{
		{"validated bucket serves fresh", "1.0", "1.0", false, false},
		{"outlier serves last-known-good as stale", "100.0", "1.0", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			served, lowConfidence, substituted := pricingguard.GuardServedVWAP1mConfidence(
				context.Background(), store, nil, pair, headlineRow(0, tc.candidate))
			snap, stale := r.guardedSnapshot(base, quote, served, lowConfidence, substituted)
			if snap.Price != tc.wantPrice {
				t.Errorf("price = %s, want %s", snap.Price, tc.wantPrice)
			}
			if snap.Substituted != tc.wantSubstituted {
				t.Errorf("Substituted = %v, want %v", snap.Substituted, tc.wantSubstituted)
			}
			if stale != tc.wantStale {
				t.Errorf("stale = %v, want %v", stale, tc.wantStale)
			}
		})
	}
}
