package v1_test

import (
	"context"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

func mkVWAPTrade(base, quote int64) canonical.Trade {
	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	pair, _ := canonical.NewPair(xlm, usd)
	return canonical.Trade{
		Source: "soroswap", Ledger: 1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		OpIndex:     0,
		Timestamp:   time.Unix(1_772_000_000, 0).UTC(),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(base)),
		QuoteAmount: canonical.NewAmount(big.NewInt(quote)),
	}
}

func TestVWAP_503WhenReaderNil(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestVWAP_404WhenNoTrades(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{trades: nil}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestVWAP_ComputesVolumeWeightedPrice(t *testing.T) {
	// VWAP = Σ(Qi) / Σ(Bi). Two trades: 20@2 + 100@3 = total
	// quote 340 / base 120 = 17/6 ≈ 2.8333333333 (10 digits, floor).
	reader := &stubHistoryReader{
		trades: []canonical.Trade{
			mkVWAPTrade(20, 40),
			mkVWAPTrade(100, 300),
		},
	}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.VWAPResult `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.Price != "2.8333333333" {
		t.Errorf("Price = %q, want 2.8333333333 (17/6 truncated to 10 digits)", env.Data.Price)
	}
	if env.Data.BaseVolume != "120" {
		t.Errorf("BaseVolume = %q, want 120", env.Data.BaseVolume)
	}
	if env.Data.QuoteVolume != "340" {
		t.Errorf("QuoteVolume = %q, want 340", env.Data.QuoteVolume)
	}
	if env.Data.TradeCount != 2 {
		t.Errorf("TradeCount = %d, want 2", env.Data.TradeCount)
	}
	if env.Data.OutliersFiltered != 0 {
		t.Errorf("OutliersFiltered = %d, want 0 (no sigma passed)", env.Data.OutliersFiltered)
	}
}

func TestVWAP_AppliesOutlierFilter(t *testing.T) {
	// 20 baseline trades around price 100, plus one at 10000.
	// sigma=3 should drop the outlier.
	baseline := []int64{
		100, 101, 99, 100, 102, 98, 101, 100, 99, 100,
		101, 100, 99, 101, 100, 102, 99, 100, 101, 100,
	}
	trades := make([]canonical.Trade, 0, len(baseline)+1)
	for _, p := range baseline {
		trades = append(trades, mkVWAPTrade(1, p))
	}
	trades = append(trades, mkVWAPTrade(1, 10_000))

	reader := &stubHistoryReader{trades: trades}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD&outlier_sigma=3")
	var env struct {
		Data v1.VWAPResult `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.OutliersFiltered != 1 {
		t.Errorf("OutliersFiltered = %d, want 1", env.Data.OutliersFiltered)
	}
	if env.Data.TradeCount != 20 {
		t.Errorf("TradeCount = %d, want 20 (outlier dropped)", env.Data.TradeCount)
	}
	// Baseline mean is ~100; the outlier at 10000 would skew the
	// unfiltered VWAP to ~571. Filtered VWAP should be ~100.
	if env.Data.Price[:3] != "100" {
		t.Errorf("filtered Price = %q, want ~100 prefix", env.Data.Price)
	}
}

func TestVWAP_AllFilteredReturns422(t *testing.T) {
	// When the sigma filter removes every trade, the handler must distinguish "empty window"
	// (404) from "everything filtered out" (422, client should relax sigma). The distribution
	// makes every trade an outlier relative to a near-zero sigma.
	baseline := []int64{100, 200, 300, 400, 500, 600, 700, 800, 900, 1000, 1100, 1200}
	trades := make([]canonical.Trade, 0, len(baseline))
	for _, p := range baseline {
		trades = append(trades, mkVWAPTrade(1, p))
	}
	reader := &stubHistoryReader{trades: trades}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	// sigma=0.01 → basically nothing survives; σ²≈120000 for this
	// range so 0.01σ is far tighter than any trade's deviation.
	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD&outlier_sigma=0.01")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (all filtered)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "all-filtered") {
		t.Errorf("body should cite all-filtered: %s", body)
	}
}

func TestVWAP_InvalidSigma400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)

	for _, bad := range []string{"-1", "abc", "NaN"} {
		resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD&outlier_sigma="+bad)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("outlier_sigma=%q: status = %d, want 400", bad, resp.StatusCode)
		}
	}
}

