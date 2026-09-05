// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"encoding/json"
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Launch-plan row 1.15 (the fiat series reaches a SAC-quoted pool) and
// row 1.14 (the same reach on the point path), landed together so both
// surfaces answer a fiat-quoted question from one population.
//
// A declared USD peg is an ASSET, not a spelling: every Soroban pool
// quotes the peg's SAC wrapper, so a market whose only USD depth is
// such a pool is stored as `<X>/<USDC SAC>` and the peg expansion —
// which emits classic spellings only — never names it. Measured on r1
// 2026-09-05 over 365 days of `prices_1d`: 43 assets have USD depth
// under the USDC SAC and under NO spelling either surface reads,
// carrying 260,833 prints and $14.63M; the largest single one carries
// $6.38M across a full year. Each served `intervals: []` and a `404`.
//
// The widening is gated per BUCKET, not per response, and these tests
// pin both halves of that: a bucket an established spelling answered
// never admits a held-back one (so a thin pool cannot set a bar's high,
// low, count or volume beside book data), and a bucket no established
// spelling answered is filled from the held-back set rather than
// reported as quiet.

const (
	// The r1-measured thin-pool shape, from `prices_1d` on 2026-06-02
	// for GQX-GD7TC72O…: the SDEX book carried 660 prints with a high
	// of 9.5396055089328007, and the `<GQX SAC>/<USDC SAC>` pool
	// carried ONE print worth $0.60 — 0.43% of the day's dollar volume
	// — at 13.0995677490335234. Merged into the book's bucket, that one
	// print moves the served high by +37.32%.
	sacReachBookHigh = "9.5396055089328007"
	sacReachBookLow  = "8.9038086820528625"
	sacReachPoolMark = "13.0995677490335234"
	sacReachBookN    = int64(660)
)

func sacReachDay(n int) time.Time {
	return time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n)
}

// sacReachSeries fetches a daily fiat-quoted series over an explicit
// window, so a bucket can be asked for in a narrow window and in a wide
// one.
func sacReachSeries(t *testing.T, ts *testServer, base, from, to string) fiatSeriesEnvelope {
	t.Helper()
	resp := mustGet(t, ts.URL+"/v1/ohlc?base="+base+"&quote=fiat:USD&interval=1d&from="+from+"&to="+to)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env fiatSeriesEnvelope
	mustDecode(t, resp, &env)
	return env
}

// ── (a) reach: a SAC-quoted pool is the only venue ─────────────────────

