package v1_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/StellarIndex/stellar-index/internal/api/v1"
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

// TestVWAP_NonstandardDecimals_Declines proves the same guard fires on
// /v1/vwap's choke point (base/quote resolved via parseBaseQuote).
func TestVWAP_NonstandardDecimals_Declines(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 18)
	srv := v1.New(v1.Options{
		History:             &stubHistoryReader{}, // must never be reached
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/vwap?base="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// TestHistory_NonstandardDecimals_Declines proves the guard fires on
// /v1/history before TradesInRangeAfter is ever called.
func TestHistory_NonstandardDecimals_Declines(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	reader := &stubHistoryReader{}
	srv := v1.New(v1.Options{
		History:             reader,
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/history?base="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if reader.lastCall.limit != 0 {
		t.Error("TradesInRangeAfter appears to have been called — the guard must short-circuit before any storage read")
	}
}

// TestOHLC_NonstandardDecimals_Declines proves the guard fires on
// /v1/ohlc for both the single-bar and interval=series modes (same
// resolved pair, same call site).
func TestOHLC_NonstandardDecimals_Declines(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	srv := v1.New(v1.Options{
		History:             &stubHistoryReader{},
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	for _, url := range []string{
		ts.URL + "/v1/ohlc?base=" + flaggedAsset + "&quote=fiat:USD",
		ts.URL + "/v1/ohlc?base=" + flaggedAsset + "&quote=fiat:USD&interval=1h",
	} {
		resp := mustGet(t, url)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", url, resp.StatusCode)
		}
	}
}

// TestTWAP_NonstandardDecimals_Declines proves the guard fires on
// /v1/twap too — TWAP over mis-scaled trades is exactly as wrong as
// VWAP over them (integration review 2026-07-09; the original guard
// omitted /v1/twap only because the founding incident's pair had no
// TWAP consumers).
func TestTWAP_NonstandardDecimals_Declines(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	srv := v1.New(v1.Options{
		History:             &stubHistoryReader{},
		NonstandardDecimals: cache,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/twap?base="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}
