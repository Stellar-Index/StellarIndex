// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"math/big"
	"testing"
	"time"
)

// The two smallest-unit scales the fiat constituent set actually spans
// (CS-040): an on-chain DEX leg stamps 7-decimal stroops, a CEX leg 8.
const (
	onChainScale = 7
	cexScale     = 8
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
// finalizeCombined renders the bucket and cross-checks that it derived
// the lift target the caller expected. `scales` lists the scale of every
// bar handed to acc.add, in the same order; the bucket takes their
// maximum itself, so a mismatch here means the accumulator stopped
// tracking its own scale rather than that the expectation is stale.
func finalizeCombined(t *testing.T, acc *ohlcBucketAcc, ts time.Time, scales ...int) OHLCSeriesBar {
	t.Helper()
	want := ohlcBarScaleUnknown
	for _, sc := range scales {
		if sc > want {
			want = sc
		}
	}
	if acc.commonScale != want {
		t.Fatalf("bucket lift target = %d, want %d (the maximum of %v)", acc.commonScale, want, scales)
	}
	return acc.finalize(ts)
}

func TestCombinedBarServesTrueExtremes(t *testing.T) {
	acc := newOHLCBucketAcc()
	// A healthy CEX-shaped constituent: vwap ~0.178, high 0.1792.
	acc.add(&OHLCSeriesBar{
		O: "0.1789", H: "0.1792", L: "0.1776", C: "0.1777",
		VBase: "1000000", VQuote: "178000", N: 2099,
	}, onChainScale)
	// A thin SDEX constituent whose high is a REAL 1.00-USDC-per-XLM fill —
	// it cleared the $0.01 notional floor in the CAGG, so it is a market
	// event that happened and must be served. Bucket vwap ≈ 0.178, so the
	// removed 2× band (ceiling ≈ 0.356) would have dropped it.
	acc.add(&OHLCSeriesBar{
		O: "0.1790", H: "1.0000000000", L: "0.0500000000", C: "0.1780",
		VBase: "500000", VQuote: "89100", N: 3177,
	}, onChainScale)
	// Both constituents are at one scale here, so every lift factor is
	// 10^0 = 1 and this stays a test of the extremes rule alone.
	bar := finalizeCombined(t, acc, time.Unix(1_750_000_000, 0).UTC(), onChainScale, onChainScale)

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
	}, onChainScale)
	bar := finalizeCombined(t, acc, time.Unix(1_750_000_000, 0).UTC(), onChainScale)

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

