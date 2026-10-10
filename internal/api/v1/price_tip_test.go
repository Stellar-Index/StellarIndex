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

// mkTipTrade builds a simple native/fiat:USD trade with the given
// timestamp, integer base, integer quote, and source. Used to seed
// stubHistoryReader fixtures in tip-window tests.
func mkTipTrade(ts time.Time, base, quote int64, source string) canonical.Trade {
	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	pair, _ := canonical.NewPair(xlm, usd)
	return canonical.Trade{
		Source:      source,
		Ledger:      1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(base)),
		QuoteAmount: canonical.NewAmount(big.NewInt(quote)),
	}
}

// TestPriceTip_RejectsBadRequests: no reader is 503; a granularity,
// missing asset, identity pair or out-of-range window is 400.
func TestPriceTip_RejectsBadRequests(t *testing.T) {
	cases := []struct {
		name, query, wantBody string
		noReader              bool
		want                  int
	}{
		{name: "no reader", noReader: true, query: "?asset=native&quote=fiat:USD", want: http.StatusServiceUnavailable},
		{name: "granularity", query: "?asset=native&quote=fiat:USD&granularity=1m", want: http.StatusBadRequest, wantBody: "invalid-tip-param"},
		{name: "missing asset", query: "", want: http.StatusBadRequest},
		{name: "identity pair", query: "?asset=native&quote=native", want: http.StatusBadRequest},
	}
	for _, raw := range []string{"0", "61", "-1", "abc", "9999999999999999999"} {
		cases = append(cases, struct {
			name, query, wantBody string
			noReader              bool
			want                  int
		}{name: "window_seconds=" + raw, query: "?asset=native&quote=fiat:USD&window_seconds=" + raw, want: http.StatusBadRequest})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := v1.Options{Prices: &stubPriceReader{}}
			if tc.noReader {
				opts = v1.Options{}
			}
			ts := startHTTPTest(t, v1.New(opts).Handler())
			resp := mustGet(t, ts.URL+"/v1/price/tip"+tc.query)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.wantBody != "" {
				if body, _ := readAll(resp); !strings.Contains(body, tc.wantBody) {
					t.Errorf("error type %q missing: %s", tc.wantBody, body)
				}
			}
		})
	}
}

// TestPriceTip_WindowVWAP — happy path: history reader returns
// trades inside the rolling window, handler computes a VWAP and the
// response carries price_type="vwap" with the requested
// window_seconds.
func TestPriceTip_WindowVWAP(t *testing.T) {
	now := time.Now().UTC()
	hist := &stubHistoryReader{
		trades: []canonical.Trade{
			// VWAP = (50 + 50) / (5 + 5) = 10
			mkTipTrade(now.Add(-3*time.Second), 5, 50, "soroswap"),
			mkTipTrade(now.Add(-1*time.Second), 5, 50, "soroswap"),
		},
	}
	prices := &stubPriceReader{}
	srv := v1.New(v1.Options{Prices: prices, History: hist})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote=fiat:USD&window_seconds=5")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	for _, want := range []string{
		`"price_type":"vwap"`,
		`"window_seconds":5`,
		`"sources":["soroswap"]`,
		`"single_source":true`,
		`"stale":false`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
	// VWAP of (5,50) + (5,50) = 100/10 = 10 → "10.0000000000" at
	// ohlcPriceDigits=10.
	if !strings.Contains(body, `"price":"10.0000000000"`) {
		t.Errorf("VWAP price wrong: %s", body)
	}
}

// TestPriceTip_FallbackWhenWindowEmpty — when the rolling window has
// no trades, the handler falls back to PriceReader.LatestPrice and
// returns whatever shape it gives (price_type="last_trade" in the
// MVP). flags.stale stays FALSE — the fallback is in-contract on
// /v1/price/tip per ADR-0018, even though the same fallback
// triggers stale=true on /v1/price.
func TestPriceTip_FallbackWhenWindowEmpty(t *testing.T) {
	hist := &stubHistoryReader{trades: nil}
	prices := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {
				AssetID:    "native",
				Quote:      "fiat:USD",
				Price:      "0.1242",
				PriceType:  "last_trade",
				ObservedAt: v1.WireTime(time.Unix(1745000000, 0).UTC()),
			},
		},
		sources: map[string][]string{"native/fiat:USD": {"sdex"}},
		// Reader's "stale" bit is set — but on /v1/price/tip it must
		// NOT propagate to the envelope flag (ADR-0018).
		stale: map[string]bool{"native/fiat:USD": true},
	}
	srv := v1.New(v1.Options{Prices: prices, History: hist})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote=fiat:USD&window_seconds=5")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	for _, want := range []string{
		`"price":"0.1242"`,
		`"price_type":"last_trade"`,
		`"sources":["sdex"]`,
		// Critical: the reader's stale=true is INTENTIONALLY ignored
		// on this surface.
		`"stale":false`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
}

