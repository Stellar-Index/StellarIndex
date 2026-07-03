// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"math/big"
	"testing"
	"time"
)

// TestCombinedBarClampsFatFinger pins S-012: the USD-peg expansion
// folds thin SDEX books into the combined fiat series, and one
// fat-finger print (1.00 USDC/XLM vs a ~0.178 market) must not become
// the served high of the flagship pair.
func TestCombinedBarClampsFatFinger(t *testing.T) {
	acc := newOHLCBucketAcc()
	// A healthy CEX-shaped constituent: vwap ~0.178.
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
	ceiling := big.NewRat(6, 10) // 3× a ~0.178 vwap ≈ 0.534 < 0.6
	if high.Cmp(ceiling) >= 0 {
		t.Fatalf("combined high %s not clamped — the 1.00 fat-finger survived", bar.H)
	}
	// The true (non-outlier) extremes must be preserved.
	low, _ := new(big.Rat).SetString(bar.L)
	if low.Cmp(big.NewRat(17, 100)) < 0 {
		t.Fatalf("low %s over-clamped", bar.L)
	}
}