// ─── error-path coverage to parity with TWAP/OHLC ───────────

func TestVWAP_InvalidTime400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD&from=bogus")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestVWAP_InvalidPair400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)

	// base == quote — NewPair rejects with invalid-pair.
	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=native")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestVWAP_ReaderError500(t *testing.T) {
	reader := &stubHistoryReader{err: errors.New("storage broke")}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// pairAwareHistoryReader implements v1.HistoryReader and serves trades per pair. Only methods used by the
// VWAP/TWAP stablecoin-fallback tests are populated; the rest use the embedded stubHistoryReader's defaults.
type pairAwareHistoryReader struct {
	stubHistoryReader
	tradesByPair map[string][]canonical.Trade
}

func (r *pairAwareHistoryReader) TradesInRange(_ context.Context, pair canonical.Pair, _, _ time.Time, _ int) ([]canonical.Trade, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.tradesByPair[pair.Base.String()+"/"+pair.Quote.String()], nil
}

// TestVWAP_StablecoinFiatProxyFallback — when the literal X/fiat:USD pair has zero trades but
// the operator declared a USDC peg, the handler retries against X/<USDC-classic> and serves the
// VWAP with flags.triangulated=true, mirroring /v1/price and /v1/price/tip.
func TestVWAP_StablecoinFiatProxyFallback(t *testing.T) {
	usdcClassic, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	xlm, _ := canonical.ParseAsset("native")
	classicPair, _ := canonical.NewPair(xlm, usdcClassic)

	pegTrade := canonical.Trade{
		Source: "sdex", Ledger: 1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		Timestamp:   time.Unix(1_772_000_000, 0).UTC(),
		Pair:        classicPair,
		BaseAmount:  canonical.NewAmount(big.NewInt(100)),
		QuoteAmount: canonical.NewAmount(big.NewInt(16)),
	}
	reader := &pairAwareHistoryReader{
		// native/fiat:USD literal: zero trades.
		// native/<USDC-classic>: one trade at 16/100 = 0.16.
		tradesByPair: map[string][]canonical.Trade{
			"native/" + usdcClassic.String(): {pegTrade},
		},
	}
	srv := v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdcClassic},
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stablecoin-fiat fallback should serve)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	for _, want := range []string{
		`"price":"0.1600000000"`,
		`"triangulated":true`,
		`"trade_count":1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
}

// TestVWAP_NoPegLeaves404 — without USDPeggedClassics the fallback
// silently skips and the handler still 404s.
func TestVWAP_NoPegLeaves404(t *testing.T) {
	reader := &pairAwareHistoryReader{tradesByPair: map[string][]canonical.Trade{}}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestVWAP_CarriesNoDivergenceVerdict: the cross-reference verdict compares the aggregator's shortest-window VWAP now, while
// /v1/vwap computes over a caller-chosen [from, to), so `divergence_checked: true` would vouch for a number never checked. With
// a firing verdict cached under every spelling, the envelope must report neither and the looker must not be asked.
func TestVWAP_CarriesNoDivergenceVerdict(t *testing.T) {
	verdict := struct{ firing, checked bool }{firing: true, checked: true}
	for _, query := range []string{
		"/v1/vwap?base=native&quote=fiat:USD",
		"/v1/vwap?base=native&quote=fiat:USD&from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z",
	} {
		t.Run(query, func(t *testing.T) {
			reader := &stubHistoryReader{
				trades: []canonical.Trade{mkVWAPTrade(20, 40), mkVWAPTrade(100, 300)},
			}
			div := &stubAliasDivergenceLooker{
				verdicts: map[string]struct{ firing, checked bool }{
					"native": verdict, "crypto:XLM": verdict,
				},
			}
			srv := v1.New(v1.Options{History: reader, Divergence: div})
			ts := httpTestServer(t, srv)

			resp := mustGet(t, ts.URL+query)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			body, _ := readAll(resp)
			for _, want := range []string{`"divergence_checked":false`, `"divergence_warning":false`} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %s: %s", want, body)
				}
			}
			if asked := div.askedSpellings(); len(asked) != 0 {
				t.Errorf("verdict looked up for %v; /v1/vwap must not consult it", asked)
			}
		})
	}
}

// TestVWAP_NonFiatQuoteAliasFirstHit — depth lives under native/<USDC>;
// a ?base=crypto:XLM query must resolve it via the alias loop instead of
// 404ing on the literal crypto:XLM/<USDC> pair.
func TestVWAP_NonFiatQuoteAliasFirstHit(t *testing.T) {
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	native, _ := canonical.ParseAsset("native")
	nativePair, _ := canonical.NewPair(native, usdc)

	// One trade at 16/100 = 0.16, keyed under the native form only.
	trade := canonical.Trade{
		Source: "sdex", Ledger: 1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		Timestamp:   time.Unix(1_772_000_000, 0).UTC(),
		Pair:        nativePair,
		BaseAmount:  canonical.NewAmount(big.NewInt(100)),
		QuoteAmount: canonical.NewAmount(big.NewInt(16)),
	}
	reader := &pairAwareHistoryReader{
		tradesByPair: map[string][]canonical.Trade{
			"native/" + usdc.String(): {trade},
		},
	}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	// Aliased input: base=crypto:XLM must fall through to native/<USDC>.
	resp := mustGet(t, ts.URL+"/v1/vwap?base=crypto:XLM&quote="+usdc.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("aliased base status = %d, want 200 (alias loop must serve native-keyed depth)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.1600000000"`) {
		t.Errorf("body missing served VWAP 0.16: %s", body)
	}

	// Primary form unchanged: base=native still serves the same rate.
	respNative := mustGet(t, ts.URL+"/v1/vwap?base=native&quote="+usdc.String())
	if respNative.StatusCode != http.StatusOK {
		t.Fatalf("primary base=native status = %d, want 200", respNative.StatusCode)
	}
	bodyNative, _ := readAll(respNative)
	if !strings.Contains(bodyNative, `"price":"0.1600000000"`) {
		t.Errorf("primary form body missing VWAP 0.16: %s", bodyNative)
	}
}