// TestPriceTip_FallbackWhenNoHistoryWired — same fallback path
// triggers when the deployment hasn't wired a HistoryReader at all
// (e.g. early bring-up, or PriceReader-only deployments). PriceReader
// alone is sufficient to serve the tip surface; window VWAP is just
// an enrichment when history is available.
func TestPriceTip_FallbackWhenNoHistoryWired(t *testing.T) {
	prices := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {Price: "0.55", PriceType: "last_trade"},
		},
	}
	srv := v1.New(v1.Options{Prices: prices})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.55"`) {
		t.Errorf("fallback body missing: %s", body)
	}
}

// TestPriceTip_AliasResolvesXLM pins, on the tip surface:
// asset=native must resolve a LatestPrice observation published under
// the crypto:XLM alias key (the rolling-window VWAP path being empty),
// exactly like handlePrice's primary read. Querying the literal form only
// would 404 while /v1/price serves fresh.
func TestPriceTip_AliasResolvesXLM(t *testing.T) {
	prices := &stubPriceReader{
		// Only the crypto:XLM form is populated; native is absent.
		snapshots: map[string]v1.PriceSnapshot{
			"crypto:XLM/fiat:USD": {
				AssetID: "crypto:XLM", Quote: "fiat:USD",
				Price: "0.55", PriceType: "last_trade",
			},
		},
	}
	srv := v1.New(v1.Options{Prices: prices})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via crypto:XLM alias", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.55"`) {
		t.Errorf("tip alias body missing price 0.55: %s", body)
	}
}

// TestPriceTip_RedisFallbackForRewrittenPair — when both the
// rolling-window VWAP path AND PriceReader.LatestPrice come up empty
// (typical for an aggregator-rewritten pair like XLM/fiat:USD whose
// literal form isn't in prices_1m), the handler falls through to the
// Redis VWAP cache. Same shape as /v1/price's tryRedisVWAPFallback —
// the two surfaces serve the same underlying data so a customer
// switching between them sees consistent prices.
func TestPriceTip_RedisFallbackForRewrittenPair(t *testing.T) {
	hist := &stubHistoryReader{trades: nil}
	prices := &stubPriceReader{err: v1.ErrPriceNotFound} // CAGG miss
	looker := &stubTriangulatedPriceLooker{
		value:          "0.157384502084",
		isTriangulated: false, // direct rewrite — no marker
		found:          true,
	}
	srv := v1.New(v1.Options{
		Prices:       prices,
		History:      hist,
		Triangulated: looker,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote=fiat:USD&window_seconds=5")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — Redis fallback should serve direct rewrites", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.157384502084"`) {
		t.Errorf("Redis-fallback price missing: %s", body)
	}
	// stale stays false on /v1/price/tip per ADR-0018, regardless of
	// which fallback path produced the snapshot.
	if !strings.Contains(body, `"stale":false`) {
		t.Errorf("stale flag wrong: %s", body)
	}
}

