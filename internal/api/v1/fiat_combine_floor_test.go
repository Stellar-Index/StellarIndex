// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// A constituent bar no trade of which cleared the $0.01 notional floor carries the CAGG's unfloored
// fallback prices. Merged beside a floored constituent it must set no price, and it must not answer
// a bucket on the held-back gate, or one crumb trade on an idle book replaces a pool's real bar.

// floorBarsReader serves fixed bars per pair, as the storage reader would after its own direction merge.
type floorBarsReader struct {
	stubHistoryReader
	bars map[string][]v1.OHLCSeriesBar
}

func (r *floorBarsReader) OHLCSeries(
	_ context.Context, pair canonical.Pair, _ string, _, _ time.Time, _ int,
) ([]v1.OHLCSeriesBar, error) {
	return r.bars[fiatParityPairKey(pair)], nil
}

func floorBar(t0 time.Time, o, h, l, c, vBase, vQuote string, n int64, belowFloor bool) v1.OHLCSeriesBar {
	return v1.OHLCSeriesBar{
		T: v1.WireTime(t0), O: o, H: h, L: l, C: c, VBase: vBase, VQuote: vQuote, N: n,
		Sources: []string{"sdex"}, BelowFloor: belowFloor,
	}
}

func fetchOneFloorBar(t *testing.T, ts *testServer, base string, t0 time.Time) v1.OHLCSeriesBar {
	t.Helper()
	resp := mustGet(t, ts.URL+"/v1/ohlc?base="+base+"&quote=fiat:USD&interval=1h&from="+
		t0.Format(time.RFC3339)+"&to="+t0.Add(time.Hour).Format(time.RFC3339))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env fiatSeriesEnvelope
	mustDecode(t, resp, &env)
	if len(env.Data.Intervals) != 1 {
		t.Fatalf("series returned %d bars, want 1", len(env.Data.Intervals))
	}
	return env.Data.Intervals[0]
}

// TestFiatSeries_NonstandardDecimalsAdjustsBeforeRendering: a 9dp token's raw ratio 0.000001234567891 is 100x that in price.
// Rendering the raw ratio first keeps five significant digits (0.0000012345) that the adjustment then scales into 0.0001234500.
func TestFiatSeries_NonstandardDecimalsAdjustsBeforeRendering(t *testing.T) {
	usdc := mustParseAsset(t, usdcClassicID)
	bookPair, _ := canonical.NewPair(mustParseAsset(t, flaggedAsset), usdc)
	t0 := fiatParityBucketStart()
	reader := &floorBarsReader{bars: map[string][]v1.OHLCSeriesBar{fiatParityPairKey(bookPair): {floorBar(t0,
		"0.000001234567891", "0.000001234567891", "0.000001234567891", "0.000001234567891", "10000000000000", "12345678", 40, false)}}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History: reader, USDPeggedClassics: []canonical.Asset{usdc}, NonstandardDecimals: nonstandardDecimalsCacheWith(t, flaggedAsset, 9),
	}))

	bar := fetchOneFloorBar(t, ts, flaggedAsset, t0)
	for name, got := range map[string]string{"o": bar.O, "h": bar.H, "l": bar.L, "c": bar.C} {
		if got != "0.0001234567" {
			t.Errorf("%s = %s, want 0.0001234567 (the exact adjusted price, rendered once)", name, got)
		}
	}
}

// TestFiatSeries_DustConstituentSetsNoWick: native/USDC trades at about 0.18; the direct native/fiat:USD
// market holds only a crumb at 7.5 in the same hour. The served high must stay the book's, not 7.5.
func TestFiatSeries_DustConstituentSetsNoWick(t *testing.T) {
	usdc := mustParseAsset(t, usdcClassicID)
	native := mustParseAsset(t, "native")
	usd := mustParseAsset(t, "fiat:USD")
	bookPair, _ := canonical.NewPair(native, usdc)
	directPair, _ := canonical.NewPair(native, usd)
	t0 := fiatParityBucketStart()
	reader := &floorBarsReader{bars: map[string][]v1.OHLCSeriesBar{
		fiatParityPairKey(bookPair):   {floorBar(t0, "0.18", "0.19", "0.17", "0.18", "10000000000", "1800000000", 40, false)},
		fiatParityPairKey(directPair): {floorBar(t0, "7.5", "7.5", "7.5", "7.5", "2", "15", 1, true)},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}}))

	bar := fetchOneFloorBar(t, ts, "native", t0)
	for _, tc := range []struct{ name, got, want string }{
		{"h", bar.H, "0.1900000000"},
		{"l", bar.L, "0.1700000000"},
		{"o", bar.O, "0.1800000000"},
		{"c", bar.C, "0.1800000000"},
		{"v_base", bar.VBase, "10000000002"},
	} {
		if mustRat(t, tc.got).Cmp(mustRat(t, tc.want)) != 0 {
			t.Errorf("%s = %s, want %s — a dust-only constituent set a price beside a floored one", tc.name, tc.got, tc.want)
		}
	}
	if bar.N != 41 {
		t.Errorf("n = %d, want 41 (dust still counts)", bar.N)
	}
}

