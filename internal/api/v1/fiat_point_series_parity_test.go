// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// C1-024 (audit-2026-07-23): a fiat-quoted POINT quote and the fiat-quoted
// SERIES it belongs to were computed over different trade populations.
//
//   - SERIES (`/v1/ohlc?interval=…`) COMBINES every constituent of
//     usdPeggedConstituents — the direct fiat pair, every base alias, the
//     crypto stablecoin backers and the operator's classic pegs — which is
//     the live aggregator's own source set.
//   - POINT (`/v1/vwap`, `/v1/twap`, single-bar `/v1/ohlc`) read the LITERAL
//     pair and, only if that came back empty, walked the classic pegs and
//     took the FIRST non-empty one. Exactly one constituent, no base
//     aliases, no crypto backers.
//
// So the same `?base=native&quote=fiat:USD` question answered differently
// depending on whether the client asked for a point or a series, and a
// published point quote could not be reconciled against the published
// series through which it runs. These tests pin the unification: the point
// path derives from the identical constituent selection.

// ── fixture ────────────────────────────────────────────────────────────
//
// One 1h bucket, three constituents that all really exist on pubnet, each
// with a single trade so every constituent bar is exact:
//
//	t0+00m  native/<USDC-classic>   7000 base / 1260 quote → 0.18  (SDEX depth;
//	                                the ONLY leg the old fallback could reach,
//	                                and only when the literal pair was empty)
//	t0+05m  native/fiat:USD         1000 base /  200 quote → 0.20  (direct fiat;
//	                                non-empty, so the old code stopped here)
//	t0+10m  crypto:XLM/fiat:USD     2000 base /  380 quote → 0.19  (the CEX
//	                                stream, which lives under the alias form and
//	                                was unreachable from ?base=native)
//
// Combined: Σbase = 10000, Σquote = 1840 → VWAP = 1840/10000 = 0.184 exactly.
// Pre-fix the point path served 200/1000 = 0.20 on 1000 base volume.
const (
	fiatParityWantVWAP       = "0.1840000000"
	fiatParityWantBaseVolume = "10000"
	fiatParityWantQuoteVol   = "1840"
	fiatParityWantHigh       = "0.2000000000"
	fiatParityWantLow        = "0.1800000000"
	fiatParityPreFixVWAP     = "0.2000000000" // literal-pair-only answer
)

func fiatParityBucketStart() time.Time {
	return time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)
}

// fiatConstituentReader serves raw trades per pair AND models the
// prices_<n> continuous aggregate over those SAME rows, so the CAGG the
// series reads and the trades the point path reads cannot disagree by
// construction — any divergence the tests below observe is the handlers'
// methodology, not the fixture's.
type fiatConstituentReader struct {
	stubHistoryReader
	tradesByPair map[string][]canonical.Trade
}

func fiatParityPairKey(p canonical.Pair) string {
	return p.Base.String() + "/" + p.Quote.String()
}

func (r *fiatConstituentReader) inWindow(pair canonical.Pair, from, to time.Time) []canonical.Trade {
	var out []canonical.Trade
	for _, t := range r.tradesByPair[fiatParityPairKey(pair)] {
		if t.Timestamp.Before(from) || !t.Timestamp.Before(to) {
			continue
		}
		out = append(out, t)
	}
	return out
}

func (r *fiatConstituentReader) TradesInRange(
	_ context.Context, pair canonical.Pair, from, to time.Time, _ int,
) ([]canonical.Trade, error) {
	return r.inWindow(pair, from, to), nil
}

// OHLCSeries derives each constituent's bar from that constituent's own
// trades exactly as migration 0002's aggregate does: open/close are the
// chronological first/last price, high/low the extremes, v_base/v_quote the
// raw sums, n the count. Only the 1h interval is modelled — the only one
// the parity tests request.
func (r *fiatConstituentReader) OHLCSeries(
	_ context.Context, pair canonical.Pair, interval string, from, to time.Time, _ int,
) ([]v1.OHLCSeriesBar, error) {
	if interval != "1h" {
		return nil, nil
	}
	byBucket := map[time.Time][]canonical.Trade{}
	for _, t := range r.inWindow(pair, from, to) {
		b := t.Timestamp.Truncate(time.Hour)
		byBucket[b] = append(byBucket[b], t)
	}
	out := make([]v1.OHLCSeriesBar, 0, len(byBucket))
	for b, trades := range byBucket {
		vBase, vQuote := new(big.Int), new(big.Int)
		var high, low *big.Rat
		for _, t := range trades {
			vBase.Add(vBase, t.BaseAmount.BigInt())
			vQuote.Add(vQuote, t.QuoteAmount.BigInt())
			p := new(big.Rat).SetFrac(t.QuoteAmount.BigInt(), t.BaseAmount.BigInt())
			if high == nil || p.Cmp(high) > 0 {
				high = p
			}
			if low == nil || p.Cmp(low) < 0 {
				low = p
			}
		}
		first := trades[0]
		last := trades[len(trades)-1]
		open := new(big.Rat).SetFrac(first.QuoteAmount.BigInt(), first.BaseAmount.BigInt())
		closeP := new(big.Rat).SetFrac(last.QuoteAmount.BigInt(), last.BaseAmount.BigInt())
		out = append(out, v1.OHLCSeriesBar{
			T:      b,
			O:      open.FloatString(10),
			H:      high.FloatString(10),
			L:      low.FloatString(10),
			C:      closeP.FloatString(10),
			VBase:  vBase.String(),
			VQuote: vQuote.String(),
			N:      int64(len(trades)),
		})
	}
	return out, nil
}

