// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The two smallest-unit scales the fiat constituent set actually spans
// — an on-chain DEX leg stamps 7-decimal stroops, a CEX leg 8.
const (
	onChainScale = 7
	cexScale     = 8
)

// TestCombinedBarServesTrueExtremes: combined bucket high/low are plain max/min of constituents, with NO price-distance filter.
// Dust is excluded per TRADE by the CAGG notional floor; a genuine large off-VWAP trade is real and must not be suppressed.
// finalizeCombined cross-checks the lift target; `scales` lists each bar's scale in acc.add order.
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
	// A thin SDEX constituent whose high is a real 1.00 USDC/XLM fill cleared the $0.01 notional floor, so it must be served;
	// bucket vwap is about 0.178, so the removed 2x band (ceiling about 0.356) would have dropped it.
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

// TestCombinedBarScaleDrivesVolumeWeighting: the same two bars (1000 units at 0.10 and 0.12) combine to 2000 units at 0.11 when
// declared 7dp+8dp, but to 1.1e11 units at 0.1181818181 when both 8dp. Equal-scale bars must come out unchanged (the
// aggregate.NormalizeAmountScale guarantee), so served numbers move only with the declared scale.
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

// TestCombinedBarUnknownScaleIsNeverLifted: a bar whose reader reported no sources is left
// exactly where it is. [ohlcBarScaleUnknown] must not be read as the registry fallback of 8,
// which would lift every OTHER bar against it and manufacture a mis-weight.
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
	// A source absent from external.Registry must not get the registry's CEX-flavoured 8-decimal fallback, the same defect as
	// the point-path commonAmountScaleDecimals, via the shared amountScaleDecimalsFor resolver.
	if got := barScaleDecimals([]string{"brand_new_dex"}); got != ohlcBarScaleUnknown {
		t.Errorf("barScaleDecimals([brand_new_dex]) = %d, want %d — \"brand_new_dex\" has "+
			"no registry entry, so its scale is unknown, not the registry's 8dp default",
			got, ohlcBarScaleUnknown)
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

// TestCombinedBarStatesScaleOnWire: a combined bucket must state the smallest-unit scale its v_base/v_quote were lifted
// to. A zero decimals value is indistinguishable on the wire from a real 0dp scale and wrong for 7dp/8dp buckets.
func TestCombinedBarStatesScaleOnWire(t *testing.T) {
	acc := newOHLCBucketAcc()
	acc.add(&OHLCSeriesBar{
		O: "0.10", H: "0.10", L: "0.10", C: "0.10",
		VBase: "10000000", VQuote: "1000000", N: 1,
	}, onChainScale)
	acc.add(&OHLCSeriesBar{
		O: "0.12", H: "0.12", L: "0.12", C: "0.12",
		VBase: "100000000", VQuote: "12000000", N: 1,
	}, cexScale)

	bar := finalizeCombined(t, acc, time.Unix(1_750_000_000, 0).UTC(), onChainScale, cexScale)
	if bar.VBaseDecimals == nil || *bar.VBaseDecimals != cexScale {
		t.Errorf("VBaseDecimals = %v, want %d (the bucket's lift target, "+
			"cexScale) — a consumer dividing by 10^VBaseDecimals must land on "+
			"the actual scale of the served v_base integer", derefScale(bar.VBaseDecimals), cexScale)
	}
	if bar.VQuoteDecimals == nil || *bar.VQuoteDecimals != cexScale {
		t.Errorf("VQuoteDecimals = %v, want %d", derefScale(bar.VQuoteDecimals), cexScale)
	}
}

// TestCombinedBarWithUnknownScaleStatesNone: a bucket with an unknown-scale bar sums unlifted and lifted integers, so no
// single scale fits; it must state null, never the known bars' lift target or the -1 sentinel.
func TestCombinedBarWithUnknownScaleStatesNone(t *testing.T) {
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
	if bar.VBaseDecimals != nil || bar.VQuoteDecimals != nil {
		t.Errorf("decimals = %v/%v, want null/null — the bucket mixes an "+
			"unknown-scale sum with an 8dp one", derefScale(bar.VBaseDecimals),
			derefScale(bar.VQuoteDecimals))
	}
}

// derefScale renders a nullable wire scale for a failure message.
func derefScale(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestSortTradesChronological_NeutralToVenueName pins that on the fiat point path the merged window's order, which picks
// open/close, follows tx_hash within a ledger close, not venue-name rank; swapping names moves nothing.
func TestSortTradesChronological_NeutralToVenueName(t *testing.T) {
	ts := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	want := []string{"aa", "bb", "cc"}
	for _, names := range [][2]string{{"aquarius", "soroswap"}, {"soroswap", "aquarius"}} {
		trades := []canonical.Trade{
			{Source: names[0], Ledger: 900, TxHash: "cc", Timestamp: ts},
			{Source: names[0], Ledger: 900, TxHash: "bb", Timestamp: ts},
			{Source: names[1], Ledger: 900, TxHash: "aa", Timestamp: ts},
		}
		sortTradesChronological(trades)
		for k := range want {
			if trades[k].TxHash != want[k] {
				t.Fatalf("venues %v: position %d = %q (%s), want %q — the open of a "+
					"same-ledger window must not go to the alphabetically-first venue",
					names, k, trades[k].TxHash, trades[k].Source, want[k])
			}
		}
	}
}
