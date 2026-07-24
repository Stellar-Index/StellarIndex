// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"math/big"
	"testing"
	"time"
)

// TestCombinedBarServesTrueExtremes pins the post-B11-F1 contract of the
// combined fiat series: the bucket high/low are the plain max/min of the
// constituent extremes, with NO price-distance filtering.
//
// This REPLACES TestCombinedBarDropsFatFinger, which pinned the 2×-VWAP band
// (`combinedOutlierBandRatio`). That band was removed by operator decision
// (2026-07-22, docs/operations/finding-dust-trades-set-chart-extremes.md
// "DECISION"): filter on trade SIZE, never on price divergence. The wicks the
// band existed for were all dust, and dust is now excluded at the individual
// TRADE level by the $0.01 notional floor on the CAGG extremes (migration
// 0115) — which the band, comparing whole-constituent extremes, could never
// reach. Proof that the dust really is gone is DB-backed:
// test/integration/ohlc_dust_floor_test.go.
//
// The band was ALSO a correctness hazard in the other direction: a
// genuine large trade far from VWAP is a real market event and suppressing it
// is editing reality. That is what this test asserts.
func TestCombinedBarServesTrueExtremes(t *testing.T) {
	acc := newOHLCBucketAcc()
	// A healthy CEX-shaped constituent: vwap ~0.178, high 0.1792.
	acc.add(&OHLCSeriesBar{
		O: "0.1789", H: "0.1792", L: "0.1776", C: "0.1777",
		VBase: "1000000", VQuote: "178000", N: 2099,
	})
	// A thin SDEX constituent whose high is a REAL 1.00-USDC-per-XLM fill —
	// it cleared the $0.01 notional floor in the CAGG, so it is a market
	// event that happened and must be served. Bucket vwap ≈ 0.178, so the
	// removed 2× band (ceiling ≈ 0.356) would have dropped it.
	acc.add(&OHLCSeriesBar{
		O: "0.1790", H: "1.0000000000", L: "0.0500000000", C: "0.1780",
		VBase: "500000", VQuote: "89100", N: 3177,
	})
	bar := acc.finalize(time.Unix(1_750_000_000, 0).UTC())

	high, ok := new(big.Rat).SetString(bar.H)
	if !ok {
		t.Fatalf("unparseable high %q", bar.H)
	}
	if high.Cmp(big.NewRat(1, 1)) != 0 {
		t.Errorf("combined high = %s, want 1.0000000000 — an above-floor print far "+
			"from VWAP is a real market event and must NOT be suppressed "+
			"(the 2× band was removed; dust is filtered in the CAGG)", bar.H)
	}
	low, ok := new(big.Rat).SetString(bar.L)
	if !ok {
		t.Fatalf("unparseable low %q", bar.L)
	}
	if low.Cmp(big.NewRat(5, 100)) != 0 {
		t.Errorf("combined low = %s, want 0.0500000000 (min across constituents)", bar.L)
	}
	// Volume-weighted open/close and the summed volumes are untouched by the
	// extremes change.
	if bar.N != 2099+3177 {
		t.Errorf("combined trade count = %d, want %d", bar.N, 2099+3177)
	}
}

// TestCombinedBarSingleConstituentExtremes is the deep-history shape: one
// constituent per bucket (USDC back to 2021), where the combined bar must be
// that constituent's bar exactly.
func TestCombinedBarSingleConstituentExtremes(t *testing.T) {
	acc := newOHLCBucketAcc()
	acc.add(&OHLCSeriesBar{
		O: "0.1822", H: "0.1848", L: "0.1822", C: "0.1834",
		VBase: "1000000", VQuote: "183000", N: 321,
	})
	bar := acc.finalize(time.Unix(1_750_000_000, 0).UTC())

	for _, tc := range []struct{ name, got, want string }{
		{"high", bar.H, "0.1848"},
		{"low", bar.L, "0.1822"},
	} {
		got, ok := new(big.Rat).SetString(tc.got)
		if !ok {
			t.Fatalf("unparseable %s %q", tc.name, tc.got)
		}
		want, _ := new(big.Rat).SetString(tc.want)
		if got.Cmp(want) != 0 {
			t.Errorf("%s = %s, want %s", tc.name, tc.got, tc.want)
		}
	}
}