// TestPriceTip_StablecoinFiatProxyFallback — when window VWAP +
// LatestPrice + Redis VWAP cache all miss but the operator has
// declared classic USD pegs, the handler rewrites X/fiat:USD to
// X/<peg> at request time. Same shape as /v1/price's
// tryStablecoinFiatProxy fallback. Without this
// /v1/price/tip?asset=native&quote=fiat:USD 404s out of the box on
// every fresh deployment.
func TestPriceTip_StablecoinFiatProxyFallback(t *testing.T) {
	usdcClassic, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	hist := &stubHistoryReader{trades: nil}
	prices := &stubPriceReader{
		// Literal native/fiat:USD missing → ErrPriceNotFound.
		// native/<USDC-classic> serves the actual VWAP.
		snapshots: map[string]v1.PriceSnapshot{
			"native/" + usdcClassic.String(): {
				AssetID:    "native",
				Quote:      usdcClassic.String(),
				Price:      "0.1626",
				PriceType:  "vwap",
				ObservedAt: v1.WireTime(time.Unix(1745000000, 0).UTC()),
			},
		},
		sources: map[string][]string{
			"native/" + usdcClassic.String(): {"sdex"},
		},
	}
	srv := v1.New(v1.Options{
		Prices:            prices,
		History:           hist,
		USDPeggedClassics: []canonical.Asset{usdcClassic},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — stablecoin-fiat-proxy fallback should serve", resp.StatusCode)
	}
	body, _ := readAll(resp)
	for _, want := range []string{
		`"price":"0.1626"`,
		`"quote":"fiat:USD"`,
		`"sources":["sdex"]`,
		`"stale":false`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
}

// TestPriceTip_ReportsWithheldFromProxyLeg: the stablecoin-
// fiat-proxy peg walk inside computeTip's fallback chain HAS a price
// for the asset (the peg leg) and policy withholds it — the tip
// surface must report errors/price-withheld, the same verdict
// /v1/price already gives via [TestPriceReportsWithheldFromProxyLeg],
// not errors/price-not-found (which tells the customer to look
// nowhere when /v1/observations, /v1/ohlc and /v1/history all have
// the data).
func TestPriceTip_ReportsWithheldFromProxyLeg(t *testing.T) {
	peg, err := canonical.ParseAsset(msp06Peg)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := canonical.ParseAsset("RIO-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ")
	if err != nil {
		t.Fatal(err)
	}

	reader := &pegAwarePriceReader{errByPair: map[string]error{
		// Direct fiat read misses — the dominant on-chain shape.
		asset.String() + "/fiat:USD": v1.ErrPriceNotFound,
		// The peg leg HAS a price, and policy withholds it.
		asset.String() + "/" + peg.String(): v1.ErrPriceWithheld,
	}}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{peg},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset="+asset.String()+"&quote=fiat:USD")
	body, _ := readAll(resp)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. Body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "errors/price-withheld") {
		t.Errorf("body reports the wrong problem type — the proxy leg returned a "+
			"WITHHELD verdict, so the answer is \"we have a price and decline to "+
			"publish it\", not \"we have none\" (T684). Body: %s", body)
	}
	if strings.Contains(body, "errors/price-not-found") {
		t.Errorf("withheld reported as not-found — the customer is told to look "+
			"nowhere, when the withheld body would name /v1/observations, "+
			"/v1/ohlc and /v1/history. Body: %s", body)
	}
}

// TestPriceTip_HistoryErrorFallsThroughToFallback — a hypertable
// hiccup must NOT take down the tip surface when LatestPrice can
// still serve. The handler logs the error and quietly drops to the
// fallback path.
func TestPriceTip_HistoryErrorFallsThroughToFallback(t *testing.T) {
	hist := &stubHistoryReader{err: errors.New("hypertable bounced")}
	prices := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {Price: "0.99", PriceType: "last_trade"},
		},
	}
	srv := v1.New(v1.Options{Prices: prices, History: hist})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — hypertable error must NOT take down tip", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.99"`) {
		t.Errorf("fallback didn't fire: %s", body)
	}
	if strings.Contains(body, "hypertable bounced") {
		t.Errorf("internal error leaked to client: %s", body)
	}
}

// TestPriceTip_404WhenNothingAvailable — empty window AND
// LatestPrice 404s. Handler returns 404 (the pair has no
// observations at all, not just no recent ones).
func TestPriceTip_404WhenNothingAvailable(t *testing.T) {
	hist := &stubHistoryReader{trades: nil}
	prices := &stubPriceReader{err: v1.ErrPriceNotFound}
	srv := v1.New(v1.Options{Prices: prices, History: hist})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestPriceTip_FallbackInternalError — non-404 error from
// LatestPrice on the fallback path returns 500 + does not leak the
// internal error string.
func TestPriceTip_FallbackInternalError(t *testing.T) {
	hist := &stubHistoryReader{trades: nil}
	prices := &stubPriceReader{err: errors.New("redis exploded")}
	srv := v1.New(v1.Options{Prices: prices, History: hist})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if strings.Contains(body, "redis exploded") {
		t.Errorf("internal error leaked: %s", body)
	}
}

// TestPriceTip_DivergenceFlagPropagates — divergence is asset-level
// and applies on the tip surface (ADR-0018 only excludes Frozen, not
// DivergenceWarning). Verifies the flag fires through both branches:
// the rolling-window VWAP and the LatestPrice fallback.
func TestPriceTip_DivergenceFlagPropagates(t *testing.T) {
	now := time.Now().UTC()

	t.Run("window-VWAP branch", func(t *testing.T) {
		hist := &stubHistoryReader{
			trades: []canonical.Trade{mkTipTrade(now.Add(-2*time.Second), 1, 1, "soroswap")},
		}
		prices := &stubPriceReader{}
		div := &stubDivergenceLooker{firing: true}
		srv := v1.New(v1.Options{Prices: prices, History: hist, Divergence: div})
		ts := startHTTPTest(t, srv.Handler())

		resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote=fiat:USD")
		body, _ := readAll(resp)
		if !strings.Contains(body, `"divergence_warning":true`) {
			t.Errorf("divergence flag not set on window-VWAP branch: %s", body)
		}
	})

	t.Run("fallback branch", func(t *testing.T) {
		prices := &stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{
				"native/fiat:USD": {Price: "0.5", PriceType: "last_trade"},
			},
		}
		div := &stubDivergenceLooker{firing: true}
		srv := v1.New(v1.Options{Prices: prices, Divergence: div})
		ts := startHTTPTest(t, srv.Handler())

		resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote=fiat:USD")
		body, _ := readAll(resp)
		if !strings.Contains(body, `"divergence_warning":true`) {
			t.Errorf("divergence flag not set on fallback branch: %s", body)
		}
	})
}

// TestPriceTip_DefaultWindowIs5s — when window_seconds is omitted,
// the handler uses the ADR's default of 5 seconds.
func TestPriceTip_DefaultWindowIs5s(t *testing.T) {
	now := time.Now().UTC()
	hist := &stubHistoryReader{
		trades: []canonical.Trade{mkTipTrade(now.Add(-1*time.Second), 1, 7, "soroswap")},
	}
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}, History: hist})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"window_seconds":5`) {
		t.Errorf("default window not 5: %s", body)
	}
	// stubHistoryReader.lastCall captures from/to — verify the
	// duration matches a 5s window.
	delta := hist.lastCall.to.Sub(hist.lastCall.from)
	// Tolerate scheduling jitter — should be exactly 5s but the
	// time.Now() in the handler runs after the test's `now` capture
	// so an exact-equality check would be flaky on a slow CI host.
	if delta < 4500*time.Millisecond || delta > 5500*time.Millisecond {
		t.Errorf("TradesInRange window = %v, want ~5s", delta)
	}
}

