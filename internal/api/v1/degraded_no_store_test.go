package v1_test

import (
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// A held (frozen) price or a list missing rows is replaced by the origin's
// next answer once the fault clears, so it must not ride the route's band.

func cacheControlOf(t *testing.T, srv *v1.Server, path string) string {
	t.Helper()
	ts := startHTTPTest(t, srv.Handler())
	resp := mustGet(t, ts.URL+path)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", path, resp.StatusCode)
	}
	return resp.Header.Get("Cache-Control")
}

func TestFrozenPriceServesAreNoStore(t *testing.T) {
	for _, tc := range []struct {
		path   string
		frozen frozenPairs
		held   lkgPairs
		reader func() *stubPriceReader
	}{
		{lastPriceURL, frozenPairs{xlmUSD: true}, lkgPairs{xlmUSD + "/300": heldUSD}, movedUSDBucketReader},
		{xLastURL, frozenPairs{xlmUSD: true}, lkgPairs{xlmUSD + "/300": heldUSD}, movedUSDBucketReader},
		{"/v1/price?asset=crypto:XLM&quote=fiat:GBP", frozenPairs{xlmGBP: true}, lkgPairs{xlmGBP + "/300": heldLKG}, movedBucketReader},
		{"/v1/price/batch?asset_ids=crypto:XLM&quote=fiat:GBP", frozenPairs{xlmGBP: true}, lkgPairs{xlmGBP + "/300": heldLKG}, movedBucketReader},
	} {
		t.Run(tc.path, func(t *testing.T) {
			unfrozen := v1.New(v1.Options{Prices: tc.reader(), Freeze: frozenPairs{}, Triangulated: tc.held})
			if got := cacheControlOf(t, unfrozen, tc.path); got == "no-store" {
				t.Errorf("unfrozen: Cache-Control = %q, want the route band", got)
			}
			frozen := v1.New(v1.Options{Prices: tc.reader(), Freeze: tc.frozen, Triangulated: tc.held})
			if got := cacheControlOf(t, frozen, tc.path); got != "no-store" {
				t.Errorf("frozen: Cache-Control = %q, want no-store", got)
			}
		})
	}
}