func fiatParityTrade(pair canonical.Pair, ledger uint32, ts time.Time, base, quote int64) canonical.Trade {
	return canonical.Trade{
		Source: "sdex", Ledger: ledger,
		TxHash:      fmt.Sprintf("%064d", ledger),
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(base)),
		QuoteAmount: canonical.NewAmount(big.NewInt(quote)),
	}
}

// newFiatParityServer wires the three-constituent fixture described above.
func newFiatParityServer(t *testing.T) *v1.Server {
	t.Helper()
	usdc, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	xlmNative, _ := canonical.ParseAsset("native")
	xlmTicker, _ := canonical.ParseAsset("crypto:XLM")
	usd, _ := canonical.ParseAsset("fiat:USD")

	sdexPair, _ := canonical.NewPair(xlmNative, usdc)
	directPair, _ := canonical.NewPair(xlmNative, usd)
	cexPair, _ := canonical.NewPair(xlmTicker, usd)

	t0 := fiatParityBucketStart()
	reader := &fiatConstituentReader{
		tradesByPair: map[string][]canonical.Trade{
			fiatParityPairKey(sdexPair):   {fiatParityTrade(sdexPair, 1, t0, 7000, 1260)},
			fiatParityPairKey(directPair): {fiatParityTrade(directPair, 2, t0.Add(5*time.Minute), 1000, 200)},
			fiatParityPairKey(cexPair):    {fiatParityTrade(cexPair, 3, t0.Add(10*time.Minute), 2000, 380)},
		},
	}
	return v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	})
}

// fiatParityWindow renders the shared explicit window as query params. Both
// modes take from/to verbatim when explicit, so point and series cover the
// identical [t0, t0+1h) span.
func fiatParityWindow() string {
	t0 := fiatParityBucketStart()
	return "&from=" + t0.Format(time.RFC3339) + "&to=" + t0.Add(time.Hour).Format(time.RFC3339)
}

func mustRat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("unparseable decimal %q", s)
	}
	return r
}

// fetchFiatSeriesBar pulls the single combined 1h bar the series serves.
func fetchFiatSeriesBar(t *testing.T, base string) v1.OHLCSeriesBar {
	t.Helper()
	resp := mustGet(t, base+"/v1/ohlc?base=native&quote=fiat:USD&interval=1h&limit=1"+fiatParityWindow())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("series status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.OHLCSeriesResponse `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data.Intervals) != 1 {
		t.Fatalf("series returned %d bars, want 1", len(env.Data.Intervals))
	}
	return env.Data.Intervals[0]
}

