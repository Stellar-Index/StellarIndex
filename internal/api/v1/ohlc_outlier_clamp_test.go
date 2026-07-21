// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"math/big"
	"testing"
	"time"
)

// TestCombinedBarDropsFatFinger pins S-012 + the 2026-07 XLM/USD wick: the
// USD-peg expansion folds thin SDEX books into the combined fiat series, and a
// fat-finger print (1.00 USDC/XLM vs a ~0.178 market) must be DROPPED from the
// served high — not clamped to a 3× ceiling (which still served a visible ~0.53
// wick on a ~0.178 pair). The served high is the real market high (~0.1792).
func TestCombinedBarDropsFatFinger(t *testing.T) {
	acc := newOHLCBucketAcc()
	// A healthy CEX-shaped constituent: vwap ~0.178, real high 0.1792.
	acc.add(&OHLCSeriesBar{
		O: "0.1789", H: "0.1792", L: "0.1776", C: "0.1777",
		VBase: "1000000", VQuote: "178000", N: 2099,
	})
	// The thin SDEX constituent carrying the 1.00 print in its high.
	acc.add(&OHLCSeriesBar{
		O: "0.1790", H: "1.0000000000", L: "0.1764", C: "0.1780",
		VBase: "500000", VQuote: "89100", N: 3177,
	})
	bar := acc.finalize(time.Unix(1_750_000_000, 0).UTC())

	high, ok := new(big.Rat).SetString(bar.H)
	if !ok {
		t.Fatalf("unparseable high %q", bar.H)
	}
	// The high must be the real market high (~0.1792), NOT the 1.00 print and
	// NOT a 3× clamp ceiling (~0.53). Bucket vwap ≈ 0.178, band 2× → ceil ≈
	// 0.356; the 1.00 print is dropped, leaving the healthy 0.1792.
	if high.Cmp(big.NewRat(20, 100)) >= 0 {
		t.Fatalf("combined high %s — fat-finger not dropped (expected ~0.1792)", bar.H)
	}
	if high.Cmp(big.NewRat(178, 1000)) < 0 {
		t.Fatalf("combined high %s — real market high lost", bar.H)
	}
	// The true (non-outlier) low must be preserved.
	low, _ := new(big.Rat).SetString(bar.L)
	if low.Cmp(big.NewRat(17, 100)) < 0 {
		t.Fatalf("low %s over-dropped", bar.L)
	}
}