// TestPriceTip_NonstandardDecimals_Normalizes proves /v1/price/tip — the
// ADR-0018 "SLA surface" — correctly scales a confirmed non-7-decimals
// leg's window VWAP. This endpoint once lacked a decline guard
// at all (declineIfNonstandardDecimals's four-endpoint list omitted the
// tip surface), so it was serving the RAW skewed ratio live and unguarded;
// this is the regression test for that gap. Same golden shape as
// internal/aggregate's TestAdjustPrice_Golden18DecimalToken: 18dp base
// token, 7dp USDC quote, true price 0.4968.
func TestPriceTip_NonstandardDecimals_Normalizes(t *testing.T) {
	const flaggedAsset = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 18)

	token, err := canonical.ParseAsset(flaggedAsset)
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	usdc, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	pair, err := canonical.NewPair(token, usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	baseAmount, ok := new(big.Int).SetString("2500000000000000000", 10)
	if !ok {
		t.Fatal("bad big.Int literal")
	}
	now := time.Now().UTC()
	trade := canonical.Trade{
		Source:      "aquarius",
		Ledger:      1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		Timestamp:   now.Add(-1 * time.Second),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(baseAmount),
		QuoteAmount: canonical.NewAmount(big.NewInt(12_420_000)),
	}
	hist := &stubHistoryReader{trades: []canonical.Trade{trade}}
	srv := v1.New(v1.Options{
		Prices:              &stubPriceReader{},
		History:             hist,
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset="+flaggedAsset+"&quote=USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN&window_seconds=5")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.4968000000"`) {
		t.Errorf("body missing normalized price 0.4968000000 (unnormalized would be ~10^-11 of that): %s", body)
	}
}

// TestPriceTip_CacheUnavailable503 — handlePriceTip's
// computeTip helper now distinguishes a MISCONF surfacing from
// PriceReader.LatestPrice from a generic internal error.
func TestPriceTip_CacheUnavailable503(t *testing.T) {
	srv := v1.New(v1.Options{Prices: tipCacheUnavailablePriceReader{}})
	tsv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsv.URL+"/v1/price/tip?asset=native&quote=fiat:USD")
	assertCacheUnavailable(t, resp)
}

func TestPriceTip_NonstandardDecimals_FallbackNormalizes(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	key := flaggedAsset + "/fiat:USD"
	srv := v1.New(v1.Options{
		// Empty history → tipWindowVWAP finds no trades → falls through to the
		// readPriceWithAliases(s.Prices) branch (price_tip.go:168), the M2 gap.
		History: &stubHistoryReader{},
		Prices: &stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{key: {
				AssetID: flaggedAsset, Quote: "fiat:USD", Price: "41.32",
				PriceType: "last_trade", ObservedAt: v1.WireTime(time.Unix(1745000000, 0).UTC()),
			}},
			sources: map[string][]string{key: {"aquarius"}},
		},
		NonstandardDecimals: cache,
	})
	tsrv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsrv.URL+"/v1/price/tip?asset="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"4132.0000000000"`) {
		t.Errorf("/v1/price/tip fallback not normalized (want 4132.0000000000): %s", body)
	}
}

// TestPriceTip_ThinPoolThirdAlias_ClassicQuoteFallsToTheClosedBook: the
// SDEX book is silent in the window, the Soroban SAC/SAC pool printed
// one trade 2s ago at five times the book. The classic-quoted tip must
// NOT serve that print; with the established forms silent it serves the
// closed-bucket read — the deep book — and never consults a SAC pair,
// because the closed bucket answered before the SAC set's turn came.
//
// RED before tipMergePairs: price 0.0050000000 from soroswap.
func TestPriceTip_ThinPoolThirdAlias_ClassicQuoteFallsToTheClosedBook(t *testing.T) {
	installPegAliasRegistry(t)
	now := time.Now().UTC()
	hist := &recordingHistoryReader{stubHistoryReader: &stubHistoryReader{trades: []canonical.Trade{
		mkPairTrade(t, pegAliasAquaSAC, pegAliasUSDCSAC, now.Add(-2*time.Second), 10_000_000, 50_000, "soroswap"),
	}}}
	prices := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			pegAliasAquaClassic + "/" + pegAliasUSDCClassic: {
				AssetID: pegAliasAquaClassic, Quote: pegAliasUSDCClassic,
				Price: "0.0010", PriceType: "vwap", ObservedAt: v1.WireTime(now.Add(-40 * time.Second)),
			},
		},
		sources: map[string][]string{pegAliasAquaClassic + "/" + pegAliasUSDCClassic: {"sdex"}},
	}
	srv := v1.New(v1.Options{Prices: prices, History: hist})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset="+pegAliasAquaClassic+"&quote="+pegAliasUSDCClassic)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.0010"`) {
		t.Errorf("price: want the closed SDEX bucket 0.0010 — a fresh SAC/SAC print must not become the classic-quoted tip: %s", body)
	}
	if !strings.Contains(body, `"sources":["sdex"]`) {
		t.Errorf("sources: want [sdex]: %s", body)
	}
	assertNoSACPairConsulted(t, hist.pairs)
	// The established forms WERE consulted (5s then the 30s escalation),
	// so the exclusion is a choice of candidates, not a disabled merge.
	if callIndex(hist.pairs, pegAliasAquaClassic+"/"+pegAliasUSDCClassic) < 0 {
		t.Errorf("tip window never read the classic pair; consulted %v", hist.pairs)
	}
}

// TestPriceTip_ThinPoolThirdAlias_XLMMergesEstablishedFormsOnly: for XLM
// the established forms (`native`, `crypto:XLM`) still merge — that is
// the merge's reason to exist — and the XLM SAC pool is left out of a
// classic-quoted window even when it printed inside it.
//
// RED before tipMergePairs: the SAC print is blended in, price
// 0.7100000000 with sources [sdex soroswap].
func TestPriceTip_ThinPoolThirdAlias_XLMMergesEstablishedFormsOnly(t *testing.T) {
	installPegAliasRegistry(t)
	now := time.Now().UTC()
	hist := &recordingHistoryReader{stubHistoryReader: &stubHistoryReader{trades: []canonical.Trade{
		// SDEX: 1 XLM for 0.315 USDC, twice.
		mkPairTrade(t, "native", pegAliasUSDCClassic, now.Add(-3*time.Second), 10_000_000, 3_150_000, "sdex"),
		mkPairTrade(t, "native", pegAliasUSDCClassic, now.Add(-1*time.Second), 10_000_000, 3_150_000, "sdex"),
		// The Soroban XLM pool, SAC/SAC: 1 XLM for 1.5 USDC — a print
		// that would drag a merged VWAP to 0.71.
		mkPairTrade(t, canonical.XLMSacContractID, pegAliasUSDCSAC, now.Add(-2*time.Second), 10_000_000, 15_000_000, "soroswap"),
	}}}
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}, History: hist})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote="+pegAliasUSDCClassic+"&window_seconds=5")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.3150000000"`) {
		t.Errorf("price: want the SDEX-only window VWAP 0.3150000000: %s", body)
	}
	if !strings.Contains(body, `"sources":["sdex"]`) {
		t.Errorf("sources: want [sdex] — the SAC pool must not be merged into a native-keyed tip: %s", body)
	}
	assertNoSACPairConsulted(t, hist.pairs)
	// Both established forms are still in the merge set.
	for _, want := range []string{"native/" + pegAliasUSDCClassic, "crypto:XLM/" + pegAliasUSDCClassic} {
		if callIndex(hist.pairs, want) < 0 {
			t.Errorf("tip window did not consult %q; consulted %v", want, hist.pairs)
		}
	}
}

