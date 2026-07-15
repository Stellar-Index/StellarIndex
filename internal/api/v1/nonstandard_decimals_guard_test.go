package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// flaggedAsset is a fixed C-strkey used across the guard tests — the SAME
// contract id named in the runbook / migration 0093 header (harmless as a
// test fixture: it's a real on-chain public contract id, not a secret).
const flaggedAsset = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"

// nonstandardDecimalsCacheWith builds a *v1.NonstandardDecimalsCache
// pre-populated (via one Refresh) with a single flagged asset.
func nonstandardDecimalsCacheWith(t *testing.T, asset string, decimals int) *v1.NonstandardDecimalsCache {
	t.Helper()
	reader := &stubNonstandardDecimalsReader{
		rows: []timescale.NonstandardDecimalsAsset{{Asset: asset, Decimals: decimals, Source: "aquarius"}},
	}
	c := v1.NewNonstandardDecimalsCache(reader, nil)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("cache refresh: %v", err)
	}
	return c
}

// TestPrice_NonstandardDecimals_NormalizesFlaggedBaseLeg proves /v1/price
// no longer declines a confirmed non-7-decimal base leg — the closed-1m-
// bucket read (the last /v1/price path still declining after v0.12.0)
// now serves the AdjustPrice-corrected value. The stub snapshot carries
// the RAW CAGG ratio 41.32 (the runbook's real CC2RB… incident value,
// decimals()=9 vs USDC's 7): K = 10^(9−7) = 100 → true price 4132.
func TestPrice_NonstandardDecimals_NormalizesFlaggedBaseLeg(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	key := flaggedAsset + "/fiat:USD"
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{key: {
			AssetID:       flaggedAsset,
			Quote:         "fiat:USD",
			Price:         "41.32",
			PriceType:     "vwap",
			ObservedAt:    time.Unix(1745000000, 0).UTC(),
			WindowSeconds: 60,
		}},
		sources: map[string][]string{key: {"aquarius"}},
	}
	srv := v1.New(v1.Options{
		Prices:              reader,
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (closed-bucket read is normalized, not declined)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"4132.0000000000"`) {
		t.Errorf("body missing normalized price 4132.0000000000: %s", body)
	}
}

// TestPrice_NonstandardDecimals_NormalizesFlaggedQuoteLeg proves the
// quote leg scales the other way: K = 10^(7−9) = 1/100.
func TestPrice_NonstandardDecimals_NormalizesFlaggedQuoteLeg(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	key := "native/" + flaggedAsset
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{key: {
			AssetID:       "native",
			Quote:         flaggedAsset,
			Price:         "41.32",
			PriceType:     "vwap",
			ObservedAt:    time.Unix(1745000000, 0).UTC(),
			WindowSeconds: 60,
		}},
		sources: map[string][]string{key: {"aquarius"}},
	}
	srv := v1.New(v1.Options{
		Prices:              reader,
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote="+flaggedAsset)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (closed-bucket read is normalized, not declined)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.4132000000"`) {
		t.Errorf("body missing normalized price 0.4132000000: %s", body)
	}
}

// TestPrice_NonstandardDecimals_UnflaggedPairServesNormally proves the
// guard is NOT a false-positive trap: with the cache wired but the
// requested pair clean, /v1/price serves exactly as it would with no
// guard configured at all.
func TestPrice_NonstandardDecimals_UnflaggedPairServesNormally(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	snap := v1.PriceSnapshot{
		AssetID:    "native",
		Quote:      "fiat:USD",
		Price:      "0.1242",
		PriceType:  "last_trade",
		ObservedAt: time.Unix(1745000000, 0).UTC(),
	}
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{"native/fiat:USD": snap},
		sources:   map[string][]string{"native/fiat:USD": {"sdex"}},
	}
	srv := v1.New(v1.Options{Prices: reader, NonstandardDecimals: cache})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unflagged pair must serve normally)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.1242"`) {
		t.Errorf("body missing expected price: %s", body)
	}
}

