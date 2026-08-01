// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"math/big"
	"strings"
	"testing"
	"time"
)

// Regression suite for audit finding M-B (MNY-06 on the TWAP surface):
// the served-TWAP direction combine.
//
// SDEX stores the same market in BOTH orientations, so TWAPPointsInRange
// has to fold the two stored directions into the one the caller asked
// for. Pre-fix it did so with a TRADE-COUNT-weighted mean of
// {twap, 1/twap_flipped} (a `1.0/twap` SQL inversion, then
// `SUM(twap·trade_count)/SUM(trade_count)`). But the twap_1h / twap_1d
// CAGGs (migration 0081) define twap = avg(prices_1m.twap) — EQUAL PER
// ELAPSED MINUTE, deliberately NOT per trade. The only correct merge
// weight is therefore each direction's minute COVERAGE (how many prices_1m
// minute-buckets it contributed = migration 0126's `sample_count`), NOT
// its trade count.
//
// The canonical fixture is one bucket of a two-sided XLM/USDC market whose
// three candidate answers are far apart, so the fix must land on exactly
// the coverage-weighted one:
//
//	direction A (flipped USDC/XLM): stored twap 2 → oriented 0.5,
//	    minute coverage 50, trade count 500
//	direction B (XLM/USDC):         stored twap 0.6 → oriented 0.6,
//	    minute coverage  5, trade count 5000
//
//	coverage-weighted (CORRECT): (0.5·50 + 0.6·5)/(50+5) = 28/55 = 0.50909…
//	trade-count-weighted (pre-fix bug): (0.5·500 + 0.6·5000)/5500 = 0.59090…
//	equal-weighted (the regression 0081 also rejects): (0.5+0.6)/2 = 0.55
//
// Coverage disagrees with BOTH by ~16% / ~8%, so a regression to either is
// a loud failure, not a rounding wobble. The flipped side stores exactly
// 2, whose reciprocal 0.5 is exact, so the whole combine is exact rational
// arithmetic (ADR-0003) with no `1.0/twap` rounding anywhere.

var (
	// twapCoverageWeighted is the correct answer, 28/55.
	twapCoverageWeighted = big.NewRat(28, 55)
	// twapCountWeighted is the pre-fix trade-count answer, 13/22.
	twapCountWeighted = big.NewRat(13, 22)
	// twapEqualWeighted is the equal-weighted answer, 11/20.
	twapEqualWeighted = big.NewRat(11, 20)
)

// twapCombineFixture is the two-sided bucket above, as combineDirTWAP sees
// it — the trade counts are DELIBERATELY absent from dirTWAP: the fold must
// not be able to reach for them.
var twapCombineFixture = []dirTWAP{
	{twapText: "2", sampleCount: 50, flipped: true},
	{twapText: "0.6", sampleCount: 5, flipped: false},
}

// ratNear reports whether the decimal text `got` parses to within `tol` of
// `want`. The combined value renders at the package's relative precision
// (combinedVWAPSigDigits), so an exact-string compare would be fragile;
// this pins the value while tolerating the final rendering digit.
func ratNear(t *testing.T, got string, want *big.Rat, tol *big.Rat) bool {
	t.Helper()
	r, ok := new(big.Rat).SetString(got)
	if !ok {
		t.Fatalf("combined TWAP %q is not numeric", got)
	}
	diff := new(big.Rat).Sub(r, want)
	diff.Abs(diff)
	return diff.Cmp(tol) <= 0
}

// TestCombineDirTWAP_CoverageWeighted pins the exact combined value and
// proves it is NEITHER the trade-count-weighted mean (the pre-fix bug) NOR
// the equal-weighted mean (the regression migration 0081 also rejects).
func TestCombineDirTWAP_CoverageWeighted(t *testing.T) {
	got, ok := combineDirTWAP(twapCombineFixture)
	if !ok {
		t.Fatal("combineDirTWAP returned ok=false for a well-formed two-sided bucket")
	}
	// tol tight enough that 0.50909 can never be confused with 0.59090 or
	// 0.55, but loose enough for the 25-sig-digit rendering.
	tol := big.NewRat(1, 1_000_000_000)
	if !ratNear(t, got, twapCoverageWeighted, tol) {
		t.Errorf("combined TWAP = %s, want coverage-weighted 28/55 = 0.50909…", got)
	}
	if ratNear(t, got, twapCountWeighted, big.NewRat(1, 100)) {
		t.Fatalf("combined TWAP = %s — that is the TRADE-COUNT-weighted mean (0.59090…), "+
			"the M-B defect; the fold must weight by minute COVERAGE (sample_count)", got)
	}
	if ratNear(t, got, twapEqualWeighted, big.NewRat(1, 100)) {
		t.Fatalf("combined TWAP = %s — that is the EQUAL-weighted mean (0.55), which "+
			"regresses the well-covered case; the fold must weight by sample_count", got)
	}
}

