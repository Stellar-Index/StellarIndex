package v1_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func mkOHLCTrade(base, quote int64, ts time.Time) canonical.Trade {
	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	pair, _ := canonical.NewPair(xlm, usd)
	return canonical.Trade{
		Source: "soroswap", Ledger: 1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(base)),
		QuoteAmount: canonical.NewAmount(big.NewInt(quote)),
	}
}

func TestOHLC_503WhenReaderNil(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestOHLC_404WhenNoTradesInWindow(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{trades: nil}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestOHLC_ComputesBarFromTrades(t *testing.T) {
	base := time.Unix(1_772_000_000, 0).UTC()
	reader := &stubHistoryReader{
		trades: []canonical.Trade{
			// Base=1, so price = quote. 100, 150, 80, 120 in order.
			mkOHLCTrade(1, 100, base),
			mkOHLCTrade(1, 150, base.Add(1*time.Second)),
			mkOHLCTrade(1, 80, base.Add(2*time.Second)),
			mkOHLCTrade(1, 120, base.Add(3*time.Second)),
		},
	}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.OHLCBar `json:"data"`
	}
	mustDecode(t, resp, &env)

	// 10-digit precision → "100.0000000000"
	wantOpen := "100.0000000000"
	wantClose := "120.0000000000"
	wantHigh := "150.0000000000"
	wantLow := "80.0000000000"
	if env.Data.Open != wantOpen {
		t.Errorf("Open = %q, want %q", env.Data.Open, wantOpen)
	}
	if env.Data.Close != wantClose {
		t.Errorf("Close = %q, want %q", env.Data.Close, wantClose)
	}
	if env.Data.High != wantHigh {
		t.Errorf("High = %q, want %q", env.Data.High, wantHigh)
	}
	if env.Data.Low != wantLow {
		t.Errorf("Low = %q, want %q", env.Data.Low, wantLow)
	}
	if env.Data.BaseVolume != "4" {
		t.Errorf("BaseVolume = %q, want 4", env.Data.BaseVolume)
	}
	if env.Data.QuoteVolume != "450" {
		t.Errorf("QuoteVolume = %q, want 450", env.Data.QuoteVolume)
	}
	if env.Data.TradeCount != 4 {
		t.Errorf("TradeCount = %d, want 4", env.Data.TradeCount)
	}
}

func TestOHLC_FractionalPrice(t *testing.T) {
	// base=3, quote=1 → price = 1/3 = 0.3333... → truncated to 10
	// digits = "0.3333333333".
	base := time.Unix(1_772_000_000, 0).UTC()
	reader := &stubHistoryReader{
		trades: []canonical.Trade{mkOHLCTrade(3, 1, base)},
	}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD")
	var env struct {
		Data v1.OHLCBar `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.Open != "0.3333333333" {
		t.Errorf("Open = %q, want 0.3333333333 (truncated 1/3)", env.Data.Open)
	}
}

func TestOHLC_InvalidTime400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&from=bogus")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// ─── error-path coverage to parity with TWAP ─────────────────

func TestOHLC_InvalidPair400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)

	// base == quote — NewPair rejects with invalid-pair.
	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=native")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOHLC_ReaderError500(t *testing.T) {
	reader := &stubHistoryReader{err: errors.New("storage broke")}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// ohlcPairAwareReader is a per-pair history reader scoped to this
// test file. The shared stubHistoryReader returns the same trade
// slice regardless of pair, which the stablecoin-fiat fallback
// can't exercise. Mirrors the pairAwareHistoryReader in vwap_test.go;
// kept colocated until that helper merges.
type ohlcPairAwareReader struct {
	stubHistoryReader
	tradesByPair map[string][]canonical.Trade
}

func (r *ohlcPairAwareReader) TradesInRange(_ context.Context, pair canonical.Pair, _, _ time.Time, _ int) ([]canonical.Trade, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.tradesByPair[pair.Base.String()+"/"+pair.Quote.String()], nil
}

// TestOHLC_StablecoinFiatProxyFallback — when the literal
// X/fiat:USD pair has zero trades but the operator declared a
// USDC peg, the OHLC handler retries against X/<USDC-classic> and
// returns the bar with flags.triangulated=true. Mirrors
// /v1/chart's chartStablecoinFallback and the same fallback on
// /v1/price, /v1/price/tip, /v1/vwap, /v1/twap and
// /v1/oracle/lastprice.
//
// Without this, /v1/ohlc?base=native&quote=fiat:USD 404s with
// "no trades in window" out of the box — /v1/ohlc is
// a launch-blocking surface for the asset-detail page.
func TestOHLC_StablecoinFiatProxyFallback(t *testing.T) {
	usdcClassic, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	xlm, _ := canonical.ParseAsset("native")
	classicPair, _ := canonical.NewPair(xlm, usdcClassic)

	t0 := time.Now().UTC().Add(-30 * time.Minute)
	pegTrades := []canonical.Trade{
		{
			Source: "sdex", Ledger: 1,
			TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
			Timestamp:   t0,
			Pair:        classicPair,
			BaseAmount:  canonical.NewAmount(big.NewInt(100)),
			QuoteAmount: canonical.NewAmount(big.NewInt(16)),
		},
		{
			Source: "sdex", Ledger: 2,
			TxHash:      "0000000000000000000000000000000000000000000000000000000000000002",
			Timestamp:   t0.Add(5 * time.Minute),
			Pair:        classicPair,
			BaseAmount:  canonical.NewAmount(big.NewInt(100)),
			QuoteAmount: canonical.NewAmount(big.NewInt(17)),
		},
	}
	reader := &ohlcPairAwareReader{
		// Literal native/fiat:USD missing. native/<USDC-classic>
		// has two trades — open=0.16, close=0.17.
		tradesByPair: map[string][]canonical.Trade{
			"native/" + usdcClassic.String(): pegTrades,
		},
	}
	srv := v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{usdcClassic},
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (peg fallback should serve)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	for _, want := range []string{
		`"open":"0.1600000000"`,
		`"close":"0.1700000000"`,
		`"trade_count":2`,
		`"triangulated":true`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
}

// TestOHLC_StablecoinFiatProxy_NoPegLeaves404 — without
// USDPeggedClassics the fallback skips silently and the 404
// "no trades" path still serves.
func TestOHLC_StablecoinFiatProxy_NoPegLeaves404(t *testing.T) {
	reader := &ohlcPairAwareReader{tradesByPair: map[string][]canonical.Trade{}}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestOHLC_DefaultOutlierFilterRejectsDustTrade exercises the default outlier filter.
// A bar full of normal native/USDC trades plus one 1-stroop ↔
// 1-stroop SDEX dust print (price=1.0, ~6× the real XLM/USDC ratio)
// must NOT have High pegged to $1 — the default 4σ filter drops the
// outlier before ComputeOHLC sees it.
//
// Guards against `/v1/ohlc?base=native&quote=fiat:USD` serving
// High=1.0000000000: ComputeOHLC iterates raw trades and
// accepts any positive base+quote — a single dust ManageOffer cross
// at the offer-book boundary trumps every legitimate print.
//
// ?outlier_sigma=0 still surfaces the dust High for callers
// who want raw inspection; default surfaces the real cluster.
func TestOHLC_DefaultOutlierFilterRejectsDustTrade(t *testing.T) {
	t0 := time.Unix(1_772_000_000, 0).UTC()
	trades := make([]canonical.Trade, 0, 30)
	// 29 normal trades around price 0.16 (XLM/USDC reality).
	// base=10000 stroops, quote=1600 stroops → price = 0.16 exactly.
	for i := 0; i < 29; i++ {
		trades = append(trades, mkOHLCTrade(10000, 1600, t0.Add(time.Duration(i)*time.Second)))
	}
	// One dust trade at price=1.0 — the contamination.
	trades = append(trades, mkOHLCTrade(1, 1, t0.Add(29*time.Second)))

	reader := &stubHistoryReader{trades: trades}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	// Default — dust must be filtered.
	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.OHLCBar `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.High != "0.1600000000" {
		t.Errorf("High = %q, want 0.1600000000 (dust trade should be filtered out)", env.Data.High)
	}
	if env.Data.TradeCount != 29 {
		t.Errorf("TradeCount = %d, want 29 (dust dropped, 29 real trades remain)", env.Data.TradeCount)
	}

	// Opt-out — raw extremes available with outlier_sigma=0.
	resp2 := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&outlier_sigma=0")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("opt-out status = %d", resp2.StatusCode)
	}
	var env2 struct {
		Data v1.OHLCBar `json:"data"`
	}
	mustDecode(t, resp2, &env2)
	if env2.Data.High != "1.0000000000" {
		t.Errorf("opt-out High = %q, want 1.0000000000 (raw, dust included)", env2.Data.High)
	}
}

// TestOHLC_InvalidSigma400 — non-numeric / negative / NaN /
// ±Inf outlier_sigma values 400 with the canonical problem+json
// shape, mirroring /v1/vwap.
func TestOHLC_InvalidSigma400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)
	for _, raw := range []string{"abc", "-1", "NaN", "Inf"} {
		resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD&outlier_sigma="+raw)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("outlier_sigma=%q: status = %d, want 400", raw, resp.StatusCode)
		}
	}
}

