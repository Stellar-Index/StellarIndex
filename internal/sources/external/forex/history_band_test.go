package forex

import (
	"context"
	"io"
	"testing"
	"time"
)

// snapshotWithHistory builds a Snapshot whose Currencies establish today's
// baseline AND whose History7d carries the given trailing-day points per
// ticker. The current-rate loop runs first in persistSnapshot, so it seeds
// the per-ticker band baseline the history points are measured against.
func snapshotWithHistory(current map[string]float64, history map[string][]HistoryPoint) *Snapshot {
	s := snapshotOf(current)
	s.History7d = history
	return s
}

// TestPersistSnapshot_HistoryRowRejectsBadBar is the MR-1 proven-red guard
// (audit-2026-08-14). Regression (1): the trailing-7d history rows were
// written with ONLY a >0/finite filter — no [maxRateDeviation] band — so a
// transiently-wrong-but-positive upstream bar for a PAST date (provider
// glitch: EUR 0.85 instead of 0.92 two days ago, but here a decimal shift
// to 9.2 to sit unambiguously outside the 50% band) overwrote the correct
// stored rate in place. fx_quotes.rate_usd is the denominator of every
// fiat-quoted usd_volume, so that one bad history bar re-scales every
// EUR-quoted trade valued off that date, with nothing to revert it.
//
// After the fix persistSnapshot bands each history point via
// acceptHistoryRate against the ticker's current accepted rate: a bar >50%
// off is dropped, an in-band bar is written.
//
// Reverting the persistSnapshot history loop to the old
// `<=0 || IsNaN || IsInf` filter turns the "bad bar dropped" assertion red.
func TestPersistSnapshot_HistoryRowRejectsBadBar(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	twoDaysAgo := today.Add(-2 * 24 * time.Hour)
	threeDaysAgo := today.Add(-3 * 24 * time.Hour)

	snap := snapshotWithHistory(
		map[string]float64{"EUR": 0.92}, // today's baseline
		map[string][]HistoryPoint{
			"EUR": {
				{Date: threeDaysAgo, RateUSD: 0.93}, // in band → written
				{Date: twoDaysAgo, RateUSD: 9.2},    // decimal-shift glitch → must be dropped
			},
		},
	)
	w.persistSnapshot(ctx, snap)

	if len(cw.batches) != 1 {
		t.Fatalf("expected 1 persisted batch, got %d", len(cw.batches))
	}
	batch := cw.batches[0]

	// The good in-band history bar is written.
	if !hasHistoryRow(batch, "EUR", threeDaysAgo, 0.93) {
		t.Errorf("in-band history bar EUR@%s=0.93 was not persisted; batch=%v",
			threeDaysAgo.Format("2006-01-02"), batch)
	}
	// The decimal-shift history bar (9.2, ~10x the 0.92 baseline, far outside
	// the 50%% band) must be REJECTED — otherwise it overwrites the correct
	// stored 0.92 for that date and mis-scales every EUR-quoted usd_volume.
	if hasHistoryRow(batch, "EUR", twoDaysAgo, 9.2) {
		t.Errorf("bad history bar EUR@%s=9.2 was persisted, want it REJECTED "+
			"(baseline 0.92, band %v) — an unbanded past bar re-scales every "+
			"EUR-quoted usd_volume for that date (MR-1)",
			twoDaysAgo.Format("2006-01-02"), maxRateDeviation)
	}
}

// TestPersistSnapshot_HistoryRowNoBaselineBootstraps — with no current-rate
// baseline for the ticker (e.g. the ticker appears only in History7d), the
// history band must not refuse to bootstrap, mirroring acceptRate: dropping
// legitimate history would starve the feed.
func TestPersistSnapshot_HistoryRowNoBaselineBootstraps(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	yesterday := today.Add(-24 * time.Hour)

	// No current rate for MXN — only a history point.
	snap := snapshotWithHistory(
		map[string]float64{},
		map[string][]HistoryPoint{
			"MXN": {{Date: yesterday, RateUSD: 18.5}},
		},
	)
	w.persistSnapshot(ctx, snap)

	if len(cw.batches) != 1 {
		t.Fatalf("expected 1 persisted batch, got %d", len(cw.batches))
	}
	if !hasHistoryRow(cw.batches[0], "MXN", yesterday, 18.5) {
		t.Errorf("first-sighting history bar MXN@%s=18.5 was dropped, want it "+
			"bootstrapped — no baseline must accept, not block",
			yesterday.Format("2006-01-02"))
	}
}

// hasHistoryRow reports whether batch carries a row for ticker at the given
// date (truncated to the day, as persistSnapshot stores it) with rate.
func hasHistoryRow(batch []FXQuote, ticker string, date time.Time, rate float64) bool {
	want := date.UTC().Truncate(24 * time.Hour)
	for _, q := range batch {
		if q.Ticker == ticker && q.Bucket.Equal(want) && q.RateUSD == rate {
			return true
		}
	}
	return false
}