// TestCombineDirTWAP_RowOrderIndependent — the merge is a ratio of sums, so
// the direction order must not change the answer.
func TestCombineDirTWAP_RowOrderIndependent(t *testing.T) {
	swapped := []dirTWAP{twapCombineFixture[1], twapCombineFixture[0]}
	got, ok := combineDirTWAP(swapped)
	if !ok {
		t.Fatal("combineDirTWAP returned ok=false for the row-swapped fixture")
	}
	if !ratNear(t, got, twapCoverageWeighted, big.NewRat(1, 1_000_000_000)) {
		t.Errorf("row-swapped combined TWAP = %s, want 28/55 = 0.50909…", got)
	}
}

// TestCombineDirTWAP_SingleDirectionIsVerbatim — a bucket that traded only
// in the requested orientation IS its stored TWAP, and the Postgres NUMERIC
// text must pass through untouched so the served bytes don't shift for the
// common one-sided pair.
func TestCombineDirTWAP_SingleDirectionIsVerbatim(t *testing.T) {
	const stored = "0.1234567890123456789012345678901"
	got, ok := combineDirTWAP([]dirTWAP{{twapText: stored, sampleCount: 42, flipped: false}})
	if !ok {
		t.Fatal("combineDirTWAP returned ok=false for a single unflipped row")
	}
	if got != stored {
		t.Errorf("combineDirTWAP reformatted a single-direction TWAP: got %s, want %s verbatim", got, stored)
	}
}

// TestCombineDirTWAP_SingleFlippedIsExactInverse — a bucket that traded
// only in the REVERSE orientation must be served at the EXACT reciprocal of
// the stored price (big.Rat 1/twap, not a rounded SQL division).
func TestCombineDirTWAP_SingleFlippedIsExactInverse(t *testing.T) {
	got, ok := combineDirTWAP([]dirTWAP{{twapText: "4", sampleCount: 10, flipped: true}})
	if !ok {
		t.Fatal("combineDirTWAP returned ok=false for a single flipped row")
	}
	if got != "0.25" {
		t.Errorf("single-flipped TWAP = %s, want 0.25 (exact inverse of the stored 4)", got)
	}
}

// TestCombineDirTWAP_ZeroCoverageIgnored — a direction with no minute
// coverage carries no weight and must not drag the answer nor blow it up.
func TestCombineDirTWAP_ZeroCoverageIgnored(t *testing.T) {
	got, ok := combineDirTWAP([]dirTWAP{
		{twapText: "0.6", sampleCount: 5, flipped: false},
		{twapText: "2", sampleCount: 0, flipped: true},
	})
	if !ok {
		t.Fatal("combineDirTWAP returned ok=false")
	}
	if got != "0.6" {
		t.Errorf("with the flipped side at zero coverage, TWAP = %s, want the surviving 0.6", got)
	}
}

// TestCombineDirTWAP_NoUsableRows — an empty, unparseable, non-positive or
// zero-coverage-only bucket reports ok=false so callers surface "no data"
// rather than a fabricated price.
func TestCombineDirTWAP_NoUsableRows(t *testing.T) {
	cases := map[string][]dirTWAP{
		"empty":            {},
		"unparseable":      {{twapText: "NaN", sampleCount: 10, flipped: false}},
		"zero-price":       {{twapText: "0", sampleCount: 10, flipped: true}},
		"negative-price":   {{twapText: "-1", sampleCount: 10, flipped: true}},
		"zero-coverage":    {{twapText: "1", sampleCount: 0, flipped: true}},
		"all-zero-covered": {{twapText: "1", sampleCount: 0, flipped: false}, {twapText: "2", sampleCount: 0, flipped: true}},
	}
	for name, rows := range cases {
		t.Run(name, func(t *testing.T) {
			if got, ok := combineDirTWAP(rows); ok {
				t.Errorf("combineDirTWAP(%v) = (%s, true), want ok=false", rows, got)
			}
		})
	}
}