// TestFiatVWAPPointMatchesSeries is the C1-024 regression: /v1/vwap must be
// computed over the same constituent population the series combines, so its
// price equals the series bar's own Σquote/Σbase and its volumes equal the
// bar's volumes exactly.
//
// Pre-fix this served 0.2000000000 on base_volume 1000 — the literal
// native/fiat:USD pair alone — against a series bar summing 10000 base.
func TestFiatVWAPPointMatchesSeries(t *testing.T) {
	srv := newFiatParityServer(t)
	ts := httpTestServer(t, srv)
	bar := fetchFiatSeriesBar(t, ts.URL)

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD"+fiatParityWindow())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("vwap status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data  v1.VWAPResult `json:"data"`
		Flags struct {
			Triangulated bool `json:"triangulated"`
		} `json:"flags"`
	}
	mustDecode(t, resp, &env)

	// (a) the corrected value itself.
	if env.Data.Price != fiatParityWantVWAP {
		t.Errorf("vwap price = %q, want %q (Σquote/Σbase = 1840/10000 over all "+
			"three constituents; %q is the pre-fix literal-pair-only answer)",
			env.Data.Price, fiatParityWantVWAP, fiatParityPreFixVWAP)
	}
	if env.Data.BaseVolume != fiatParityWantBaseVolume {
		t.Errorf("vwap base_volume = %q, want %q", env.Data.BaseVolume, fiatParityWantBaseVolume)
	}
	if env.Data.QuoteVolume != fiatParityWantQuoteVol {
		t.Errorf("vwap quote_volume = %q, want %q", env.Data.QuoteVolume, fiatParityWantQuoteVol)
	}
	if env.Data.TradeCount != 3 {
		t.Errorf("vwap trade_count = %d, want 3 (one per constituent)", env.Data.TradeCount)
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false, want true — a USDC-quoted leg contributed")
	}

	// (b) the invariant: point == series at the shared timestamps.
	if got, want := mustRat(t, env.Data.BaseVolume), mustRat(t, bar.VBase); got.Cmp(want) != 0 {
		t.Errorf("vwap base_volume %s != series v_base %s — point and series are "+
			"reading different constituent sets", env.Data.BaseVolume, bar.VBase)
	}
	if got, want := mustRat(t, env.Data.QuoteVolume), mustRat(t, bar.VQuote); got.Cmp(want) != 0 {
		t.Errorf("vwap quote_volume %s != series v_quote %s", env.Data.QuoteVolume, bar.VQuote)
	}
	seriesVWAP := new(big.Rat).Quo(mustRat(t, bar.VQuote), mustRat(t, bar.VBase))
	if got := mustRat(t, env.Data.Price); got.Cmp(seriesVWAP) != 0 {
		t.Errorf("vwap price %s != series v_quote/v_base %s — the point quote does "+
			"not belong to the series it is published alongside",
			env.Data.Price, seriesVWAP.FloatString(10))
	}
}

// TestFiatSingleBarOHLCMatchesSeriesExtremes pins the same unification for
// the single-bar /v1/ohlc branch: its high/low and volumes must equal the
// combined series bar's.
//
// outlier_sigma=0 because the single-bar branch defaults to a 4σ MAD filter
// the CAGG series has no equivalent of — a separate, still-open divergence
// (audit MNY-22) that must not be conflated with the constituent-set one
// under test here.
//
// open/close are deliberately NOT compared: the series' combined open/close
// are base-volume-weighted across constituents (a CAGG cannot know the
// cross-constituent chronological order), which ohlcSeriesFiatCombined
// documents as its one approximation. The point path's are the true
// first/last prints.
func TestFiatSingleBarOHLCMatchesSeriesExtremes(t *testing.T) {
	srv := newFiatParityServer(t)
	ts := httpTestServer(t, srv)
	bar := fetchFiatSeriesBar(t, ts.URL)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&outlier_sigma=0"+fiatParityWindow())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("single-bar ohlc status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.OHLCBar `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.High != fiatParityWantHigh {
		t.Errorf("single-bar high = %q, want %q (max across all constituents)",
			env.Data.High, fiatParityWantHigh)
	}
	if env.Data.Low != fiatParityWantLow {
		t.Errorf("single-bar low = %q, want %q (min across all constituents)",
			env.Data.Low, fiatParityWantLow)
	}
	if env.Data.BaseVolume != fiatParityWantBaseVolume {
		t.Errorf("single-bar base_volume = %q, want %q", env.Data.BaseVolume, fiatParityWantBaseVolume)
	}
	if env.Data.TradeCount != 3 {
		t.Errorf("single-bar trade_count = %d, want 3", env.Data.TradeCount)
	}

	for _, tc := range []struct{ name, point, series string }{
		{"high", env.Data.High, bar.H},
		{"low", env.Data.Low, bar.L},
		{"base_volume", env.Data.BaseVolume, bar.VBase},
		{"quote_volume", env.Data.QuoteVolume, bar.VQuote},
	} {
		if mustRat(t, tc.point).Cmp(mustRat(t, tc.series)) != 0 {
			t.Errorf("single-bar %s %s != series %s — point and series are reading "+
				"different constituent sets", tc.name, tc.point, tc.series)
		}
	}
}

// TestFiatTWAPPointUsesAllConstituents covers the third point endpoint,
// which shares the same fetch. Time-weighted over the merged, chronologically
// ordered population: 0.18 for [t0, t0+5m), 0.20 for [t0+5m, t0+10m), 0.19
// for [t0+10m, windowEnd=t0+1h) — a 5/5/50-minute weighting that is only
// expressible if all three constituents are present and correctly ordered.
//
//	(0.18·5 + 0.20·5 + 0.19·50) / 60 = 11.4/60 = 0.19
func TestFiatTWAPPointUsesAllConstituents(t *testing.T) {
	srv := newFiatParityServer(t)
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/twap?base=native&quote=fiat:USD"+fiatParityWindow())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("twap status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.TWAPResult `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.TradeCount != 3 {
		t.Errorf("twap trade_count = %d, want 3 (one per constituent)", env.Data.TradeCount)
	}
	if env.Data.Price != "0.1900000000" {
		t.Errorf("twap price = %q, want 0.1900000000 — (0.18·5m + 0.20·5m + 0.19·50m)/60m",
			env.Data.Price)
	}
}