// TestOHLC_NonstandardDecimals proves BOTH modes normalize now:
// single-bar mode (raw trades, query-time — normalized since v0.12.0)
// and interval= series mode (prices_<n> CAGG — normalized,
// not declined with 422).
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

// A count majority of dust prints must not become the single-bar OHLC
// by trimming a volume majority: 3 × 1,000,000 XLM at 0.100 and
// 4 × 30,000 XLM at 0.114 would serve a 200 bar of the 4 wash prints
// alone. The window is contested, so the default filter withholds it
// as all-filtered rather than serving either side or claiming "no trades".
func TestOHLC_DustCountMajorityWindowIsWithheldNotServed(t *testing.T) {
	t0 := time.Unix(1_772_000_000, 0).UTC()
	trades := make([]canonical.Trade, 0, 7)
	for i := 0; i < 3; i++ {
		trades = append(trades, mkOHLCTrade(10_000_000_000_000, 1_000_000_000_000, t0.Add(time.Duration(i)*10*time.Second)))
	}
	for i := 0; i < 4; i++ {
		trades = append(trades, mkOHLCTrade(300_000_000_000, 34_200_000_000, t0.Add(time.Duration(i)*10*time.Second+5*time.Second)))
	}
	ts := httpTestServer(t, v1.New(v1.Options{History: &stubHistoryReader{trades: trades}}))

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (contested window withheld); body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "all-filtered") {
		t.Errorf("body should cite all-filtered: %s", body)
	}
}