// TestTWAPPointsInRange_CoverageWeightedUnion drives the real Store method
// end-to-end over the canned driver (query text, scan, fold) and asserts
// the VALUE it serves — so a regression anywhere along the path fails, not
// just in combineDirTWAP. The fixture's per-direction trade counts (500 vs
// 5000) are the ones that produced 0.59090 under the pre-fix SQL; the read
// must now serve the coverage-weighted 0.50909 and never touch trade_count.
func TestTWAPPointsInRange_CoverageWeightedUnion(t *testing.T) {
	pair := testXLMUSDCPair(t)
	bucket := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	conn := &cannedConn{plan: []cannedResult{{
		cols: []string{"bucket", "base_asset", "twap", "sample_count", "volume_usd"},
		rows: [][]driver.Value{
			// flipped USDC/XLM: stored 2 → oriented 0.5, coverage 50
			{bucket, pair.Quote.String(), "2", int64(50), "1000.00"},
			// XLM/USDC: stored 0.6, coverage 5
			{bucket, pair.Base.String(), "0.6", int64(5), "2000.00"},
		},
	}}}
	store := &Store{db: sql.OpenDB(&cannedConnector{conn: conn})}
	defer func() { _ = store.db.Close() }()

	pts, err := store.TWAPPointsInRange(context.Background(), pair, Granularity1h, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("TWAPPointsInRange: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("TWAPPointsInRange returned %d points, want 1 (both directions fold into ONE bucket)", len(pts))
	}
	tol := big.NewRat(1, 1_000_000_000)
	if !ratNear(t, pts[0].VWAP, twapCoverageWeighted, tol) {
		t.Errorf("served TWAP = %s, want coverage-weighted 28/55 = 0.50909… "+
			"(0.59090 = trade-count-weighted, 0.55 = equal-weighted)", pts[0].VWAP)
	}
	if ratNear(t, pts[0].VWAP, twapCountWeighted, big.NewRat(1, 100)) {
		t.Fatalf("served TWAP = %s is the trade-count-weighted 0.59090 — the M-B defect", pts[0].VWAP)
	}
	if ratNear(t, pts[0].VWAP, twapEqualWeighted, big.NewRat(1, 100)) {
		t.Fatalf("served TWAP = %s is the equal-weighted 0.55 — coverage weighting is ignored", pts[0].VWAP)
	}
	if pts[0].VolumeUSD == nil || *pts[0].VolumeUSD != "3000.00" {
		t.Errorf("served volume_usd = %v, want 3000.00 (both directions summed)", pts[0].VolumeUSD)
	}
	// The read must ask for BOTH orientations and select the coverage column.
	q := conn.queries[0]
	if !strings.Contains(q, "base_asset = $1 AND quote_asset = $2") ||
		!strings.Contains(q, "base_asset = $2 AND quote_asset = $1") {
		t.Errorf("TWAPPointsInRange query reads only one stored orientation:\n%s", q)
	}
	if !strings.Contains(q, "sample_count") {
		t.Errorf("TWAPPointsInRange query does not select sample_count (migration 0126):\n%s", q)
	}
	// It must NOT fall back to trade-count weighting in SQL.
	if strings.Contains(q, "trade_count") {
		t.Errorf("TWAPPointsInRange query still references trade_count — the coverage fix is not wired:\n%s", q)
	}
}

// TestTWAPPointsInRange_FlippedOnlyBucketIsServed — a TWAP bucket that
// traded ONLY in the reverse orientation must still be served, at the exact
// inverse of the stored price (not dropped).
func TestTWAPPointsInRange_FlippedOnlyBucketIsServed(t *testing.T) {
	pair := testXLMUSDCPair(t)
	bucket := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	conn := &cannedConn{plan: []cannedResult{{
		cols: []string{"bucket", "base_asset", "twap", "sample_count", "volume_usd"},
		rows: [][]driver.Value{
			{bucket, pair.Quote.String(), "4", int64(7), "500.00"},
		},
	}}}
	store := &Store{db: sql.OpenDB(&cannedConnector{conn: conn})}
	defer func() { _ = store.db.Close() }()

	pts, err := store.TWAPPointsInRange(context.Background(), pair, Granularity1d, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("TWAPPointsInRange: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("flipped-only bucket returned %d points, want 1 (it must not be dropped)", len(pts))
	}
	if pts[0].VWAP != "0.25" {
		t.Errorf("flipped-only TWAP = %s, want 0.25 (exact inverse of the stored 4)", pts[0].VWAP)
	}
	if pts[0].VolumeUSD == nil || *pts[0].VolumeUSD != "500.00" {
		t.Errorf("flipped-only volume_usd = %v, want 500.00", pts[0].VolumeUSD)
	}
}