// TestFiatSeries_DustBookBarDoesNotSuppressPool: AQUA's SDEX book is idle but for one crumb; its real hour
// trades in the USDC SAC pool. The pool's bar must be served, not the crumb's O=H=L=C.
func TestFiatSeries_DustBookBarDoesNotSuppressPool(t *testing.T) {
	_ = installUSDCSACRegistry(t)
	aqua := mustParseAsset(t, aquaClassicID)
	usdcClassic := mustParseAsset(t, usdcClassicID)
	usdcSAC := mustParseAsset(t, pegAliasUSDCSAC)
	bookPair, _ := canonical.NewPair(aqua, usdcClassic)
	poolPair, _ := canonical.NewPair(aqua, usdcSAC)
	t0 := fiatParityBucketStart()
	reader := &floorBarsReader{bars: map[string][]v1.OHLCSeriesBar{
		fiatParityPairKey(bookPair): {floorBar(t0, "9", "9", "9", "9", "1", "9", 1, true)},
		fiatParityPairKey(poolPair): {floorBar(t0, "0.002", "0.0021", "0.0019", "0.002", "50000000000", "100000000", 12, false)},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdcClassic}}))

	bar := fetchOneFloorBar(t, ts, aquaClassicID, t0)
	if mustRat(t, bar.H).Cmp(mustRat(t, "0.0021")) != 0 || mustRat(t, bar.L).Cmp(mustRat(t, "0.0019")) != 0 {
		t.Errorf("h/l = %s/%s, want 0.0021/0.0019 — a dust book bar answered the bucket and suppressed the pool", bar.H, bar.L)
	}
	if bar.N != 13 {
		t.Errorf("n = %d, want 13 (crumb + pool)", bar.N)
	}
}

// TestFiatPoint_DustBookTradeDoesNotSuppressPool is the point-path twin: a sub-cent book print (200 stroops of
// USDC) in the pool's minute must not drop the pool's 20 USDC trade.
func TestFiatPoint_DustBookTradeDoesNotSuppressPool(t *testing.T) {
	_ = installUSDCSACRegistry(t)
	aqua := mustParseAsset(t, aquaClassicID)
	usdcClassic := mustParseAsset(t, usdcClassicID)
	usdcSAC := mustParseAsset(t, pegAliasUSDCSAC)
	bookPair, _ := canonical.NewPair(aqua, usdcClassic)
	poolPair, _ := canonical.NewPair(aqua, usdcSAC)
	t0 := fiatParityBucketStart()
	poolTrade := fiatParityTrade(poolPair, 2, t0.Add(100*time.Second), 1000_0000000, 20_0000000)
	poolTrade.ClearsNotionalFloor = true
	reader := &fiatConstituentReader{tradesByPair: map[string][]canonical.Trade{
		fiatParityPairKey(bookPair): {fiatParityTrade(bookPair, 1, t0.Add(90*time.Second), 1000, 200)},
		fiatParityPairKey(poolPair): {poolTrade},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdcClassic}}))
	win := "&from=" + t0.Format(time.RFC3339) + "&to=" + t0.Add(time.Hour).Format(time.RFC3339)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base="+aquaClassicID+"&quote=fiat:USD&outlier_sigma=0"+win)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("single-bar ohlc status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.OHLCBar `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.TradeCount != 2 {
		t.Errorf("trade_count = %d, want 2 — a dust book print suppressed the pool's trade", env.Data.TradeCount)
	}
}