// TestOHLC_VolumeScale_CEXWindowIsEightDecimals is the headline case: a
// Coinbase-fed pair's volume integers are at 1e8, and until the bar said
// so the /markets/[pair] page divided them by a hardcoded 1e7 and printed
// a quote volume ten times the market's.
func TestOHLC_VolumeScale_CEXWindowIsEightDecimals(t *testing.T) {
	base := time.Unix(1_772_000_000, 0).UTC()
	// 1000 XLM at $0.25, three times: 750 USD of quote volume.
	reader := &stubHistoryReader{
		trades: []canonical.Trade{
			mkScaledOHLCTrade("coinbase", scaledUnits(1000, 8), scaledUnits(250, 8), 1, base),
			mkScaledOHLCTrade("coinbase", scaledUnits(1000, 8), scaledUnits(250, 8), 2, base.Add(time.Second)),
			mkScaledOHLCTrade("coinbase", scaledUnits(1000, 8), scaledUnits(250, 8), 3, base.Add(2*time.Second)),
		},
	}
	got := getOHLCBar(t, reader, "")

	if got.QuoteVolumeDecimals == nil || *got.QuoteVolumeDecimals != 8 {
		t.Errorf("quote_volume_decimals = %s, want 8 (coinbase stamps amounts at 1e8)",
			showDecimals(got.QuoteVolumeDecimals))
	}
	if u := assetUnits(t, got.QuoteVolume, got.QuoteVolumeDecimals); u != "750.00000000" {
		t.Errorf("quote volume in asset units = %s, want 750.00000000", u)
	}
	if u := assetUnits(t, got.BaseVolume, got.BaseVolumeDecimals); u != "3000.00000000" {
		t.Errorf("base volume in asset units = %s, want 3000.00000000", u)
	}
}