// TestPrice_NonstandardDecimals_NoCacheWired_ServesNormally proves a
// deployment that never wires NonstandardDecimals (the pre-guard shape,
// and every deployment until the cache is configured) is unaffected —
// declineIfNonstandardDecimals must be a pure no-op when s.nonstandardDecimals
// is nil.
func TestPrice_NonstandardDecimals_NoCacheWired_ServesNormally(t *testing.T) {
	snap := v1.PriceSnapshot{AssetID: "native", Quote: "fiat:USD", Price: "0.5", PriceType: "last_trade"}
	reader := &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{"native/fiat:USD": snap}}
	srv := v1.New(v1.Options{Prices: reader}) // NonstandardDecimals left nil
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestVWAP_NonstandardDecimals_Normalizes proves /v1/vwap no longer
// declines a confirmed non-7-decimals pair — since 2026-07-10 it computes
// entirely from raw trades at query time, so the fix is to serve the
// CORRECTED price (aggregate.AdjustPrice) rather than 422. See
// docs/operations/runbooks/dex-nonstandard-decimals.md "Root cause
// analysis". flaggedAsset is declared decimals()=18 here; base_amount =
// 2.5*10^18, quote_amount = 1.242*10^7 (USDC, 7dp) → true price 0.4968,
// the SAME golden case as internal/aggregate's TestAdjustPrice_Golden18DecimalToken.
func TestVWAP_NonstandardDecimals_Normalizes(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 18)
	baseAmount, ok := new(big.Int).SetString("2500000000000000000", 10)
	if !ok {
		t.Fatal("bad big.Int literal")
	}
	xlmUSD, err := canonical.ParseAsset(flaggedAsset)
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	usd, _ := canonical.ParseAsset("fiat:USD")
	pair, err := canonical.NewPair(xlmUSD, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	trade := canonical.Trade{
		Source:      "aquarius",
		Ledger:      1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		Timestamp:   time.Unix(1_772_000_000, 0).UTC(),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(baseAmount),
		QuoteAmount: canonical.NewAmount(big.NewInt(12_420_000)),
	}
	srv := v1.New(v1.Options{
		History:             &stubHistoryReader{trades: []canonical.Trade{trade}},
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/vwap?base="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (query-time compute is normalized, not declined)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.4968000000"`) {
		t.Errorf("body missing normalized price 0.4968000000: %s", body)
	}
}

// TestHistory_NonstandardDecimals_Normalizes proves /v1/history no longer
// declines — it reads exclusively from raw trades (TradesInRangeAfter),
// so the per-row Price field is corrected instead.
func TestHistory_NonstandardDecimals_Normalizes(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	xlmUSD, err := canonical.ParseAsset(flaggedAsset)
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	usd, _ := canonical.ParseAsset("fiat:USD")
	pair, err := canonical.NewPair(xlmUSD, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	// base 100 * 10^9 (9dp), quote 250 * 10^7 (7dp fiat) → true price
	// 250/100 = 2.5.
	trade := canonical.Trade{
		Source:      "aquarius",
		Ledger:      1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		Timestamp:   time.Unix(1_772_000_000, 0).UTC(),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(100_000_000_000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(2_500_000_000)),
	}
	reader := &stubHistoryReader{trades: []canonical.Trade{trade}}
	srv := v1.New(v1.Options{
		History:             reader,
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/history?base="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (raw-trade path is normalized, not declined)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"2.5000000000"`) {
		t.Errorf("body missing normalized price 2.5000000000: %s", body)
	}
}

// TestOHLC_NonstandardDecimals proves BOTH modes normalize now:
// single-bar mode (raw trades, query-time — normalized since v0.12.0)
// and interval= series mode (prices_<n> CAGG — normalized 2026-07-10,
// closing the deferred tail; previously declined 422).
func TestOHLC_NonstandardDecimals(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	xlmUSD, err := canonical.ParseAsset(flaggedAsset)
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	usd, _ := canonical.ParseAsset("fiat:USD")
	pair, err := canonical.NewPair(xlmUSD, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	trade := canonical.Trade{
		Source:      "aquarius",
		Ledger:      1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		Timestamp:   time.Unix(1_772_000_000, 0).UTC(),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(100_000_000_000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(2_500_000_000)),
	}
	srv := v1.New(v1.Options{
		History:             &stubHistoryReader{trades: []canonical.Trade{trade}},
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/ohlc?base="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("single-bar: status = %d, want 200 (normalized, not declined)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"open":"2.5000000000"`) {
		t.Errorf("single-bar body missing normalized open 2.5000000000: %s", body)
	}
}

// classicUSDC is a 7dp classic quote leg for series-mode fixtures — a
// non-fiat quote takes ohlcSeriesWithAliases' first-hit path (no
// fiat-combine fan-out), so the stub's bars map 1:1 onto the response.
const classicUSDC = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// TestOHLCSeries_NonstandardDecimals_NormalizesBarsNotVolumes pins the
// series-mode contract closed on 2026-07-10:
//
//   - o/h/l/c: the raw prices_<n> CAGG ratio × K (K = 10^(9−7) = 100
//     for a 9dp base vs a 7dp classic quote) — the same factor the
//     single-bar path applies, since every bar shares the pair.
//   - v_base/v_quote: UNCHANGED. The CAGG's volume columns are raw
//     smallest-unit sums in each asset's OWN declared decimals
//     (migration 0002: volume = Σ(base_amount)), and the wire contract
//     (OHLCBar/VWAPResult doc: "in the asset's smallest unit") promises
//     exactly that — same precedent as /v1/history, which serves raw
//     base_amount/quote_amount plus base_decimals/quote_decimals
//     metadata and never rescales amounts. Scaling volumes by 10^(7−dec)
//     would silently break the smallest-unit contract.
func TestOHLCSeries_NonstandardDecimals_NormalizesBarsNotVolumes(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	reader := &stubHistoryReader{ohlcBars: []v1.OHLCSeriesBar{{
		T: t0,
		// Raw CAGG ratios (quote_amount/base_amount on smallest units):
		// true price is 100x these for a 9dp base vs 7dp quote.
		O: "2.5", H: "3", L: "2", C: "2.5",
		// Raw smallest-unit sums: 10^11 base units at 9dp = 100 tokens;
		// 2.5*10^9 quote units at 7dp = 250 USDC.
		VBase: "100000000000", VQuote: "2500000000",
		N: 4,
	}}}
	srv := v1.New(v1.Options{
		History:             reader,
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/ohlc?base="+flaggedAsset+"&quote="+classicUSDC+"&interval=1h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("series mode: status = %d, want 200 (CAGG read is normalized, not declined)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	for _, want := range []string{
		`"o":"250.0000000000"`,
		`"h":"300.0000000000"`,
		`"l":"200.0000000000"`,
		`"c":"250.0000000000"`,
		// Volumes byte-identical to the raw CAGG values.
		`"v_base":"100000000000"`,
		`"v_quote":"2500000000"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("series body missing %q: %s", want, body)
		}
	}
}

// TestOHLCSeries_NonstandardDecimals_7dpByteIdentical proves wiring the
// cache does NOT reformat an unflagged (7dp/7dp) pair's bars — the CAGG's
// NUMERIC::text strings must pass through byte-identical (AdjustPrice's
// no-op contract), not get re-rendered at 10 fixed digits.
func TestOHLCSeries_NonstandardDecimals_7dpByteIdentical(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9) // flagged asset NOT in this pair
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	reader := &stubHistoryReader{ohlcBars: []v1.OHLCSeriesBar{{
		T: t0, O: "0.16", H: "0.17", L: "0.15", C: "0.165",
		VBase: "1000", VQuote: "165", N: 4,
	}}}
	srv := v1.New(v1.Options{
		History:             reader,
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote="+classicUSDC+"&interval=1h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	for _, want := range []string{
		`"o":"0.16"`, `"h":"0.17"`, `"l":"0.15"`, `"c":"0.165"`,
		`"v_base":"1000"`, `"v_quote":"165"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("7dp bars must be byte-identical; body missing %q: %s", want, body)
		}
	}
}

// TestChart_NonstandardDecimals_NormalizesPriceNotVolumeUSD pins the
// /v1/chart contract closed on 2026-07-10 (this endpoint was never
// guarded at all — it served the raw prices_<gran> ratio):
//
//   - each point's `p`: raw CAGG ratio × K (10^(9−7) = 100 here).
//   - each point's `v_usd`: UNCHANGED — prices_<gran>.volume_usd is
//     Σ(usd_volume) (migration 0002), already USD-denominated at
//     trade-valuation time and invariant to the pair's decimals split.
func TestChart_NonstandardDecimals_NormalizesPriceNotVolumeUSD(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	vusd := "1234.56"
	reader := &stubHistoryReader{points: []v1.HistoryPoint{{
		Bucket: t0, VWAP: "41.32", VolumeUSD: &vusd,
	}}}
	srv := v1.New(v1.Options{
		History:             reader,
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/chart?asset="+flaggedAsset+"&quote="+classicUSDC+"&timeframe=24h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"p":"4132.0000000000"`) {
		t.Errorf("chart body missing normalized point price 4132.0000000000: %s", body)
	}
	if !strings.Contains(body, `"v_usd":"1234.56"`) {
		t.Errorf("chart v_usd must be untouched (already USD-anchored): %s", body)
	}
}

// TestChart_NonstandardDecimals_7dpByteIdentical — wiring the cache must
// not reformat an unflagged pair's points.
func TestChart_NonstandardDecimals_7dpByteIdentical(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	reader := &stubHistoryReader{points: []v1.HistoryPoint{{Bucket: t0, VWAP: "0.1242"}}}
	srv := v1.New(v1.Options{
		History:             reader,
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/chart?asset=native&quote="+classicUSDC+"&timeframe=24h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"p":"0.1242"`) {
		t.Errorf("7dp chart points must be byte-identical; body: %s", body)
	}
}

// TestMarkets_NonstandardDecimals_NormalizesLastPrice — /v1/markets'
// last_price (prices_1d/prices_1m raw ratio) was never guarded; now
// corrected per row. The unflagged sibling row must stay byte-identical.
func TestMarkets_NonstandardDecimals_NormalizesLastPrice(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	flaggedPrice := "41.32"
	cleanPrice := "0.1242"
	reader := &stubMarketsReader{pairs: []v1.Market{
		{Base: flaggedAsset, Quote: classicUSDC, LastPrice: &flaggedPrice},
		{Base: "native", Quote: classicUSDC, LastPrice: &cleanPrice},
	}}
	srv := v1.New(v1.Options{Markets: reader, NonstandardDecimals: cache})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/markets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"last_price":"4132.0000000000"`) {
		t.Errorf("flagged market row not normalized: %s", body)
	}
	if !strings.Contains(body, `"last_price":"0.1242"`) {
		t.Errorf("7dp market row must be byte-identical: %s", body)
	}
}

// TestPools_NonstandardDecimals_NormalizesLastPrice — same fix on
// /v1/pools (pools_per_source_1h bucket_last_price).
func TestPools_NonstandardDecimals_NormalizesLastPrice(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	flaggedPrice := "41.32"
	reader := &stubMarketsReader{pairs: []v1.Market{
		{Base: flaggedAsset, Quote: classicUSDC, LastPrice: &flaggedPrice},
	}}
	srv := v1.New(v1.Options{Markets: reader, NonstandardDecimals: cache})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/pools")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"last_price":"4132.0000000000"`) {
		t.Errorf("flagged pool row not normalized: %s", body)
	}
}

// TestPairs_NonstandardDecimals_NormalizesLastPrice — /v1/pairs shares
// the same Market wire shape; PairMarket's last_price is the same raw
// prices_1m ratio.
func TestPairs_NonstandardDecimals_NormalizesLastPrice(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	flaggedPrice := "41.32"
	reader := &stubMarketsReader{
		pair:      v1.Market{Base: flaggedAsset, Quote: classicUSDC, LastPrice: &flaggedPrice},
		pairFound: true,
	}
	srv := v1.New(v1.Options{Markets: reader, NonstandardDecimals: cache})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/pairs?base="+flaggedAsset+"&quote="+classicUSDC)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"last_price":"4132.0000000000"`) {
		t.Errorf("flagged pair row not normalized: %s", body)
	}
}

// TestPriceBatch_NonstandardDecimals_Normalizes — the batch endpoint's
// direct closed-bucket read goes through the same normalization as the
// single-asset handler.
func TestPriceBatch_NonstandardDecimals_Normalizes(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	key := flaggedAsset + "/fiat:USD"
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{key: {
			AssetID: flaggedAsset, Quote: "fiat:USD", Price: "41.32",
			PriceType: "vwap", ObservedAt: time.Unix(1745000000, 0).UTC(),
		}},
		sources: map[string][]string{key: {"aquarius"}},
	}
	srv := v1.New(v1.Options{Prices: reader, NonstandardDecimals: cache})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids="+flaggedAsset)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"4132.0000000000"`) {
		t.Errorf("batch row not normalized: %s", body)
	}
}