// TestVWAP_CacheUnavailable503 — The TradesInRange call
// returning MISCONF lands on the cache-unavailable 503 branch.
func TestVWAP_CacheUnavailable503(t *testing.T) {
	reader := &stubHistoryReader{err: miscOnfErr}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD")
	assertCacheUnavailable(t, resp)
}

func TestVWAP_ProxyDeviationBand(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	xlm, _ := canonical.ParseAsset("native")
	classicPair, _ := canonical.NewPair(xlm, usdc)
	trade := canonical.Trade{
		Source: "sdex", Ledger: 1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		Timestamp:   time.Now().UTC().Add(-time.Minute),
		Pair:        classicPair,
		BaseAmount:  canonical.NewAmount(big.NewInt(100)),
		QuoteAmount: canonical.NewAmount(big.NewInt(16)),
	}
	for _, tc := range []struct {
		usdcUSD string
		want    bool
	}{{"0.95", true}, {"1.001", false}, {"", false}} {
		byPair := map[string]string{}
		if tc.usdcUSD != "" {
			byPair[depegUSDCUSDPair] = tc.usdcUSD
		}
		srv := v1.New(v1.Options{
			History: &pairAwareHistoryReader{tradesByPair: map[string][]canonical.Trade{
				depegXLMUSDCPair: {trade},
			}},
			PriceAt:           &recordingPriceAtReader{byPair: byPair},
			USDPeggedClassics: []canonical.Asset{usdc},
		})
		resp := mustGet(t, startHTTPTest(t, srv.Handler()).URL+"/v1/vwap?base=native&quote=fiat:USD")
		body, _ := readAll(resp)
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"triangulated":true`) {
			t.Fatalf("usdc/usd=%q: status %d, body %s", tc.usdcUSD, resp.StatusCode, body)
		}
		if got := strings.Contains(string(body), `"proxy_deviation":true`); got != tc.want {
			t.Errorf("usdc/usd=%q: proxy_deviation present = %v, want %v\n%s", tc.usdcUSD, got, tc.want, body)
		}
	}
}

// TestVWAP_NonstandardDecimals_Normalizes: /v1/vwap computes from raw trades, so a confirmed non-7-decimals pair is served at
// the CORRECTED price (aggregate.AdjustPrice), not declined with 422. Golden case as TestAdjustPrice_Golden18DecimalToken: base
// 2.5*10^18 (18dp) over quote 1.242*10^7 (USDC 7dp) gives 0.4968.
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

// A seeded market below the substance floor still serves on /v1/vwap and
// /v1/twap (ADR-0018 raw tier), but must say so: flags.thin_market plus the
// same substance block /v1/price withholds with.
func TestVWAPTWAPFlagThinMarketWithEvidence(t *testing.T) {
	native := canonical.NativeAsset()
	thin, err := canonical.ParseAsset(thinAssetID)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err := canonical.ParseAsset(clearedAssetID)
	if err != nil {
		t.Fatal(err)
	}
	reader := &pairAwareHistoryReader{tradesByPair: map[string][]canonical.Trade{
		thin.String() + "/native":    {scamTestTrade(t, thin, native)},
		cleared.String() + "/native": {scamTestTrade(t, cleared, native)},
	}}
	gate := pricingguard.NewSubstanceGate(thinStore{thin: map[string]bool{thinAssetID: true}}, pricingguard.SubstanceGateOptions{})
	ts := startHTTPTest(t, v1.New(v1.Options{History: reader, Substance: gate}).Handler())

	for _, endpoint := range []string{"/v1/vwap", "/v1/twap"} {
		status, body := fetch(t, ts.URL+endpoint+"?base="+thinAssetID+"&quote=native")
		if status != http.StatusOK {
			t.Fatalf("%s thin: status %d, want 200 (raw tier serves): %s", endpoint, status, body)
		}
		env := decodeMap(t, body)
		if flags, _ := env["flags"].(map[string]any); flags["thin_market"] != true {
			t.Errorf("%s thin: flags.thin_market = %v, want true", endpoint, flags["thin_market"])
		}
		data, _ := env["data"].(map[string]any)
		assertEvidence(t, data["substance"], thinAssetID)

		status, body = fetch(t, ts.URL+endpoint+"?base="+clearedAssetID+"&quote=native")
		if status != http.StatusOK {
			t.Fatalf("%s cleared: status %d, want 200: %s", endpoint, status, body)
		}
		if strings.Contains(string(body), "thin_market") || strings.Contains(string(body), "substance") {
			t.Errorf("%s cleared pair carries a thin marker: %s", endpoint, body)
		}
	}
}

const vwapBDBase = "/v1/vwap?base=native&quote=fiat:USD"

func bdTrade(source string, ts time.Time, base, quote int64) canonical.Trade {
	t := mkVWAPTrade(base, quote)
	t.Source = source
	t.Timestamp = ts
	return t
}

func bdGet(t *testing.T, reader *stubHistoryReader, url string) v1.VWAPResult {
	t.Helper()
	ts := httpTestServer(t, v1.New(v1.Options{History: reader}))
	resp := mustGet(t, ts.URL+url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, url)
	}
	var env struct {
		Data v1.VWAPResult `json:"data"`
	}
	mustDecode(t, resp, &env)
	return env.Data
}

func ratOf(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("not a decimal: %q", s)
	}
	return r
}

func TestVWAPBreakdown_AbsentWithoutParam(t *testing.T) {
	reader := &stubHistoryReader{trades: []canonical.Trade{mkVWAPTrade(20, 40), mkVWAPTrade(100, 300)}}
	ts := httpTestServer(t, v1.New(v1.Options{History: reader}))
	resp := mustGet(t, ts.URL+vwapBDBase)
	body, _ := readAll(resp)
	if strings.Contains(body, "breakdown") {
		t.Errorf("response without ?breakdown= must not mention breakdown: %s", body)
	}
}

func TestVWAPBreakdown_SourcesSumToBucketAndWeightsToOne(t *testing.T) {
	t0 := time.Unix(1_772_000_000, 0).UTC()
	reader := &stubHistoryReader{trades: []canonical.Trade{
		bdTrade("soroswap", t0, 10, 30),
		bdTrade("soroswap", t0.Add(time.Second), 20, 50),
		bdTrade("sdex", t0.Add(2*time.Second), 5, 20),
	}}
	res := bdGet(t, reader, vwapBDBase+"&breakdown=source")
	bd := res.Breakdown
	if bd == nil || len(bd.Buckets) != 1 || bd.Interval != nil || bd.Truncated {
		t.Fatalf("breakdown = %+v, want one bucket, null interval, not truncated", bd)
	}
	b := bd.Buckets[0]
	if b.QuoteVolume != res.QuoteVolume || b.TradeCount != res.TradeCount {
		t.Errorf("bucket (%s,%d) != headline (%s,%d)", b.QuoteVolume, b.TradeCount, res.QuoteVolume, res.TradeCount)
	}
	sumQ, sumW := new(big.Int), new(big.Rat)
	for _, s := range b.Sources {
		q, _ := new(big.Int).SetString(s.QuoteVolume, 10)
		sumQ.Add(sumQ, q)
		sumW.Add(sumW, ratOf(t, s.Weight))
	}
	if sumQ.String() != res.QuoteVolume {
		t.Errorf("Σ source quote = %s, want %s", sumQ, res.QuoteVolume)
	}
	if d := new(big.Rat).Sub(big.NewRat(1, 1), sumW); d.Sign() < 0 || d.Cmp(big.NewRat(1, 1_000_000_000)) > 0 {
		t.Errorf("Σ weights = %s, want 1 within 1e-9", sumW.FloatString(12))
	}
	top := b.Sources[0]
	if top.Source != "soroswap" || top.QuoteVolume != "80" || top.BaseVolume != "30" || top.TradeCount != 2 ||
		top.Weight != "0.8000000000" || top.OutliersExcluded != 0 || top.Price == nil || *top.Price != "2.6666666666" {
		t.Errorf("soroswap = %+v", top)
	}
}

func TestVWAPBreakdown_IntervalBuckets(t *testing.T) {
	t0 := time.Date(2026, 3, 1, 10, 30, 0, 0, time.UTC)
	reader := &stubHistoryReader{trades: []canonical.Trade{
		bdTrade("soroswap", t0, 1, 2),
		bdTrade("sdex", t0.Add(time.Hour), 1, 4),
	}}
	bd := bdGet(t, reader, vwapBDBase+"&breakdown=source&interval=1h").Breakdown
	if bd.Interval == nil || *bd.Interval != "1h" || len(bd.Buckets) != 2 {
		t.Fatalf("breakdown = %+v, want 2 buckets at 1h", bd)
	}
	if got := time.Time(bd.Buckets[0].Start); !got.Equal(time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("first bucket start = %v", got)
	}
	if bd.Buckets[0].Sources[0].Source != "soroswap" || bd.Buckets[1].Sources[0].Source != "sdex" {
		t.Errorf("buckets not in ascending time order: %+v", bd.Buckets)
	}
}

func TestVWAPBreakdown_MixedDecimalsWeightsNormalised(t *testing.T) {
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatal(err)
	}
	xlm, _ := canonical.ParseAsset("native")
	pair, _ := canonical.NewPair(xlm, usdc)
	// Same real value: 100 at 7 decimals == 1000 at 8 decimals.
	on := bdTrade("soroswap", time.Unix(1_772_000_000, 0).UTC(), 10, 100)
	cex := bdTrade("binance", time.Unix(1_772_000_001, 0).UTC(), 100, 1000)
	on.Pair, cex.Pair = pair, pair
	res := bdGet(t, &stubHistoryReader{trades: []canonical.Trade{on, cex}},
		"/v1/vwap?base=native&quote="+w2t2USDC+"&breakdown=source")
	for _, s := range res.Breakdown.Buckets[0].Sources {
		if ratOf(t, s.Weight).Cmp(big.NewRat(1, 2)) != 0 {
			t.Errorf("%s weight = %s, want 0.5 after scale normalisation", s.Source, s.Weight)
		}
		// Lifted to the window's 8dp scale, the units of the headline sums.
		if s.QuoteVolume != "1000" {
			t.Errorf("%s quote_volume = %s, want 1000 at 8dp", s.Source, s.QuoteVolume)
		}
	}
	if res.QuoteVolume != "2000" {
		t.Errorf("quote_volume = %s, want 2000 (the sources sum to it)", res.QuoteVolume)
	}
}

func TestVWAPBreakdown_OutlierExclusionsPerSource(t *testing.T) {
	t0 := time.Unix(1_772_000_000, 0).UTC()
	baseline := []int64{100, 101, 99, 100, 102, 98, 101, 100, 99, 100, 101, 100, 99, 101, 100, 102, 99, 100, 101, 100}
	var trades []canonical.Trade
	for i, p := range baseline {
		trades = append(trades, bdTrade("sdex", t0.Add(time.Duration(i)*time.Second), 1, p))
	}
	trades = append(trades, bdTrade("soroswap", t0.Add(time.Minute), 1, 10_000))

	res := bdGet(t, &stubHistoryReader{trades: trades}, vwapBDBase+"&breakdown=source&outlier_sigma=3")
	if res.OutliersFiltered != 1 {
		t.Fatalf("OutliersFiltered = %d, want 1", res.OutliersFiltered)
	}
	got := map[string]v1.VWAPSourceBreakdown{}
	for _, s := range res.Breakdown.Buckets[0].Sources {
		got[s.Source] = s
	}
	if s := got["sdex"]; s.OutliersExcluded != 0 || s.TradeCount != 20 {
		t.Errorf("sdex = %+v, want 0 excluded / 20 trades", s)
	}
	s := got["soroswap"]
	if s.OutliersExcluded != 1 || s.TradeCount != 0 || s.Price != nil || s.Weight != "0" || s.QuoteVolume != "0" {
		t.Errorf("soroswap = %+v, want fully excluded, null price, weight 0", s)
	}

	// Without sigma nothing is excluded.
	res = bdGet(t, &stubHistoryReader{trades: trades}, vwapBDBase+"&breakdown=source")
	for _, s := range res.Breakdown.Buckets[0].Sources {
		if s.OutliersExcluded != 0 {
			t.Errorf("%s excluded %d with sigma 0", s.Source, s.OutliersExcluded)
		}
	}
}

func TestVWAPBreakdown_TruncatedFlag(t *testing.T) {
	t0 := time.Unix(1_772_000_000, 0).UTC()
	trades := make([]canonical.Trade, 10000)
	for i := range trades {
		trades[i] = bdTrade("sdex", t0, 1, 2)
	}
	res := bdGet(t, &stubHistoryReader{trades: trades}, vwapBDBase+"&breakdown=source")
	if !res.Truncated || !res.Breakdown.Truncated {
		t.Errorf("truncated = %v / breakdown %v, want both true at the fetch cap", res.Truncated, res.Breakdown.Truncated)
	}
	res = bdGet(t, &stubHistoryReader{trades: trades[:9999]}, vwapBDBase+"&breakdown=source")
	if res.Breakdown.Truncated {
		t.Error("breakdown.truncated true below the cap")
	}
}

func TestVWAPBreakdown_ParamValidation400(t *testing.T) {
	ts := httpTestServer(t, v1.New(v1.Options{History: &stubHistoryReader{trades: []canonical.Trade{mkVWAPTrade(1, 2)}}}))
	for _, q := range []string{
		"&breakdown=venue",
		"&breakdown=source&interval=7m",
		"&interval=1h",
	} {
		resp := mustGet(t, ts.URL+vwapBDBase+q)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, resp.StatusCode)
		}
	}
}

func TestVWAPBreakdown_MixedDecimalsRankedByWeight(t *testing.T) {
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatal(err)
	}
	xlm, _ := canonical.ParseAsset("native")
	pair, _ := canonical.NewPair(xlm, usdc)
	// 300 at 7 decimals outweighs 1000 at 8 decimals (== 100 at 7), so a
	// raw-volume sort would put binance first.
	on := bdTrade("soroswap", time.Unix(1_772_000_000, 0).UTC(), 30, 300)
	cex := bdTrade("binance", time.Unix(1_772_000_001, 0).UTC(), 100, 1000)
	on.Pair, cex.Pair = pair, pair
	res := bdGet(t, &stubHistoryReader{trades: []canonical.Trade{on, cex}},
		"/v1/vwap?base=native&quote="+w2t2USDC+"&breakdown=source")
	if got := res.Breakdown.Buckets[0].Sources[0].Source; got != "soroswap" {
		t.Errorf("top source = %s, want soroswap (weight 0.75)", got)
	}
}

// /v1/vwap and /v1/twap must withhold a scam-flagged issuer's aggregated price like the other price surfaces. They compute from
// raw trades via their own fetch, bypassing the price-reader gate, so without their own gate /v1/price 404s price-withheld
// while they return 200 with a price.

// scamGateFor withholds exactly the listed bases and records the surface
// label each call was made with, so a test can assert the gate was
// consulted with the right identity — not merely that a 404 appeared.
type scamGateFor struct {
	withheld map[string]bool
	surfaces []string
}

func (g *scamGateFor) Withheld(_ context.Context, base canonical.Asset, surface string) bool {
	g.surfaces = append(g.surfaces, surface)
	return g.withheld[base.String()]
}

// scamTestTrade builds one trade for the pair so the ungated path would
// otherwise serve a real 200 with a price — without it the endpoints
// would 404 for lack of data and the test would pass vacuously.
func scamTestTrade(t *testing.T, base, quote canonical.Asset) canonical.Trade {
	t.Helper()
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return canonical.Trade{
		Source: "sdex", Ledger: 1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000002",
		Timestamp:   time.Now().UTC().Add(-time.Hour),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(1_000_000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(160_000)),
	}
}

// flaggedIssuerAsset is a classic asset whose issuer stands in for a
// directory-scam-flagged one. The gate stub keys on the asset id, so the
// specific strkey only has to parse.
const flaggedIssuerAsset = "RIO-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"

func TestVWAPWithholdsScamFlaggedIssuer(t *testing.T) {
	base, err := canonical.ParseAsset(flaggedIssuerAsset)
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	native, _ := canonical.ParseAsset("native")

	reader := &pairAwareHistoryReader{
		tradesByPair: map[string][]canonical.Trade{
			base.String() + "/native": {scamTestTrade(t, base, native)},
		},
	}
	gate := &scamGateFor{withheld: map[string]bool{base.String(): true}}
	srv := v1.New(v1.Options{History: reader, Scam: gate})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/vwap?base="+base.String()+"&quote=native")
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/v1/vwap status = %d, want 404 — a scam-flagged issuer's "+
			"aggregated price must be withheld here exactly as it is on "+
			"/v1/price and /v1/price/tip (MSP-02). Body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("/v1/vwap body missing the price-withheld problem type: %s", body)
	}
	if len(gate.surfaces) == 0 || gate.surfaces[0] != "vwap" {
		t.Errorf("gate surfaces = %v, want the first to be \"vwap\" — the surface "+
			"label is how an operator attributes a withholding decision", gate.surfaces)
	}
}

func TestTWAPWithholdsScamFlaggedIssuer(t *testing.T) {
	base, err := canonical.ParseAsset(flaggedIssuerAsset)
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	native, _ := canonical.ParseAsset("native")

	reader := &pairAwareHistoryReader{
		tradesByPair: map[string][]canonical.Trade{
			base.String() + "/native": {scamTestTrade(t, base, native)},
		},
	}
	gate := &scamGateFor{withheld: map[string]bool{base.String(): true}}
	srv := v1.New(v1.Options{History: reader, Scam: gate})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/twap?base="+base.String()+"&quote=native")
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/v1/twap status = %d, want 404 (MSP-02). Body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("/v1/twap body missing the price-withheld problem type: %s", body)
	}
	if len(gate.surfaces) == 0 || gate.surfaces[0] != "twap" {
		t.Errorf("gate surfaces = %v, want the first to be \"twap\"", gate.surfaces)
	}
}

// TestVWAPTWAPGateKeysOnBaseNotQuote: the gate keys on the base alone so it survives the frontend's XLM
// triangulation; keying on the pair would let /v1/vwap?base=<flagged>&quote=native through whenever the flagged
// asset appeared on the other side of a different quote.
func TestVWAPTWAPGateKeysOnBaseNotQuote(t *testing.T) {
	base, err := canonical.ParseAsset(flaggedIssuerAsset)
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatalf("parse usdc: %v", err)
	}

	reader := &pairAwareHistoryReader{
		tradesByPair: map[string][]canonical.Trade{
			base.String() + "/" + usdc.String(): {scamTestTrade(t, base, usdc)},
		},
	}
	gate := &scamGateFor{withheld: map[string]bool{base.String(): true}}
	srv := v1.New(v1.Options{History: reader, Scam: gate})
	ts := httpTestServer(t, srv)

	for _, path := range []string{"/v1/vwap", "/v1/twap"} {
		resp := mustGet(t, ts.URL+path+"?base="+base.String()+"&quote="+usdc.String())
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s with a different quote returned %d, want 404 — the gate "+
				"keys on the BASE so it covers every quote", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

// TestVWAPTWAPServeUnflaggedIssuer is the blast-radius guard. The gate
// must withhold ONLY flagged bases; if it 404s an ordinary pair the fix
// has broken two public endpoints for everyone.
func TestVWAPTWAPServeUnflaggedIssuer(t *testing.T) {
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatalf("parse usdc: %v", err)
	}
	native, _ := canonical.ParseAsset("native")

	reader := &pairAwareHistoryReader{
		tradesByPair: map[string][]canonical.Trade{
			"native/" + usdc.String(): {scamTestTrade(t, native, usdc)},
		},
	}
	// Gate is wired but flags nothing.
	gate := &scamGateFor{withheld: map[string]bool{}}
	srv := v1.New(v1.Options{History: reader, Scam: gate})
	ts := httpTestServer(t, srv)

	for _, path := range []string{"/v1/vwap", "/v1/twap"} {
		resp := mustGet(t, ts.URL+path+"?base=native&quote="+usdc.String())
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s for an UNFLAGGED pair returned %d, want 200 — the scam gate "+
				"must not withhold ordinary markets. Body: %s", path, resp.StatusCode, body)
		}
	}
}

// TestVWAPTWAPNilGateServes — a deployment with no gate wired must keep
// serving. Every other gate in this package is nil-safe and these two
// call sites must not be the exception.
func TestVWAPTWAPNilGateServes(t *testing.T) {
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatalf("parse usdc: %v", err)
	}
	native, _ := canonical.ParseAsset("native")

	reader := &pairAwareHistoryReader{
		tradesByPair: map[string][]canonical.Trade{
			"native/" + usdc.String(): {scamTestTrade(t, native, usdc)},
		},
	}
	srv := v1.New(v1.Options{History: reader}) // no Scam gate
	ts := httpTestServer(t, srv)

	for _, path := range []string{"/v1/vwap", "/v1/twap"} {
		resp := mustGet(t, ts.URL+path+"?base=native&quote="+usdc.String())
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s with a nil scam gate returned %d, want 200", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}
