// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// CS-040 money-path: the fiat-combine point path (Server.fiatCombinedTrades)
// merges every usdPeggedConstituent into ONE slice fed to aggregate.VWAP —
// and those constituents span source scales. On-chain DEX legs
// (native/USDC-classic) stamp amounts at 7 decimals; CEX legs
// (crypto:XLM/crypto:USDT) at 8. A trade's PRICE (quote/base) is
// scale-invariant, but its WEIGHT in Σquote/Σbase is its raw base amount =
// real_volume × 10^scale, so the 8dp CEX leg is over-weighted 10× versus an
// equal-real-volume 7dp on-chain leg. The served VWAP was therefore dragged
// toward the CEX price. Server.fiatCombinedTrades now lifts every trade to the
// common (max) scale before returning, restoring the true
// real-volume-weighted mean.
//
// Fixture — one 1h bucket, two constituents of the native/fiat:USD target,
// equal REAL volume (1000 XLM each), different scales:
//
//	native/<USDC-classic>   source=sdex    (7dp)  10^10 base / 10^9  quote → 0.10
//	crypto:XLM/crypto:USDT  source=binance (8dp)  10^11 base / 1.2·10^10 quote → 0.12
//
// True equal-volume mean = 0.11. Pre-fix VWAP = (10^9 + 1.2·10^10) /
// (10^10 + 10^11) = 13/110 ≈ 0.1181818… — the 10×-over-weighted-CEX value
// (the wire renders it 0.1181818181, ratToDecimal truncating at 10 digits).

const (
	scaleNormWantVWAP   = "0.1100000000" // real-volume-weighted midpoint (fixed)
	scaleNormPreFixVWAP = "0.1181818181" // 10×-over-weighted CEX (un-normalized)
	// Both legs are 1000 XLM; normalized to the common 8dp scale each is
	// 10^11, so the combined base volume is 2·10^11.
	scaleNormWantBaseVolume = "200000000000"
)

func scaleNormBucketStart() time.Time { return time.Date(2026, 5, 6, 7, 0, 0, 0, time.UTC) }

func scaleNormWindow() string {
	t0 := scaleNormBucketStart()
	return "&from=" + t0.Format(time.RFC3339) + "&to=" + t0.Add(time.Hour).Format(time.RFC3339)
}

// scaleNormTrade builds a trade at an explicit source (so its scale is what
// external.Lookup resolves) with big-int amounts.
func scaleNormTrade(source string, pair canonical.Pair, ledger uint32, ts time.Time, base, quote *big.Int) canonical.Trade {
	return canonical.Trade{
		Source:      source,
		Ledger:      ledger,
		TxHash:      "00000000000000000000000000000000000000000000000000000000000000ab",
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(base),
		QuoteAmount: canonical.NewAmount(quote),
	}
}