// TestOHLC_VolumeScale_SurvivesOutlierFilterDroppingTheOnlyCEXPrint is
// the narrow window a scale fix could re-open: the scale must be
// resolved over the PRE-outlier-filter population, because the lift to
// the common scale ran over that population and the filter does not
// un-lift the rows it keeps.
//
// Four honest on-chain prints at 1e7 plus one aberrant CEX print at 1e8:
// the CEX print is dropped by the default sigma, but the four survivors
// were already multiplied by ten to meet it. A bar that resolves its
// scale over the post-filter slice states 7 for integers that are at 8
// and renders 14000 USD where the market did 1400.
func TestOHLC_VolumeScale_SurvivesOutlierFilterDroppingTheOnlyCEXPrint(t *testing.T) {
	base := time.Unix(1_772_000_000, 0).UTC()
	reader := &stubHistoryReader{
		trades: []canonical.Trade{
			// Four on-chain prints: 1000 XLM at $0.35 each, 1e7 scale.
			mkScaledOHLCTrade("sdex", scaledUnits(1000, 7), scaledUnits(350, 7), 1, base),
			mkScaledOHLCTrade("sdex", scaledUnits(1000, 7), scaledUnits(350, 7), 2, base.Add(time.Second)),
			mkScaledOHLCTrade("sdex", scaledUnits(1000, 7), scaledUnits(350, 7), 3, base.Add(2*time.Second)),
			mkScaledOHLCTrade("sdex", scaledUnits(1000, 7), scaledUnits(350, 7), 4, base.Add(3*time.Second)),
			// One wild CEX print at $50 — the only 1e8 venue in the
			// window, and the one the default sigma removes.
			mkScaledOHLCTrade("coinbase", scaledUnits(1, 8), scaledUnits(50, 8), 5, base.Add(4*time.Second)),
		},
	}
	got := getOHLCBar(t, reader, "")

	if got.TradeCount != 4 {
		t.Fatalf("trade_count = %d, want 4 — the fixture's outlier must be filtered "+
			"for this test to exercise the population it is about", got.TradeCount)
	}
	if got.QuoteVolumeDecimals == nil || *got.QuoteVolumeDecimals != 8 {
		t.Errorf("quote_volume_decimals = %s, want 8 — the surviving rows were lifted "+
			"to the dropped venue's scale and stay there", showDecimals(got.QuoteVolumeDecimals))
	}
	if u := assetUnits(t, got.QuoteVolume, got.QuoteVolumeDecimals); u != "1400.00000000" {
		t.Errorf("quote volume in asset units = %s, want 1400.00000000", u)
	}
	if u := assetUnits(t, got.BaseVolume, got.BaseVolumeDecimals); u != "4000.00000000" {
		t.Errorf("base volume in asset units = %s, want 4000.00000000", u)
	}
}

// TestOHLC_VolumeScale_OnChainWindowIsSevenDecimals pins the other
// direction: an all-sdex window really is 1e7 and must say 7, so the fix
// cannot degenerate into "always answer 8".
func TestOHLC_VolumeScale_OnChainWindowIsSevenDecimals(t *testing.T) {
	base := time.Unix(1_772_000_000, 0).UTC()
	reader := &stubHistoryReader{
		trades: []canonical.Trade{
			mkScaledOHLCTrade("sdex", scaledUnits(1000, 7), scaledUnits(350, 7), 1, base),
			mkScaledOHLCTrade("sdex", scaledUnits(1000, 7), scaledUnits(350, 7), 2, base.Add(time.Second)),
			mkScaledOHLCTrade("sdex", scaledUnits(1000, 7), scaledUnits(350, 7), 3, base.Add(2*time.Second)),
		},
	}
	got := getOHLCBar(t, reader, "")

	if got.BaseVolumeDecimals == nil || *got.BaseVolumeDecimals != 7 {
		t.Errorf("base_volume_decimals = %s, want 7 (sdex stamps amounts at 1e7)",
			showDecimals(got.BaseVolumeDecimals))
	}
	if u := assetUnits(t, got.QuoteVolume, got.QuoteVolumeDecimals); u != "1050.0000000" {
		t.Errorf("quote volume in asset units = %s, want 1050.0000000", u)
	}
}