// TestOraclePrices_NonstandardDecimals_Normalizes — the SEP-40
// prices(asset, records) passthrough reads the same raw prices_1m CAGG
// (RecentClosedSnapshots) and was neither guarded nor normalized; fixed
// alongside the /v1/price closed-bucket path (2026-07-10).
func TestOraclePrices_NonstandardDecimals_Normalizes(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	key := flaggedAsset + "/fiat:USD"
	reader := &stubPriceReader{
		recent: map[string][]v1.PriceSnapshot{key: {{
			AssetID: flaggedAsset, Quote: "fiat:USD", Price: "41.32",
			PriceType: "vwap", ObservedAt: time.Unix(1745000000, 0).UTC(),
		}}},
	}
	srv := v1.New(v1.Options{Prices: reader, NonstandardDecimals: cache})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/prices?asset="+flaggedAsset)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"4132.0000000000"`) {
		t.Errorf("oracle prices row not normalized: %s", body)
	}
}

// TestTWAP_NonstandardDecimals_Normalizes proves /v1/twap no longer
// declines — same rationale as /v1/vwap.
func TestTWAP_NonstandardDecimals_Normalizes(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	xlmUSD, err := canonical.ParseAsset(flaggedAsset)
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	usd, _ := canonical.ParseAsset("fiat:USD")
	pair, err := canonical.NewPair(xlmUSD, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	trade := canonical.Trade{
		Source:      "aquarius",
		Ledger:      1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		Timestamp:   time.Now().Add(-time.Minute),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(100_000_000_000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(2_500_000_000)),
	}
	srv := v1.New(v1.Options{
		History:             &stubHistoryReader{trades: []canonical.Trade{trade}},
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/twap?base="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (query-time compute is normalized, not declined)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"2.5000000000"`) {
		t.Errorf("body missing normalized price 2.5000000000: %s", body)
	}
}