// TestPriceTip_ThinPoolThirdAlias_SACKeyedRequestMergesTheNamedPool: a
// caller who names the SAC forms is asking about that pool and gets its
// window VWAP — unchanged behaviour, and the proof that the pool is
// reachable at all (the two tests above are not passing against a pair
// the stub could never have returned).
func TestPriceTip_ThinPoolThirdAlias_SACKeyedRequestMergesTheNamedPool(t *testing.T) {
	installPegAliasRegistry(t)
	now := time.Now().UTC()
	hist := &recordingHistoryReader{stubHistoryReader: &stubHistoryReader{trades: []canonical.Trade{
		mkPairTrade(t, pegAliasAquaSAC, pegAliasUSDCSAC, now.Add(-2*time.Second), 10_000_000, 50_000, "soroswap"),
	}}}
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}, History: hist})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset="+pegAliasAquaSAC+"&quote="+pegAliasUSDCSAC)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.0050000000"`) || !strings.Contains(body, `"sources":["soroswap"]`) {
		t.Errorf("SAC-keyed tip: want the named pool's own 0.0050000000 from soroswap: %s", body)
	}
	if callIndex(hist.pairs, pegAliasAquaSAC+"/"+pegAliasUSDCSAC) < 0 {
		t.Errorf("tip window never read the named SAC pair; consulted %v", hist.pairs)
	}
}

// TestPriceTip_ThinPoolThirdAlias_SorobanOnlyWrappedClassicServesThePoolLast:
// a wrapped classic with NO classic venue — no SDEX trade in the window
// and no closed bucket for the classic pair — whose Soroban SAC/SAC pool
// printed 2s ago. The classic-keyed tip serves that print, from the pool
// (the alternative is 404), and reaches it only AFTER the established
// combinations' window at both bounds and the closed-bucket read have
// missed.
//
// RED with the SAC set dropped instead of read last: 404.
func TestPriceTip_ThinPoolThirdAlias_SorobanOnlyWrappedClassicServesThePoolLast(t *testing.T) {
	installPegAliasRegistry(t)
	now := time.Now().UTC()
	log := &tipCallLog{}
	hist := &recordingHistoryReader{stubHistoryReader: &stubHistoryReader{trades: []canonical.Trade{
		mkPairTrade(t, pegAliasAquaSAC, pegAliasUSDCSAC, now.Add(-2*time.Second), 10_000_000, 50_000, "soroswap"),
	}}, log: log}
	prices := &tipOrderPriceReader{stubPriceReader: &stubPriceReader{}, log: log}
	srv := v1.New(v1.Options{Prices: prices, History: hist})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset="+pegAliasAquaClassic+"&quote="+pegAliasUSDCClassic)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a wrapped classic whose only market is its pool must still serve from it", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.0050000000"`) || !strings.Contains(body, `"sources":["soroswap"]`) {
		t.Errorf("want the pool's own 0.0050000000 from soroswap: %s", body)
	}
	// Order: the classic window (5s, then the 30s escalation), then the
	// closed-bucket read, and only then the first SAC-form window read.
	classicPair := pegAliasAquaClassic + "/" + pegAliasUSDCClassic
	lastClassicWindow := lastIndexWhere(log.calls, func(c string) bool { return c == "trades:"+classicPair })
	closedBucket := firstIndexWhere(log.calls, func(c string) bool { return strings.HasPrefix(c, "latest:") })
	firstSACWindow := firstIndexWhere(log.calls, func(c string) bool {
		return strings.HasPrefix(c, "trades:") && sacFormPair(strings.TrimPrefix(c, "trades:"))
	})
	if lastClassicWindow < 0 || closedBucket < 0 || firstSACWindow < 0 {
		t.Fatalf("want the classic window, the closed-bucket read and a SAC window read; got %v", log.calls)
	}
	if lastClassicWindow > closedBucket || closedBucket > firstSACWindow {
		t.Errorf("SAC-form combinations must be read LAST — after the classic window and the closed-bucket read: %v", log.calls)
	}
}

