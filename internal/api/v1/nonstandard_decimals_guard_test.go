package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/StellarIndex/stellar-index/internal/api/v1"
	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
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

// TestPrice_NonstandardDecimals_DeclinesFlaggedBaseLeg proves /v1/price
// declines (422, no-store, RFC 9457 body naming the leg + its decimals)
// when the requested asset is a confirmed non-7-decimal Soroban token —
// the read-time enforcement half of the dex-nonstandard-decimals guard.
// The PriceReader is never consulted: the guard fires before any storage
// read, so a reader that would panic/error on lookup still proves nothing
// leaked through.
func TestPrice_NonstandardDecimals_DeclinesFlaggedBaseLeg(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	srv := v1.New(v1.Options{
		Prices:              &stubPriceReader{}, // empty: must never be reached
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-store")
	}
	if got := resp.Header.Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
	body, _ := readAll(resp)
	for _, want := range []string{
		flaggedAsset,
		`"status":422`,
		"decimals()=9",
		"nonstandard-decimals",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
}

// TestPrice_NonstandardDecimals_DeclinesFlaggedQuoteLeg proves the guard
// also checks the quote leg, not just the base/asset param.
func TestPrice_NonstandardDecimals_DeclinesFlaggedQuoteLeg(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	srv := v1.New(v1.Options{
		Prices:              &stubPriceReader{},
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote="+flaggedAsset)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
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

// TestOHLC_NonstandardDecimals proves the split behaviour: single-bar
// mode (raw trades, query-time) normalizes and serves 200; interval=
// series mode (prices_<n> CAGG, still unnormalized) keeps declining.
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

	resp = mustGet(t, ts.URL+"/v1/ohlc?base="+flaggedAsset+"&quote=fiat:USD&interval=1h")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("series mode: status = %d, want 422 (still CAGG-backed, still declined)", resp.StatusCode)
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