// TestFiatPointConstituentReadErrorFailsClosed pins the deliberate
// fail-closed posture: a constituent whose read errors is NOT skipped.
// Skipping would silently narrow the methodology back to a subset — the
// exact defect C1-024 removes — and serve a quietly-wrong money value
// instead of an error the operator can see.
func TestFiatPointConstituentReadErrorFailsClosed(t *testing.T) {
	usdc, _ := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	xlmNative, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	directPair, _ := canonical.NewPair(xlmNative, usd)
	sdexPair, _ := canonical.NewPair(xlmNative, usdc)
	t0 := fiatParityBucketStart()

	reader := &fiatParityFailingReader{
		fiatConstituentReader: fiatConstituentReader{
			tradesByPair: map[string][]canonical.Trade{
				fiatParityPairKey(directPair): {fiatParityTrade(directPair, 2, t0.Add(5*time.Minute), 1000, 200)},
			},
		},
		failPair: fiatParityPairKey(sdexPair),
	}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD"+fiatParityWindow())
	if resp.StatusCode != http.StatusInternalServerError {
		body, _ := readAll(resp)
		t.Errorf("status = %d, want 500 — a failed constituent read must not be "+
			"silently dropped from a money answer: %s", resp.StatusCode, body)
	}
}

// TestFiatEURPointServesLikeSeries is the divergence in its most extreme
// form. The old point path was hard-gated on `quote.Code == "USD"` while the
// series path gates on `quote.Type == AssetFiat`, so for a EUR-quoted asset
// with real EURC depth the SERIES returned a full bar and the POINT returned
// a flat 404 — the same question, one answered and one refused.
//
// The unified point path resolves fiat:EUR through the same EUR backers
// (aggregate.FiatBackers("EUR") → EURC/EUROC/EUROB) the series combines.
func TestFiatEURPointServesLikeSeries(t *testing.T) {
	xlmNative, _ := canonical.ParseAsset("native")
	eurc, err := canonical.ParseAsset("crypto:EURC")
	if err != nil {
		t.Fatalf("parse crypto:EURC: %v", err)
	}
	eurcPair, _ := canonical.NewPair(xlmNative, eurc)
	t0 := fiatParityBucketStart()
	reader := &fiatConstituentReader{
		tradesByPair: map[string][]canonical.Trade{
			fiatParityPairKey(eurcPair): {fiatParityTrade(eurcPair, 4, t0.Add(time.Minute), 1000, 170)},
		},
	}
	ts := httpTestServer(t, v1.New(v1.Options{History: reader}))

	seriesResp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:EUR&interval=1h&limit=1"+fiatParityWindow())
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

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:EUR"+fiatParityWindow())
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("vwap status = %d, want 200 — the series serves this window from "+
			"the EURC constituent, so the point quote must too: %s", resp.StatusCode, body)
	}
	var env struct {
		Data v1.VWAPResult `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.Price != "0.1700000000" {
		t.Errorf("vwap price = %q, want 0.1700000000 (170/1000 from the EURC leg)", env.Data.Price)
	}
	bar := seriesEnv.Data.Intervals[0]
	seriesVWAP := new(big.Rat).Quo(mustRat(t, bar.VQuote), mustRat(t, bar.VBase))
	if mustRat(t, env.Data.Price).Cmp(seriesVWAP) != 0 {
		t.Errorf("vwap price %s != series v_quote/v_base %s", env.Data.Price, seriesVWAP.FloatString(10))
	}
}

type fiatParityFailingReader struct {
	fiatConstituentReader
	failPair string
}

func (r *fiatParityFailingReader) TradesInRange(
	ctx context.Context, pair canonical.Pair, from, to time.Time, limit int,
) ([]canonical.Trade, error) {
	if fiatParityPairKey(pair) == r.failPair {
		return nil, errFiatParityConstituent
	}
	return r.fiatConstituentReader.TradesInRange(ctx, pair, from, to, limit)
}

var errFiatParityConstituent = errors.New("constituent read failed")