// TestOHLC_VolumeScale_UnregisteredSourceIsUnknown pins that a source
// absent from external.Registry must not be answered with the registry's
// CEX-flavoured 8-decimal fallback. An unregistered on-chain DEX at 1e7
// would otherwise be stated as 8, overstating its volume tenfold the
// opposite way.
func TestOHLC_VolumeScale_UnregisteredSourceIsUnknown(t *testing.T) {
	base := time.Unix(1_772_000_000, 0).UTC()
	reader := &stubHistoryReader{
		trades: []canonical.Trade{
			mkScaledOHLCTrade("brand_new_dex", scaledUnits(1000, 7), scaledUnits(350, 7), 1, base),
			mkScaledOHLCTrade("brand_new_dex", scaledUnits(1000, 7), scaledUnits(350, 7), 2, base.Add(time.Second)),
		},
	}
	got := getOHLCBar(t, reader, "")

	if got.BaseVolumeDecimals != nil {
		t.Errorf("base_volume_decimals = %s, want absent (null) — \"brand_new_dex\" has "+
			"no external.Registry entry, so its scale is unknown, not the registry's "+
			"8-decimal fallback", showDecimals(got.BaseVolumeDecimals))
	}
	if got.QuoteVolumeDecimals != nil {
		t.Errorf("quote_volume_decimals = %s, want absent (null)", showDecimals(got.QuoteVolumeDecimals))
	}
}

// ohlcScaledBar decodes /v1/ohlc's single-bar payload with the two
// volume-scale fields as POINTERS on purpose: this file has to compile
// against a build that does not serve them, so that the assertions below
// are provably about the served scale and not about a struct tag.
type ohlcScaledBar struct {
	BaseVolume          string `json:"base_volume"`
	QuoteVolume         string `json:"quote_volume"`
	BaseVolumeDecimals  *int   `json:"base_volume_decimals"`
	QuoteVolumeDecimals *int   `json:"quote_volume_decimals"`
	TradeCount          int    `json:"trade_count"`
}

// mkScaledOHLCTrade builds one native/fiat:USD print attributed to
// `source`, with the raw smallest-unit amounts given verbatim — the
// point of these fixtures is that the caller chooses the scale, because
// the source is what decides it (sdex 1e7, coinbase 1e8).
func mkScaledOHLCTrade(source string, base, quote *big.Int, opIndex uint32, ts time.Time) canonical.Trade {
	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	pair, _ := canonical.NewPair(xlm, usd)
	return canonical.Trade{
		Source:      source,
		Ledger:      1,
		TxHash:      fmt.Sprintf("%064d", opIndex),
		OpIndex:     opIndex,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(base),
		QuoteAmount: canonical.NewAmount(quote),
	}
}

// scaledUnits is `n` whole asset units at a `decimals`-place smallest
// unit — n × 10^decimals.
func scaledUnits(n int64, decimals int) *big.Int {
	return new(big.Int).Mul(big.NewInt(n),
		new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
}

// assetUnits renders a served volume integer in the asset's own units,
// the way a consumer must: value / 10^stated_decimals. A response that
// states no scale cannot be rendered at all, which is a defect
// and is reported as such.
func assetUnits(t *testing.T, raw string, decimals *int) string {
	t.Helper()
	if decimals == nil {
		t.Fatalf("response states no volume scale, so %q cannot be rendered in asset units "+
			"— a consumer is left guessing a divisor (finding F096)", raw)
	}
	if *decimals < 0 {
		t.Fatalf("volume scale = %d, want a non-negative decimal exponent", *decimals)
	}
	n, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		t.Fatalf("volume %q is not an integer", raw)
	}
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(*decimals)), nil)
	return new(big.Rat).SetFrac(n, pow).FloatString(*decimals)
}

// showDecimals renders a stated scale for a failure message: the value,
// or "absent" when the response carried none at all.
func showDecimals(d *int) string {
	if d == nil {
		return "absent"
	}
	return fmt.Sprintf("%d", *d)
}

func getOHLCBar(t *testing.T, reader *stubHistoryReader, query string) ohlcScaledBar {
	t.Helper()
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD"+query)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data ohlcScaledBar `json:"data"`
	}
	mustDecode(t, resp, &env)
	return env.Data
}