// TestFiatSeries_SACQuotedPoolIsTheOnlyVenueIsServed is row 1.15's own
// case. AQUA's only USD market is `AQUA/<USDC SAC>`; the declared peg is
// classic USDC, so every spelling the expansion names comes back empty.
//
// Red before the widening: `intervals: []` — the shape 43 assets on r1
// serve today.
func TestFiatSeries_SACQuotedPoolIsTheOnlyVenueIsServed(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	pool := mkSeriesBar(sacReachDay(0), "0.0041", "0.0042", "0.0040", "0.0041", "1000", "4.1", 3)
	reader := &stubHistoryReader{ohlcByPair: map[string][]v1.OHLCSeriesBar{
		aquaClassicID + "/" + pegAliasUSDCSAC: {pool},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	env := fiatSeriesGet(t, ts, aquaClassicID)
	if len(env.Data.Intervals) != 1 {
		t.Fatalf("intervals = %d, want the pool's bar — no established spelling holds a bucket, "+
			"so the held-back SAC form is where the alternative is no answer at all (reads=%v)",
			len(env.Data.Intervals), reader.ohlcPairs)
	}
	assertBookBar(t, env.Data.Intervals[0], pool)
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false; the series was served through the peg's SAC wrapper")
	}
}

// TestFiatPoint_SACQuotedPoolIsTheOnlyVenueIsServed is row 1.14: the
// point path takes the same held-back arm, so `/v1/vwap` answers the
// asset the series now charts instead of 404ing beside it.
func TestFiatPoint_SACQuotedPoolIsTheOnlyVenueIsServed(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	aqua := mustParseAsset(t, aquaClassicID)
	usdcSAC := mustParseAsset(t, pegAliasUSDCSAC)
	poolPair, err := canonical.NewPair(aqua, usdcSAC)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	t0 := sacReachDay(0)
	reader := &fiatConstituentReader{tradesByPair: map[string][]canonical.Trade{
		fiatParityPairKey(poolPair): {
			fiatParityTrade(poolPair, 1, t0.Add(time.Minute), 1000, 4),
			fiatParityTrade(poolPair, 2, t0.Add(2*time.Minute), 1000, 5),
		},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	resp := mustGet(t, ts.URL+"/v1/vwap?base="+aquaClassicID+"&quote=fiat:USD"+
		"&from="+t0.Format(time.RFC3339)+"&to="+t0.Add(time.Hour).Format(time.RFC3339))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("vwap status = %d, want 200 — the pool is the only USD venue this asset has", resp.StatusCode)
	}
	var env struct {
		Data  v1.VWAPResult `json:"data"`
		Flags struct {
			Triangulated bool `json:"triangulated"`
		} `json:"flags"`
	}
	mustDecode(t, resp, &env)
	if env.Data.TradeCount != 2 {
		t.Errorf("vwap trade_count = %d, want 2 (both pool prints)", env.Data.TradeCount)
	}
	if env.Data.BaseVolume != "2000" {
		t.Errorf("vwap base_volume = %q, want 2000", env.Data.BaseVolume)
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false; the quote leg was proxied through the peg's SAC wrapper")
	}
}

// ── (c) the guarantee: a thin pool never sets a bar beside book data ───

// TestFiatSeries_ThinSACPoolNeverSetsABarBesideBookData is the guard the
// widening exists to need. The book and the pool hold the SAME bucket;
// the served bar must be the book's own, print for print.
//
// Red against a widening that merges the two sets unconditionally: the
// per-bucket max/min and the per-bucket sums take the pool's single
// $0.60 print, and the bar's high moves from 9.5396055089328007 to
// 13.0995677490335234 — the +37.32% the r1 measurement recorded — with
// the count reading 661 rather than 660.
func TestFiatSeries_ThinSACPoolNeverSetsABarBesideBookData(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	day := sacReachDay(0)
	book := mkSeriesBar(day, "9.10", sacReachBookHigh, sacReachBookLow, "9.20", "1000000", "9100000", sacReachBookN)
	pool := mkSeriesBar(day, sacReachPoolMark, sacReachPoolMark, sacReachPoolMark, sacReachPoolMark, "46", "600", 1)
	reader := &stubHistoryReader{ohlcByPair: map[string][]v1.OHLCSeriesBar{
		pegAliasAquaClassic + "/" + usdcClassicID: {book},
		pegAliasAquaSAC + "/" + pegAliasUSDCSAC:   {pool},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	env := fiatSeriesGet(t, ts, pegAliasAquaClassic)
	if len(env.Data.Intervals) != 1 {
		t.Fatalf("intervals = %d, want 1 — one bucket (reads=%v)", len(env.Data.Intervals), reader.ohlcPairs)
	}
	got := env.Data.Intervals[0]
	if got.N != sacReachBookN {
		t.Errorf("n = %d, want %d — the pool's single print inflated the book's count", got.N, sacReachBookN)
	}
	if g, w := mustFloat(t, got.H), mustFloat(t, sacReachBookHigh); !approxEq(g, w) {
		t.Errorf("h = %s, want %s — one $0.60 pool print set the bar's high against a book "+
			"trading a million units (the +37.32%% move measured on r1)", got.H, sacReachBookHigh)
	}
	if g, w := mustFloat(t, got.L), mustFloat(t, sacReachBookLow); !approxEq(g, w) {
		t.Errorf("l = %s, want %s", got.L, sacReachBookLow)
	}
	if got.VBase != book.VBase {
		t.Errorf("v_base = %q, want %q — the pool's volume was summed into the book's", got.VBase, book.VBase)
	}
}

// ── (b) the decision: suppression is per BUCKET, never per response ────

// TestFiatSeries_PoolFillsOnlyTheBucketsTheBookCannotAnswer pins the
// decision recorded in docs/architecture/aggregate-alias-folding.md §7.5.
// The book holds day 1; the pool holds days 1, 2 and 3.
//
// Served: three bars. Day 1 is the book's alone — the pool is dropped
// from it entirely, which is what makes the guarantee above absolute
// rather than arithmetic — and days 2 and 3 are the pool's, because the
// alternative for them is reporting a market that traded as quiet.
//
// Red against a first-hit evaluated once per RESPONSE: the established
// set answered somewhere in the window, so the held-back set is never
// read and one bar is served. Measured on r1, that alternative drops
// 3,356 daily buckets carrying 671,712 prints and $175.96M across the
// 24 assets that have both a book and a pool.
func TestFiatSeries_PoolFillsOnlyTheBucketsTheBookCannotAnswer(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	book := mkSeriesBar(sacReachDay(0), "0.0041", "0.0042", "0.0040", "0.0041", "100", "0.41", 1)
	poolDay1 := mkSeriesBar(sacReachDay(0), "0.0030", "0.5000", "0.0100", "0.0035", "20", "0.07", 50)
	poolDay2 := mkSeriesBar(sacReachDay(1), "0.0035", "0.0036", "0.0034", "0.0035", "10", "0.035", 3)
	poolDay3 := mkSeriesBar(sacReachDay(2), "0.0038", "0.0039", "0.0037", "0.0038", "12", "0.045", 4)
	reader := &stubHistoryReader{ohlcByPair: map[string][]v1.OHLCSeriesBar{
		pegAliasAquaClassic + "/" + usdcClassicID: {book},
		pegAliasAquaSAC + "/" + pegAliasUSDCSAC:   {poolDay1, poolDay2, poolDay3},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	env := fiatSeriesGet(t, ts, pegAliasAquaClassic)
	if len(env.Data.Intervals) != 3 {
		t.Fatalf("intervals = %d, want 3 — the book's day 1 plus the two days only the pool traded "+
			"(reads=%v): %+v", len(env.Data.Intervals), reader.ohlcPairs, env.Data.Intervals)
	}
	assertBookBar(t, env.Data.Intervals[0], book)
	assertBookBar(t, env.Data.Intervals[1], poolDay2)
	assertBookBar(t, env.Data.Intervals[2], poolDay3)
}

// mkSourcedBar is [mkSeriesBar] with the CAGG's `sources` column filled
// in. It matters because the combine resolves a bar's smallest-unit
// scale from that column and from nothing else: a fixture that leaves it
// empty makes every bar unknown-scale, every lift factor 10^0, and every
// assertion about scaling vacuous.
func mkSourcedBar(t time.Time, o, h, l, c, vb, vq string, n int64, sources ...string) v1.OHLCSeriesBar {
	bar := mkSeriesBar(t, o, h, l, c, vb, vq, n)
	bar.Sources = sources
	return bar
}

// TestFiatSeries_ABucketRendersTheSameInEveryWindow is the property that
// decides §7.5 rather than a preference. Resolving admission once per
// RESPONSE makes the constituent set a function of the WINDOW, so the
// same day renders one way in a window the book also covers and another
// way in a window it does not — the same day charting differently at 3d
// and at 1d, from one unchanged database.
//
// Every bucket is checked against itself fetched alone, and the bars
// carry REAL venue sources at two different scales, which is what makes
// the check bite. An earlier version of this test used [mkSeriesBar],
// whose bars name no venue: every bar was then unknown-scale, every lift
// factor was 10^0, and the test passed with the scale machinery deleted
// outright while the property it names was broken. The lift target is
// per bucket for exactly this reason — a response-wide maximum is a
// second, quieter way for a window to change a bar.
func TestFiatSeries_ABucketRendersTheSameInEveryWindow(t *testing.T) {
	cases := []struct {
		name             string
		bookSrc, poolSrc string
	}{
		// The book is coarser than the pool: admitting the pool used to
		// lift the BOOK's bucket by 10 in any window reaching both.
		{"7dp book, 8dp pool", "sdex", "some-unregistered-amm"},
		// And the mirror — the pool is coarser, so its own bucket used
		// to be lifted by a CEX day it never shared a bucket with.
		{"8dp book, 7dp pool", "binance", "aquarius"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usdc := installPegAliasRegistry(t)
			book := mkSourcedBar(sacReachDay(0), "0.0041", "0.0042", "0.0040", "0.0041", "1000000", "4100", 660, tc.bookSrc)
			pool := mkSourcedBar(sacReachDay(1), "0.0035", "0.0036", "0.0034", "0.0035", "500", "1.75", 3, tc.poolSrc)
			reader := &stubHistoryReader{ohlcByPair: map[string][]v1.OHLCSeriesBar{
				pegAliasAquaClassic + "/" + usdcClassicID: {book},
				pegAliasAquaSAC + "/" + pegAliasUSDCSAC:   {pool},
			}}
			ts := httpTestServer(t, v1.New(v1.Options{
				History:           reader,
				USDPeggedClassics: []canonical.Asset{usdc},
			}))

			wide := sacReachSeries(t, ts, pegAliasAquaClassic, "2024-06-01T00:00:00Z", "2024-06-04T00:00:00Z")
			if len(wide.Data.Intervals) != 2 {
				t.Fatalf("wide window served %d bars, want the book's day and the pool's: %+v",
					len(wide.Data.Intervals), wide.Data.Intervals)
			}
			for i := range wide.Data.Intervals {
				bar := wide.Data.Intervals[i]
				day := bar.T.UTC().Format("2006-01-02")
				alone := sacReachSeries(t, ts, pegAliasAquaClassic,
					bar.T.UTC().Format(time.RFC3339), bar.T.UTC().AddDate(0, 0, 1).Format(time.RFC3339))
				if len(alone.Data.Intervals) != 1 {
					t.Fatalf("%s served %d bars when asked for alone", day, len(alone.Data.Intervals))
				}
				gotWide, err := json.Marshal(&bar)
				if err != nil {
					t.Fatalf("marshal wide: %v", err)
				}
				gotAlone, err := json.Marshal(&alone.Data.Intervals[0])
				if err != nil {
					t.Fatalf("marshal narrow: %v", err)
				}
				if string(gotWide) != string(gotAlone) {
					t.Errorf("%s renders differently by window:\n in a 3-day window %s\n on its own      %s\n"+
						"a served bar must be a function of its bucket, not of what else the caller asked for",
						day, gotWide, gotAlone)
				}
			}
		})
	}
}

// TestFiatSeries_EstablishedMixedScaleBucketIsWindowInvariant pins the
// same property with NO held-back set in play at all — two established
// constituents, at two venue scales, on two days.
//
// This shape predates the widening and was broken before it: on the
// pre-widening tree the 7dp day served v_base 1000000 asked for alone
// and 10000000 asked for beside the 8dp day, because the lift target was
// the maximum across the RESPONSE. Scoping it to the bucket fixes that
// too, so this is a regression pin on a defect the widening inherited
// rather than caused.
func TestFiatSeries_EstablishedMixedScaleBucketIsWindowInvariant(t *testing.T) {
	usdc := mustParseAsset(t, usdcClassicID)
	onChain := mkSourcedBar(sacReachDay(0), "9.10", "9.20", "9.00", "9.15", "1000000", "9100000", 660, "sdex")
	cex := mkSourcedBar(sacReachDay(1), "9.10", "9.20", "9.00", "9.15", "500", "4550", 12, "binance")
	reader := &stubHistoryReader{ohlcByPair: map[string][]v1.OHLCSeriesBar{
		"native/" + usdcClassicID:     {onChain},
		"crypto:XLM/" + "crypto:USDT": {cex},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	alone := sacReachSeries(t, ts, "native", "2024-06-01T00:00:00Z", "2024-06-02T00:00:00Z")
	both := sacReachSeries(t, ts, "native", "2024-06-01T00:00:00Z", "2024-06-04T00:00:00Z")
	if len(alone.Data.Intervals) != 1 || len(both.Data.Intervals) != 2 {
		t.Fatalf("served %d / %d bars, want 1 / 2", len(alone.Data.Intervals), len(both.Data.Intervals))
	}
	if got, want := both.Data.Intervals[0].VBase, alone.Data.Intervals[0].VBase; got != want {
		t.Errorf("the on-chain day's v_base = %q beside the CEX day and %q on its own — "+
			"a bar cannot change units because of a bar in another bucket", got, want)
	}
	if got := alone.Data.Intervals[0].VBase; got != "1000000" {
		t.Errorf("on-chain day v_base = %q, want 1000000 — its own venues' raw sum", got)
	}
}

// ── point == series, over constituents that genuinely differ ───────────

// The three constituents below are stamped at DIFFERENT sources, so the
// series' per-bar scale lift and the point path's NormalizeAmountScale
// both actually run. The pre-existing parity fixture stamps every trade
// `sdex`, which holds the invariant vacuously: one scale means every
// lift factor is 10^0 and a broken lift is invisible.
//
//	native/<USDC classic>   sdex     (7dp)   7000 base /  1260 quote → 0.18
//	crypto:XLM/fiat:USD     binance  (8dp)  20000 base /  3800 quote → 0.19
//	native/fiat:USD         coinbase (8dp)  10000 base /  2000 quote → 0.20
//
// Lifted to the common 8dp scale the sdex leg is 70000/12600, so
// Σbase = 100000, Σquote = 18400 and the mean is 0.184 exactly.
const (
	mixedScaleParityVWAP       = "0.1840000000"
	mixedScaleParityBaseVolume = "100000"
	mixedScaleParityQuoteVol   = "18400"
)

func mixedScaleParityTrade(source string, pair canonical.Pair, ledger uint32, ts time.Time, base, quote int64) canonical.Trade {
	tr := fiatParityTrade(pair, ledger, ts, base, quote)
	tr.Source = source
	return tr
}

// newMixedScaleParityServer wires the fixture above through the reader
// that models the CAGG over the same rows, so any divergence the test
// observes is the two handlers' methodology and not the fixture.
func newMixedScaleParityServer(t *testing.T) *v1.Server {
	t.Helper()
	usdc := mustParseAsset(t, usdcClassicID)
	xlmNative := mustParseAsset(t, "native")
	xlmTicker := mustParseAsset(t, "crypto:XLM")
	usd := mustParseAsset(t, "fiat:USD")

	sdexPair, _ := canonical.NewPair(xlmNative, usdc)
	cexPair, _ := canonical.NewPair(xlmTicker, usd)
	directPair, _ := canonical.NewPair(xlmNative, usd)

	t0 := fiatParityBucketStart()
	reader := &fiatConstituentReader{tradesByPair: map[string][]canonical.Trade{
		fiatParityPairKey(sdexPair):   {mixedScaleParityTrade("sdex", sdexPair, 1, t0, 7000, 1260)},
		fiatParityPairKey(cexPair):    {mixedScaleParityTrade("binance", cexPair, 2, t0.Add(5*time.Minute), 20000, 3800)},
		fiatParityPairKey(directPair): {mixedScaleParityTrade("coinbase", directPair, 3, t0.Add(10*time.Minute), 10000, 2000)},
	}}
	return v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}})
}

// TestFiatPointMatchesSeries_AcrossVenueScales holds the C1-024 parity
// invariant where it can actually fail: three constituents at two
// smallest-unit scales. Point and series must agree on the price and on
// both volumes, which is only true if each lifts the 7dp leg by the same
// exact factor of ten.
func TestFiatPointMatchesSeries_AcrossVenueScales(t *testing.T) {
	ts := httpTestServer(t, newMixedScaleParityServer(t))
	bar := fetchFiatSeriesBar(t, ts.URL)

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD"+fiatParityWindow())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("vwap status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.VWAPResult `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.Price != mixedScaleParityVWAP {
		t.Errorf("vwap price = %q, want %q (Σquote/Σbase at one scale)", env.Data.Price, mixedScaleParityVWAP)
	}
	if env.Data.BaseVolume != mixedScaleParityBaseVolume {
		t.Errorf("vwap base_volume = %q, want %q", env.Data.BaseVolume, mixedScaleParityBaseVolume)
	}
	if env.Data.QuoteVolume != mixedScaleParityQuoteVol {
		t.Errorf("vwap quote_volume = %q, want %q", env.Data.QuoteVolume, mixedScaleParityQuoteVol)
	}
	for _, tc := range []struct{ name, point, series string }{
		{"base_volume", env.Data.BaseVolume, bar.VBase},
		{"quote_volume", env.Data.QuoteVolume, bar.VQuote},
	} {
		if mustRat(t, tc.point).Cmp(mustRat(t, tc.series)) != 0 {
			t.Errorf("%s point %s != series %s — the two surfaces weight one population two ways",
				tc.name, tc.point, tc.series)
		}
	}
	seriesVWAP := new(big.Rat).Quo(mustRat(t, bar.VQuote), mustRat(t, bar.VBase))
	if got := mustRat(t, env.Data.Price); got.Cmp(seriesVWAP) != 0 {
		t.Errorf("vwap price %s != series v_quote/v_base %s", env.Data.Price, seriesVWAP.FloatString(10))
	}
}

// TestFiatPointMatchesSeries_OnAPoolOnlyVenue holds the same invariant
// on the arm rows 1.14 and 1.15 add. A point window IS one bucket, so
// "read the held-back set when the established set answered nothing"
// and "fill the buckets the established set did not answer" are one rule
// stated at each path's own grain — and the two surfaces stay on one
// population where they previously served a series and a 404.
func TestFiatPointMatchesSeries_OnAPoolOnlyVenue(t *testing.T) {
	usdc := installUSDCSACRegistry(t)
	aqua := mustParseAsset(t, aquaClassicID)
	usdcSAC := mustParseAsset(t, pegAliasUSDCSAC)
	poolPair, err := canonical.NewPair(aqua, usdcSAC)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	t0 := fiatParityBucketStart()
	reader := &fiatConstituentReader{tradesByPair: map[string][]canonical.Trade{
		fiatParityPairKey(poolPair): {
			mixedScaleParityTrade("sdex", poolPair, 1, t0, 7000, 1260),
			mixedScaleParityTrade("sdex", poolPair, 2, t0.Add(5*time.Minute), 3000, 600),
		},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	seriesResp := mustGet(t, ts.URL+"/v1/ohlc?base="+aquaClassicID+"&quote=fiat:USD&interval=1h&limit=1"+fiatParityWindow())
	if seriesResp.StatusCode != http.StatusOK {
		t.Fatalf("series status = %d, want 200", seriesResp.StatusCode)
	}
	var seriesEnv struct {
		Data v1.OHLCSeriesResponse `json:"data"`
	}
	mustDecode(t, seriesResp, &seriesEnv)
	if len(seriesEnv.Data.Intervals) != 1 {
		t.Fatalf("series returned %d bars, want 1", len(seriesEnv.Data.Intervals))
	}
	bar := seriesEnv.Data.Intervals[0]

	resp := mustGet(t, ts.URL+"/v1/vwap?base="+aquaClassicID+"&quote=fiat:USD"+fiatParityWindow())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("vwap status = %d, want 200 — the series serves this asset, so the point must too", resp.StatusCode)
	}
	var env struct {
		Data v1.VWAPResult `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.TradeCount != 2 {
		t.Errorf("vwap trade_count = %d, want 2", env.Data.TradeCount)
	}
	if mustRat(t, env.Data.BaseVolume).Cmp(mustRat(t, bar.VBase)) != 0 {
		t.Errorf("vwap base_volume %s != series v_base %s — point and series are reading "+
			"different constituent sets on the held-back arm", env.Data.BaseVolume, bar.VBase)
	}
	if mustRat(t, env.Data.QuoteVolume).Cmp(mustRat(t, bar.VQuote)) != 0 {
		t.Errorf("vwap quote_volume %s != series v_quote %s", env.Data.QuoteVolume, bar.VQuote)
	}
}

// ── point == series over a window of MORE than one bucket ──────────────

// twoBucketFixture builds a book print in the first hour of the window
// and a held-back pool print in the second — the shape where a gate
// resolved once per window puts the two surfaces on two populations.
func twoBucketFixture(t *testing.T) (*testServer, string, time.Time, time.Time) {
	t.Helper()
	_ = installUSDCSACRegistry(t)
	aqua := mustParseAsset(t, aquaClassicID)
	usdcClassic := mustParseAsset(t, usdcClassicID)
	usdcSAC := mustParseAsset(t, pegAliasUSDCSAC)
	bookPair, err := canonical.NewPair(aqua, usdcClassic)
	if err != nil {
		t.Fatalf("NewPair book: %v", err)
	}
	poolPair, err := canonical.NewPair(aqua, usdcSAC)
	if err != nil {
		t.Fatalf("NewPair pool: %v", err)
	}
	t0 := fiatParityBucketStart()
	t1 := t0.Add(time.Hour)
	reader := &fiatConstituentReader{tradesByPair: map[string][]canonical.Trade{
		fiatParityPairKey(bookPair): {fiatParityTrade(bookPair, 1, t0.Add(time.Minute), 1000, 200)},
		fiatParityPairKey(poolPair): {fiatParityTrade(poolPair, 2, t1.Add(time.Minute), 500, 150)},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdcClassic},
	}))
	return ts, "&from=" + t0.Format(time.RFC3339) + "&to=" + t1.Add(time.Hour).Format(time.RFC3339), t0, t1
}

// seriesTotals sums a series' bars, which is what a point over the same
// window has to equal if the two are reading one population.
func seriesTotals(t *testing.T, ts *testServer, base, interval, win string) (n int64, vBase, vQuote *big.Rat) {
	t.Helper()
	resp := mustGet(t, ts.URL+"/v1/ohlc?base="+base+"&quote=fiat:USD&interval="+interval+win)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("series status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.OHLCSeriesResponse `json:"data"`
	}
	mustDecode(t, resp, &env)
	vBase, vQuote = new(big.Rat), new(big.Rat)
	for _, iv := range env.Data.Intervals {
		n += iv.N
		vBase.Add(vBase, mustRat(t, iv.VBase))
		vQuote.Add(vQuote, mustRat(t, iv.VQuote))
	}
	return n, vBase, vQuote
}

// pointTotals reads /v1/vwap over the same window.
func pointTotals(t *testing.T, ts *testServer, base, win string) (n int64, vBase, vQuote *big.Rat) {
	t.Helper()
	resp := mustGet(t, ts.URL+"/v1/vwap?base="+base+"&quote=fiat:USD"+win)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("vwap status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.VWAPResult `json:"data"`
	}
	mustDecode(t, resp, &env)
	return int64(env.Data.TradeCount), mustRat(t, env.Data.BaseVolume), mustRat(t, env.Data.QuoteVolume)
}

// TestFiatPointMatchesSeries_OverAMultiBucketWindow is the C1-024
// invariant on a window of more than one bucket — where a first attempt
// at rows 1.14/1.15 broke it.
//
// That attempt gated the point path on the whole window ("read the
// held-back set only when the established set returned nothing"), on the
// reasoning that a point window is its own bucket. It is, but only when
// the window IS one bucket, and every parity fixture in the tree was
// exactly one bucket, so nothing saw it. With the book in hour 1 and the
// pool in hour 2, the series served both and `/v1/vwap` served the book
// alone: n=1 v_base=1000 against n=2 v_base=1500, two surfaces answering
// one window from two populations.
func TestFiatPointMatchesSeries_OverAMultiBucketWindow(t *testing.T) {
	ts, win, _, _ := twoBucketFixture(t)
	sn, sBase, sQuote := seriesTotals(t, ts, aquaClassicID, "1h", win)
	pn, pBase, pQuote := pointTotals(t, ts, aquaClassicID, win)
	if sn != 2 {
		t.Fatalf("series served n=%d over two buckets, want 2 — the fixture is not exercising the gate", sn)
	}
	if pn != sn || pBase.Cmp(sBase) != 0 || pQuote.Cmp(sQuote) != 0 {
		t.Errorf("point n=%d v_base=%s v_quote=%s != series n=%d v_base=%s v_quote=%s — "+
			"the same window answered from two populations",
			pn, pBase.RatString(), pQuote.RatString(), sn, sBase.RatString(), sQuote.RatString())
	}
}

// TestFiatPointEqualsTheFinestSeriesExactly states the claim precisely
// rather than approximately. The point path resolves its held-back gate
// at the FINEST interval the series can be asked for, so the equality is
// exact against `interval=1m`.
//
// It is not exact against every interval, and it cannot be: per-bucket
// admission is a function of what a bucket IS, so a coarser bucket is
// likelier to hold an established print and therefore suppresses more.
// That is the caller's question changing, not the population splitting —
// both surfaces run one rule over one constituent split. Pinning the
// finest grain is what keeps that statement checkable.
func TestFiatPointEqualsTheFinestSeriesExactly(t *testing.T) {
	ts, win, _, _ := twoBucketFixture(t)
	sn, sBase, sQuote := seriesTotals(t, ts, aquaClassicID, "1m", win)
	pn, pBase, pQuote := pointTotals(t, ts, aquaClassicID, win)
	if pn != sn || pBase.Cmp(sBase) != 0 || pQuote.Cmp(sQuote) != 0 {
		t.Errorf("point n=%d v_base=%s v_quote=%s != 1m series n=%d v_base=%s v_quote=%s",
			pn, pBase.RatString(), pQuote.RatString(), sn, sBase.RatString(), sQuote.RatString())
	}
}

// TestFiatPoint_PoolInAnAnsweredBucketIsSuppressed is the point-path
// twin of the thin-pool guarantee: widening the point path's reach must
// not let a pool print join a bucket the book already answered. Both
// prints sit in the SAME minute, so the gate drops the pool and the
// served aggregate is the book's alone.
//
// The pool print is priced at 0.80 against the book's 0.20, so a failure
// is loud: it takes the served high to 0.80 and the volume to 1500.
func TestFiatPoint_PoolInAnAnsweredBucketIsSuppressed(t *testing.T) {
	_ = installUSDCSACRegistry(t)
	aqua := mustParseAsset(t, aquaClassicID)
	usdcClassic := mustParseAsset(t, usdcClassicID)
	usdcSAC := mustParseAsset(t, pegAliasUSDCSAC)
	bookPair, err := canonical.NewPair(aqua, usdcClassic)
	if err != nil {
		t.Fatalf("NewPair book: %v", err)
	}
	poolPair, err := canonical.NewPair(aqua, usdcSAC)
	if err != nil {
		t.Fatalf("NewPair pool: %v", err)
	}
	t0 := fiatParityBucketStart()
	reader := &fiatConstituentReader{tradesByPair: map[string][]canonical.Trade{
		fiatParityPairKey(bookPair): {fiatParityTrade(bookPair, 1, t0.Add(90*time.Second), 1000, 200)},
		// same minute as the book's print, 4x the price
		fiatParityPairKey(poolPair): {fiatParityTrade(poolPair, 2, t0.Add(100*time.Second), 500, 400)},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdcClassic},
	}))
	win := "&from=" + t0.Format(time.RFC3339) + "&to=" + t0.Add(time.Hour).Format(time.RFC3339)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base="+aquaClassicID+"&quote=fiat:USD&outlier_sigma=0"+win)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("single-bar ohlc status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.OHLCBar `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.TradeCount != 1 {
		t.Errorf("trade_count = %d, want 1 — the pool print shares the book's bucket", env.Data.TradeCount)
	}
	if env.Data.High != "0.2000000000" {
		t.Errorf("high = %q, want 0.2000000000 — a pool print set the high of a bucket the book answered",
			env.Data.High)
	}
	if env.Data.BaseVolume != "1000" {
		t.Errorf("base_volume = %q, want 1000", env.Data.BaseVolume)
	}
}