func scaleNormPow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// TestFiatVWAPMixedScaleNormalized is the CS-040 regression. It goes through
// the real /v1/vwap handler over a mixed-scale constituent set and asserts the
// corrected, real-volume-weighted price.
//
// Non-vacuous by construction: reverting the fix (dropping the
// NormalizeAmountScale call in fiatCombinedTrades) makes this serve
// scaleNormPreFixVWAP and the assertion fails; it uses only pre-existing
// symbols, so it compiles against the un-fixed tree.
func TestFiatVWAPMixedScaleNormalized(t *testing.T) {
	usdc, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	xlmNative, _ := canonical.ParseAsset("native")
	xlmTicker, _ := canonical.ParseAsset("crypto:XLM")
	usdt, _ := canonical.ParseAsset("crypto:USDT")

	onchainPair, _ := canonical.NewPair(xlmNative, usdc) // 7dp, source sdex
	cexPair, _ := canonical.NewPair(xlmTicker, usdt)     // 8dp, source binance
	t0 := scaleNormBucketStart()

	reader := &fiatConstituentReader{
		tradesByPair: map[string][]canonical.Trade{
			// 1000 XLM @0.10 on-chain 7dp: 10^10 base, 10^9 quote.
			fiatParityPairKey(onchainPair): {scaleNormTrade("sdex", onchainPair, 1, t0,
				new(big.Int).Mul(big.NewInt(1000), scaleNormPow10(7)),
				new(big.Int).Mul(big.NewInt(100), scaleNormPow10(7)))},
			// 1000 XLM @0.12 CEX 8dp: 10^11 base, 1.2·10^10 quote.
			fiatParityPairKey(cexPair): {scaleNormTrade("binance", cexPair, 2, t0.Add(5*time.Minute),
				new(big.Int).Mul(big.NewInt(1000), scaleNormPow10(8)),
				new(big.Int).Mul(big.NewInt(120), scaleNormPow10(8)))},
		},
	}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD"+scaleNormWindow())
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("vwap status = %d, want 200: %s", resp.StatusCode, body)
	}
	var env struct {
		Data v1.VWAPResult `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.TradeCount != 2 {
		t.Fatalf("trade_count = %d, want 2 (one on-chain, one CEX constituent)", env.Data.TradeCount)
	}
	if env.Data.Price != scaleNormWantVWAP {
		t.Errorf("vwap price = %q, want %q (equal-real-volume midpoint of 0.10 on-chain "+
			"and 0.12 CEX); %q is the pre-fix value with the 8dp CEX leg over-weighted 10×",
			env.Data.Price, scaleNormWantVWAP, scaleNormPreFixVWAP)
	}
	// Volume is now reported at the common 8dp scale (both legs 1000 XLM →
	// 2000 XLM total). Pre-fix this summed cross-scale integers
	// (10^10 + 10^11 = 1.1·10^11), a physically meaningless quantity.
	if env.Data.BaseVolume != scaleNormWantBaseVolume {
		t.Errorf("vwap base_volume = %q, want %q (2000 XLM at the common 8dp scale)",
			env.Data.BaseVolume, scaleNormWantBaseVolume)
	}
}

// TestFiatVWAPUniformOnChainUnchanged is the byte-identical guard at the
// handler layer: an all-on-chain (uniform 7dp) constituent set must serve the
// exact same VWAP with the normalization in place as without it. Two 7dp
// constituents, 0.10 and 0.20 on equal 1000-XLM base → 0.15 exactly, and the
// raw base volume (2·10^10) is preserved unchanged (no rescale to 8dp).
func TestFiatVWAPUniformOnChainUnchanged(t *testing.T) {
	usdc, _ := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	usdcCrypto, _ := canonical.ParseAsset("crypto:USDC")
	xlmNative, _ := canonical.ParseAsset("native")

	pairA, _ := canonical.NewPair(xlmNative, usdc)       // native/USDC-classic (peg)
	pairB, _ := canonical.NewPair(xlmNative, usdcCrypto) // native/crypto:USDC (backer)
	t0 := scaleNormBucketStart()

	reader := &fiatConstituentReader{
		tradesByPair: map[string][]canonical.Trade{
			// 1000 XLM @0.10, 7dp.
			fiatParityPairKey(pairA): {scaleNormTrade("sdex", pairA, 1, t0,
				new(big.Int).Mul(big.NewInt(1000), scaleNormPow10(7)),
				new(big.Int).Mul(big.NewInt(100), scaleNormPow10(7)))},
			// 1000 XLM @0.20, 7dp.
			fiatParityPairKey(pairB): {scaleNormTrade("soroswap", pairB, 2, t0.Add(time.Minute),
				new(big.Int).Mul(big.NewInt(1000), scaleNormPow10(7)),
				new(big.Int).Mul(big.NewInt(200), scaleNormPow10(7)))},
		},
	}
	ts := httpTestServer(t, v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdc},
	}))

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD"+scaleNormWindow())
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("vwap status = %d, want 200: %s", resp.StatusCode, body)
	}
	var env struct {
		Data v1.VWAPResult `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.Price != "0.1500000000" {
		t.Errorf("uniform-7dp vwap price = %q, want 0.1500000000 (0.10 and 0.20 on equal "+
			"volume) — normalization must be a no-op for a single-scale window", env.Data.Price)
	}
	if env.Data.BaseVolume != "20000000000" {
		t.Errorf("uniform-7dp vwap base_volume = %q, want 20000000000 (2·10^10 raw, "+
			"unchanged — a uniform window must not be rescaled)", env.Data.BaseVolume)
	}
}