// TestPriceTip_ThinPoolThirdAlias_DeeperPoolNeverBlendsIntoTheClassicBook:
// the wrapped classic's DEEPER venue is the Soroban pool — ten prints in
// the window against one SDEX trade. The classic-keyed tip is the SDEX
// book alone. The exclusion is by form, not by depth: depth on a pool is
// exactly what an attacker can manufacture, and the classic-quoted
// answer is the classic book's, as on /v1/price.
//
// RED before tipMergePairs: a blended VWAP with sources [sdex soroswap].
func TestPriceTip_ThinPoolThirdAlias_DeeperPoolNeverBlendsIntoTheClassicBook(t *testing.T) {
	installPegAliasRegistry(t)
	now := time.Now().UTC()
	trades := []canonical.Trade{
		// SDEX: one trade, 1 AQUA for 0.001 USDC.
		mkPairTrade(t, pegAliasAquaClassic, pegAliasUSDCClassic, now.Add(-3*time.Second), 10_000_000, 10_000, "sdex"),
	}
	for i := 1; i <= 10; i++ {
		// The pool: ten prints at five times the book.
		trades = append(trades, mkPairTrade(t, pegAliasAquaSAC, pegAliasUSDCSAC,
			now.Add(-time.Duration(i)*100*time.Millisecond), 10_000_000, 50_000, "soroswap"))
	}
	hist := &recordingHistoryReader{stubHistoryReader: &stubHistoryReader{trades: trades}}
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}, History: hist})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset="+pegAliasAquaClassic+"&quote="+pegAliasUSDCClassic)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.0010000000"`) {
		t.Errorf("price: want the SDEX-only window VWAP 0.0010000000 — the deeper pool is not a reason to merge it: %s", body)
	}
	if !strings.Contains(body, `"sources":["sdex"]`) {
		t.Errorf("sources: want [sdex]: %s", body)
	}
	assertNoSACPairConsulted(t, hist.pairs)
}

// TestPriceTip_FallbackBranchesOmitWindowSeconds pins the tip surface's
// window_seconds contract: it names the rolling window the tip was
// computed over, clamped to [1,60]. A fallback is not that window — the
// Redis VWAP cache is a 300s read and the closed bucket a 60s one — so
// every fallback branch omits the field rather than report its source's
// own resolution as if it were the caller's window.
func TestPriceTip_FallbackBranchesOmitWindowSeconds(t *testing.T) {
	cases := []struct {
		name string
		opts v1.Options
		url  string
	}{
		{
			// Only the fiat:USD leg is cached; BRL is derived from it.
			name: "usd_anchored_fiat_cross_over_cached_usd_leg",
			opts: v1.Options{
				Prices:       &stubPriceReader{err: v1.ErrPriceNotFound},
				History:      &stubHistoryReader{},
				Triangulated: usdOnlyVWAPLooker{value: "0.2"},
				Currencies:   brlCurrencies(),
			},
			url: "/v1/price/tip?asset=native&quote=fiat:BRL",
		},
		{
			name: "direct_redis_vwap_cache",
			opts: v1.Options{
				Prices:       &stubPriceReader{err: v1.ErrPriceNotFound},
				History:      &stubHistoryReader{},
				Triangulated: usdOnlyVWAPLooker{value: "0.2"},
			},
			url: "/v1/price/tip?asset=native&quote=fiat:USD&window_seconds=5",
		},
		{
			name: "closed_bucket",
			opts: v1.Options{
				Prices: &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{
					"native/fiat:USD": {
						AssetID: "native", Quote: "fiat:USD", Price: "0.2",
						PriceType: "vwap", WindowSeconds: 60,
					},
				}},
				History: &stubHistoryReader{},
			},
			url: "/v1/price/tip?asset=native&quote=fiat:USD&window_seconds=5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := startHTTPTest(t, v1.New(tc.opts).Handler())
			resp := mustGet(t, ts.URL+tc.url)
			body, _ := readAll(resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
			}
			if strings.Contains(body, `"window_seconds"`) {
				t.Errorf("a fallback tip must omit window_seconds — it was not "+
					"computed over the caller's [1,60] rolling window: %s", body)
			}
		})
	}
}

func TestPriceTip_Withheld_Distinct404Type(t *testing.T) {
	// The reader HAS a snapshot for the pair — proving the tip verdict
	// comes from the gate, not from data absence.
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {Price: "0.12", PriceType: "vwap"},
		},
	}
	gate := &stubSubstanceGate{allow: false}
	srv := v1.New(v1.Options{Prices: reader, Substance: gate})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body missing price-withheld problem type: %s", body)
	}
	if len(gate.surfaces) == 0 || gate.surfaces[0] != "tip" {
		t.Errorf("gate consulted with surfaces %v, want [tip ...]", gate.surfaces)
	}
}

func TestPriceTip_GateAllows_Serves(t *testing.T) {
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {Price: "0.12", PriceType: "vwap"},
		},
	}
	srv := v1.New(v1.Options{Prices: reader, Substance: &stubSubstanceGate{allow: true}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 200: %s", resp.StatusCode, body)
	}
}

// TestPriceTip_Withheld_ScamIssuerReasonInDetail — a scam-issuer
// withhold and a substance-gate withhold both 404 with the same problem
// TYPE, but the DETAIL text must name the gate that actually fired: otherwise
// writePriceWithheldProblem would hard-code the substance gate's wording for
// every withheld cause, so a directory-flagged issuer's response would claim
// "trailing market activity is below the serve floor" — a reason that
// never fired. Driven through the real *pricingguard.ScamGate, following
// the /v1/vwap and /v1/price/tip quote-leg suites, so the pair-aware
// primitive is what's proved, not a hand-written fake.
func TestPriceTip_Withheld_ScamIssuerReasonInDetail(t *testing.T) {
	flagged, err := canonical.NewClassicAsset("RIO", scamQuoteLegIssuer)
	if err != nil {
		t.Fatalf("build flagged classic: %v", err)
	}
	reader := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/" + flagged.String(): {Price: "0.12", PriceType: "vwap"},
		},
	}
	dir := &scamQuoteLegDirectory{flagged: map[string]bool{scamQuoteLegIssuer: true}}
	srv := v1.New(v1.Options{
		Prices: reader,
		Scam:   pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{}),
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote="+flagged.String())
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 404: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "directory-flagged issuer") {
		t.Errorf("scam-gate withheld detail must name the flagged issuer as the cause: %s", body)
	}
	if strings.Contains(string(body), "trailing market activity is below the serve floor") {
		t.Errorf("scam-gate withheld body must not reuse the substance gate's wording — the reason must discriminate: %s", body)
	}
}

// usdOnlyVWAPLooker is a Redis VWAP cache holding only X/fiat:USD, so a
// non-USD fiat quote can only be served through the USD-anchored cross.
type usdOnlyVWAPLooker struct{ value string }

func (l usdOnlyVWAPLooker) LookupTriangulatedVWAP(
	_ context.Context, _, quote canonical.Asset, _ time.Duration,
) (v1.CachedVWAP, bool, error) {
	if quote.String() != "fiat:USD" {
		return v1.CachedVWAP{}, false, nil
	}
	return v1.CachedVWAP{Value: l.value, ObservedAt: time.Now().UTC()}, true, nil
}

// stubSubstanceGate implements v1.PriceSubstanceGate for the tip path.
type stubSubstanceGate struct {
	allow    bool
	surfaces []string
}

func (g *stubSubstanceGate) Allowed(_ context.Context, _, _ canonical.Asset, surface string) bool {
	g.surfaces = append(g.surfaces, surface)
	return g.allow
}

func (g *stubSubstanceGate) Probe(ctx context.Context, base, quote canonical.Asset) (allowed, measured bool, floor pricingguard.SubstanceFloor) {
	return g.Allowed(ctx, base, quote, "probe"), true, pricingguard.FloorNone
}

// tipCallLog is the order in which the tip consulted its readers — one
// entry per TradesInRange ("trades:<pair>") and LatestPrice
// ("latest:<pair>") call — so a test can prove not just WHICH alias
// combinations were read but WHEN, relative to the closed-bucket read.
type tipCallLog struct{ calls []string }

func firstIndexWhere(calls []string, pred func(string) bool) int {
	for i, c := range calls {
		if pred(c) {
			return i
		}
	}
	return -1
}

func lastIndexWhere(calls []string, pred func(string) bool) int {
	for i := len(calls) - 1; i >= 0; i-- {
		if pred(calls[i]) {
			return i
		}
	}
	return -1
}

// sacFormPair reports whether either side of a "<base>/<quote>" string
// is a Soroban (SAC) form.
func sacFormPair(p string) bool {
	for _, side := range strings.SplitN(p, "/", 2) {
		if a, err := canonical.ParseAsset(side); err == nil && a.Type == canonical.AssetSoroban {
			return true
		}
	}
	return false
}

// recordingHistoryReader wraps stubHistoryReader (whose TradesInRange
// already filters the fixture by each trade's own Pair) and records every
// pair TradesInRange was asked for, so a test can prove which alias
// combinations the tip consulted.
type recordingHistoryReader struct {
	*stubHistoryReader
	pairs []string
	log   *tipCallLog
}

func (r *recordingHistoryReader) TradesInRange(ctx context.Context, pair canonical.Pair, from, to time.Time, limit int) ([]canonical.Trade, error) {
	key := pair.Base.String() + "/" + pair.Quote.String()
	r.pairs = append(r.pairs, key)
	if r.log != nil {
		r.log.calls = append(r.log.calls, "trades:"+key)
	}
	return r.stubHistoryReader.TradesInRange(ctx, pair, from, to, limit)
}

// tipOrderPriceReader wraps stubPriceReader and records every
// LatestPrice call into the shared tipCallLog, so the closed-bucket
// read's place in the tip's order is observable.
type tipOrderPriceReader struct {
	*stubPriceReader
	log *tipCallLog
}

func (r *tipOrderPriceReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	r.log.calls = append(r.log.calls, "latest:"+a.String()+"/"+q.String())
	return r.stubPriceReader.LatestPrice(ctx, a, q)
}

// mkPairTrade builds one on-chain trade on an explicit pair: base and
// quote are stroop-scale integers (7dp), so (10_000_000, 50_000) is one
// unit of base for 0.005 units of quote.
func mkPairTrade(t *testing.T, base, quote string, ts time.Time, baseAmt, quoteAmt int64, source string) canonical.Trade {
	t.Helper()
	b, err := canonical.ParseAsset(base)
	if err != nil {
		t.Fatalf("ParseAsset(%s): %v", base, err)
	}
	q, err := canonical.ParseAsset(quote)
	if err != nil {
		t.Fatalf("ParseAsset(%s): %v", quote, err)
	}
	pair, err := canonical.NewPair(b, q)
	if err != nil {
		t.Fatalf("NewPair(%s/%s): %v", base, quote, err)
	}
	return canonical.Trade{
		Source:      source,
		Ledger:      1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(baseAmt)),
		QuoteAmount: canonical.NewAmount(big.NewInt(quoteAmt)),
	}
}

func assertNoSACPairConsulted(t *testing.T, pairs []string) {
	t.Helper()
	for _, p := range pairs {
		if sacFormPair(p) {
			t.Errorf("tip window consulted %q — a SAC-form combination the caller did not name", p)
		}
	}
}