// TestCombinedBarScaleDrivesVolumeWeighting pins the CS-040 series fix at
// the accumulator: the SAME two constituent bars must combine to different
// (and each correct) numbers depending on the scale their venues declare.
//
// One 1000-unit leg at price 0.10 and one 1000-unit leg at 0.12:
//
//   - declared 7dp and 8dp, the 7dp leg is really 1000 units and the 8dp
//     leg is really 1000 units, so the bar is 2000 units at the common 8dp
//     scale and the volume-weighted price is 0.11;
//   - declared BOTH 8dp, the first leg really is a tenth of the second, so
//     1.1e11 units and 0.1181818181 is the right answer and the lift must
//     not touch it.
//
// The second case is the byte-identical guard [aggregate.NormalizeAmountScale]
// makes for uniform windows, restated for bars: a response whose bars share
// one scale must come out exactly as it did before any of this existed.
// Together they show the served numbers move BECAUSE of the declared scale
// and for no other reason.
func TestCombinedBarScaleDrivesVolumeWeighting(t *testing.T) {
	pow10 := func(n int) string {
		s := "1"
		for i := 0; i < n; i++ {
			s += "0"
		}
		return s
	}
	build := func() *ohlcBucketAcc {
		acc := newOHLCBucketAcc()
		acc.add(&OHLCSeriesBar{ // 1000 units @ 0.10
			O: "0.10", H: "0.10", L: "0.10", C: "0.10",
			VBase: pow10(10), VQuote: pow10(9), N: 1,
		}, onChainScale)
		acc.add(&OHLCSeriesBar{ // 1000 units @ 0.12
			O: "0.12", H: "0.12", L: "0.12", C: "0.12",
			VBase: pow10(11), VQuote: "12" + pow10(9), N: 1,
		}, cexScale)
		return acc
	}
	ts := time.Unix(1_750_000_000, 0).UTC()

	mixed := finalizeCombined(t, build(), ts, onChainScale, cexScale)
	if mixed.VBase != "200000000000" {
		t.Errorf("cross-scale v_base = %q, want 200000000000 (2000 units at the "+
			"common 8dp scale); 110000000000 is the raw cross-scale sum, which "+
			"counts the 7dp leg at a tenth of the volume it traded", mixed.VBase)
	}
	if mixed.O != "0.1100000000" || mixed.C != "0.1100000000" {
		t.Errorf("cross-scale open/close = %q/%q, want 0.1100000000 — equal real "+
			"volume at 0.10 and 0.12; 0.1181818181 is the 10x-over-weighted value",
			mixed.O, mixed.C)
	}

	// Same bars, both venues declaring 8dp: nothing may be lifted.
	accU := newOHLCBucketAcc()
	accU.add(&OHLCSeriesBar{
		O: "0.10", H: "0.10", L: "0.10", C: "0.10",
		VBase: pow10(10), VQuote: pow10(9), N: 1,
	}, cexScale)
	accU.add(&OHLCSeriesBar{
		O: "0.12", H: "0.12", L: "0.12", C: "0.12",
		VBase: pow10(11), VQuote: "12" + pow10(9), N: 1,
	}, cexScale)
	uniform := finalizeCombined(t, accU, ts, cexScale, cexScale)
	if uniform.VBase != "110000000000" {
		t.Errorf("uniform v_base = %q, want 110000000000 unchanged — a single-scale "+
			"response must be byte-identical to the pre-lift combine", uniform.VBase)
	}
	if uniform.O != "0.1181818181" {
		t.Errorf("uniform open = %q, want 0.1181818181 — with both legs genuinely at "+
			"8dp the second leg really is 10x the first and must stay weighted so",
			uniform.O)
	}

	// Extremes and the count are scale-invariant either way.
	for _, tc := range []struct{ name, got, want string }{
		{"mixed high", mixed.H, "0.1200000000"},
		{"mixed low", mixed.L, "0.1000000000"},
		{"uniform high", uniform.H, "0.1200000000"},
		{"uniform low", uniform.L, "0.1000000000"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q — a lift multiplies both legs of a bar and "+
				"cannot move a price", tc.name, tc.got, tc.want)
		}
	}
	if mixed.N != 2 || uniform.N != 2 {
		t.Errorf("trade counts = %d/%d, want 2/2", mixed.N, uniform.N)
	}
}

// TestCombinedBarUnknownScaleIsNeverLifted pins the deliberate posture for a
// bar whose reader reported no sources: it is left exactly where it is.
// [ohlcBarScaleUnknown] must not be silently read as the registry fallback
// of 8, which would lift every OTHER bar against it and manufacture the
// mis-weight this file removes.
func TestCombinedBarUnknownScaleIsNeverLifted(t *testing.T) {
	if got := barScaleDecimals(nil); got != ohlcBarScaleUnknown {
		t.Errorf("barScaleDecimals(nil) = %d, want %d — a bar that names no venue "+
			"has no resolvable scale and must not be given a plausible one",
			got, ohlcBarScaleUnknown)
	}
	if got := barScaleDecimals([]string{"sdex"}); got != onChainScale {
		t.Errorf("barScaleDecimals([sdex]) = %d, want %d", got, onChainScale)
	}
	if got := barScaleDecimals([]string{"sdex", "binance"}); got != cexScale {
		t.Errorf("barScaleDecimals([sdex binance]) = %d, want %d — a bar that mixes "+
			"scales internally takes the finest, the lift that cannot inflate its "+
			"own weight against its peers", got, cexScale)
	}

	// The unknown-scale bar sits in the SAME bucket as an 8dp one, which
	// is the only place it can now meet it: the lift target is the
	// bucket's own maximum, so a bar in another bucket cannot reach it.
	acc := newOHLCBucketAcc()
	acc.add(&OHLCSeriesBar{
		O: "0.10", H: "0.10", L: "0.10", C: "0.10",
		VBase: "1000", VQuote: "100", N: 1,
	}, ohlcBarScaleUnknown)
	acc.add(&OHLCSeriesBar{
		O: "0.10", H: "0.10", L: "0.10", C: "0.10",
		VBase: "500", VQuote: "50", N: 1,
	}, cexScale)
	bar := finalizeCombined(t, acc, time.Unix(1_750_000_000, 0).UTC(), ohlcBarScaleUnknown, cexScale)
	if bar.VBase != "1500" {
		t.Errorf("v_base = %q, want 1500 — the unknown-scale bar contributes its raw "+
			"1000 beside the 8dp bar's 500. Reading the unknown as the registry "+
			"fallback of 7 and lifting it would serve 10500", bar.VBase)
	}
}