// TestPriceTipMixedScaleNormalized pins the same fix on the live /v1/price/tip
// surface. tipWindowVWAP MERGES the XLM alias pairs — on-chain native/<q>
// (7dp) with CEX crypto:XLM/<q> (8dp) — into one slice before aggregate.VWAP,
// so an asset cross-listed on both venues had its tip price dragged toward the
// finer-scaled CEX leg. Same fixture as the fiat case: 1000 XLM @0.10 on-chain
// + 1000 XLM @0.12 CEX → true tip 0.11.
//
// Non-vacuous: reverting the NormalizeAmountScale call in tipWindowVWAP serves
// 0.1181818181 and fails this; it uses only pre-existing symbols.
func TestPriceTipMixedScaleNormalized(t *testing.T) {
	xlmNative, _ := canonical.ParseAsset("native")
	xlmTicker, _ := canonical.ParseAsset("crypto:XLM")
	usdt, _ := canonical.ParseAsset("crypto:USDT")

	onchainPair, _ := canonical.NewPair(xlmNative, usdt) // 7dp, source sdex
	cexPair, _ := canonical.NewPair(xlmTicker, usdt)     // 8dp, source binance
	now := time.Now().UTC()

	reader := &fiatConstituentReader{
		tradesByPair: map[string][]canonical.Trade{
			fiatParityPairKey(onchainPair): {scaleNormTrade("sdex", onchainPair, 1, now.Add(-2*time.Second),
				new(big.Int).Mul(big.NewInt(1000), scaleNormPow10(7)),
				new(big.Int).Mul(big.NewInt(100), scaleNormPow10(7)))},
			fiatParityPairKey(cexPair): {scaleNormTrade("binance", cexPair, 2, now.Add(-1*time.Second),
				new(big.Int).Mul(big.NewInt(1000), scaleNormPow10(8)),
				new(big.Int).Mul(big.NewInt(120), scaleNormPow10(8)))},
		},
	}
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}, History: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=crypto:XLM&quote=crypto:USDT&window_seconds=5")
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("tip status = %d, want 200: %s", resp.StatusCode, body)
	}
	var env struct {
		Data struct {
			Price     string `json:"price"`
			PriceType string `json:"price_type"`
		} `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.PriceType != "vwap" {
		t.Fatalf("price_type = %q, want vwap (the rolling-window path must be the one exercised)", env.Data.PriceType)
	}
	if env.Data.Price != scaleNormWantVWAP {
		t.Errorf("tip price = %q, want %q (equal-real-volume midpoint); %q is the pre-fix "+
			"value with the 8dp CEX leg over-weighted 10×",
			env.Data.Price, scaleNormWantVWAP, scaleNormPreFixVWAP)
	}
}

// ── the SERIES arm of the same defect ───────────────────────────────────
//
// Everything above tests paths that merge raw TRADES, each row carrying its
// own Source. Server.ohlcSeriesFiatCombined merges continuous-aggregate
// BARS instead, over the identical constituent set
// (Server.usdPeggedConstituents), and a bar is a sum of smallest-unit
// amounts in units only its contributing venues identify. Reading that
// column and lifting every bar to one common scale is what these pin.
//
// Same fixture as TestFiatVWAPMixedScaleNormalized, so the point answer and
// the series answer are computed from one population and must agree:
//
//	native/<USDC classic>   sdex    (7dp)  10^10 base / 10^9  quote → 0.10
//	crypto:XLM/crypto:USDT  binance (8dp)  10^11 base / 1.2·10^10 quote → 0.12
//
// Both legs are 1000 XLM, so the true bar is 2000 XLM at the common 8dp
// scale with a volume-weighted open/close of exactly 0.11.
const (
	scaleNormWantSeriesVBase  = "200000000000" // 2000 XLM at the common 8dp scale
	scaleNormWantSeriesVQuote = "22000000000"  // 220 USD at the same scale
	// Pre-fix: 10^10 + 10^11, the sdex leg counted at a tenth of its real
	// volume because its 7dp integers were added to 8dp ones unchanged.
	scaleNormPreFixSeriesVBase = "110000000000"
)

// mixedScaleFiatServer wires the two-constituent, two-scale fixture behind
// the real /v1/ohlc and /v1/vwap handlers.
func mixedScaleFiatServer(t *testing.T) *v1.Server {
	t.Helper()
	usdc, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	xlmNative, _ := canonical.ParseAsset("native")
	xlmTicker, _ := canonical.ParseAsset("crypto:XLM")
	usdt, _ := canonical.ParseAsset("crypto:USDT")

	onchainPair, _ := canonical.NewPair(xlmNative, usdc)
	cexPair, _ := canonical.NewPair(xlmTicker, usdt)
	t0 := scaleNormBucketStart()

	reader := &fiatConstituentReader{
		tradesByPair: map[string][]canonical.Trade{
			fiatParityPairKey(onchainPair): {scaleNormTrade("sdex", onchainPair, 1, t0,
				new(big.Int).Mul(big.NewInt(1000), scaleNormPow10(7)),
				new(big.Int).Mul(big.NewInt(100), scaleNormPow10(7)))},
			fiatParityPairKey(cexPair): {scaleNormTrade("binance", cexPair, 2, t0.Add(5*time.Minute),
				new(big.Int).Mul(big.NewInt(1000), scaleNormPow10(8)),
				new(big.Int).Mul(big.NewInt(120), scaleNormPow10(8)))},
		},
	}
	return v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}})
}

// fetchMixedScaleSeriesBar pulls the single combined 1h bar over the
// mixed-scale window.
func fetchMixedScaleSeriesBar(t *testing.T, base string) v1.OHLCSeriesBar {
	t.Helper()
	resp := mustGet(t, base+"/v1/ohlc?base=native&quote=fiat:USD&interval=1h&limit=1"+scaleNormWindow())
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("series status = %d, want 200: %s", resp.StatusCode, body)
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

// TestFiatSeriesMixedScaleNormalized is the CS-040 regression on the series
// arm. It goes through the real /v1/ohlc?interval= handler over a
// constituent set that genuinely spans two scales and asserts the corrected
// bar, value by value.
//
// Non-vacuous: against the un-fixed combine — which sums each constituent
// bar's raw v_base straight into one total — this serves v_base
// 110000000000 and a volume-weighted open/close of 0.1181818181, the 7dp
// leg counted at a tenth of the volume it traded.
func TestFiatSeriesMixedScaleNormalized(t *testing.T) {
	ts := httpTestServer(t, mixedScaleFiatServer(t))
	bar := fetchMixedScaleSeriesBar(t, ts.URL)

	if bar.VBase != scaleNormWantSeriesVBase {
		t.Errorf("series v_base = %q, want %q (1000 XLM on-chain + 1000 XLM CEX at the "+
			"common 8dp scale); %q is the pre-fix cross-scale sum, which counts the "+
			"7dp leg at a tenth of its real volume",
			bar.VBase, scaleNormWantSeriesVBase, scaleNormPreFixSeriesVBase)
	}
	// v_quote renders at ohlcPriceDigits on this surface, so compare it as
	// a number — the same rule TestFiatVWAPPointMatchesSeries applies.
	if mustRat(t, bar.VQuote).Cmp(mustRat(t, scaleNormWantSeriesVQuote)) != 0 {
		t.Errorf("series v_quote = %q, want %q", bar.VQuote, scaleNormWantSeriesVQuote)
	}
	// Open and close are base-volume weighted across constituents, so the
	// 10x mis-weight lands on them too: 0.10 and 0.12 on equal real volume
	// is 0.11, not the 0.1181818181 an un-lifted weight produces.
	for _, tc := range []struct{ name, got string }{{"open", bar.O}, {"close", bar.C}} {
		if tc.got != scaleNormWantVWAP {
			t.Errorf("series %s = %q, want %q (equal-real-volume midpoint of 0.10 "+
				"on-chain and 0.12 CEX); %q is the pre-fix value",
				tc.name, tc.got, scaleNormWantVWAP, scaleNormPreFixVWAP)
		}
	}
	// The bar's own Σquote/Σbase — the number a client recomputes from the
	// two volume fields — must be the true real-volume-weighted price.
	vq, ok := new(big.Rat).SetString(bar.VQuote)
	if !ok {
		t.Fatalf("unparseable v_quote %q", bar.VQuote)
	}
	vb, ok := new(big.Rat).SetString(bar.VBase)
	if !ok {
		t.Fatalf("unparseable v_base %q", bar.VBase)
	}
	if got := new(big.Rat).Quo(vq, vb); got.Cmp(big.NewRat(11, 100)) != 0 {
		t.Errorf("series v_quote/v_base = %s, want 0.11 exactly", got.FloatString(10))
	}
	// Extremes and the count are ratios and counts, so the lift must leave
	// them exactly where they were.
	if bar.H != "0.1200000000" || bar.L != "0.1000000000" {
		t.Errorf("series high/low = %q/%q, want 0.1200000000/0.1000000000 — a scale "+
			"lift multiplies both legs of a bar and must not move a price", bar.H, bar.L)
	}
	if bar.N != 2 {
		t.Errorf("series n = %d, want 2 (one trade per constituent)", bar.N)
	}
}

// TestFiatMixedScalePointMatchesSeries is the C1-024 invariant under the
// condition that makes it bite. TestFiatVWAPPointMatchesSeries pins point
// against series too, but every trade in its fixture is stamped `sdex`, so
// both sides are single-scale and the invariant holds whether or not the
// series lifts anything. Give the two constituents different scales and the
// un-fixed series answers 0.1181818181 on 110000000000 base while the point
// path — which has normalised since CS-040 — answers 0.11 on 200000000000:
// two surfaces, one question, two populations, which is the exact defect
// C1-024 exists to forbid.
func TestFiatMixedScalePointMatchesSeries(t *testing.T) {
	ts := httpTestServer(t, mixedScaleFiatServer(t))
	bar := fetchMixedScaleSeriesBar(t, ts.URL)

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD"+scaleNormWindow())
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("vwap status = %d, want 200: %s", resp.StatusCode, body)
	}
	var env struct {
		Data v1.VWAPResult `json:"data"`
	}
	mustDecode(t, resp, &env)

	if mustRat(t, env.Data.BaseVolume).Cmp(mustRat(t, bar.VBase)) != 0 {
		t.Errorf("point base_volume %q != series v_base %q — the two surfaces are "+
			"weighting one population at two different scales",
			env.Data.BaseVolume, bar.VBase)
	}
	if mustRat(t, env.Data.QuoteVolume).Cmp(mustRat(t, bar.VQuote)) != 0 {
		t.Errorf("point quote_volume %q != series v_quote %q",
			env.Data.QuoteVolume, bar.VQuote)
	}
	seriesVWAP := new(big.Rat).Quo(
		mustRat(t, bar.VQuote), mustRat(t, bar.VBase))
	if got := mustRat(t, env.Data.Price); got.Cmp(seriesVWAP) != 0 {
		t.Errorf("point price %s != series v_quote/v_base %s — a point quote that "+
			"cannot be reconciled against the series it is published alongside",
			env.Data.Price, seriesVWAP.FloatString(10))
	}
}
