package v1_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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

func assertNoStore(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a degraded 200", cc)
	}
}

// proxyFailingHistory answers the literal pair and fails every proxy
// spelling, so the chart walk finishes degraded rather than erroring.
type proxyFailingHistory struct {
	*stubHistoryReader
	quote string
}

func (h proxyFailingHistory) HistoryPointsInRange(ctx context.Context, p canonical.Pair, g string, from, to time.Time, n int) ([]v1.HistoryPoint, error) {
	if p.Quote.String() != h.quote {
		return nil, errChartReadBroke
	}
	return h.stubHistoryReader.HistoryPointsInRange(ctx, p, g, from, to, n)
}

func (h proxyFailingHistory) HistoryPoints(ctx context.Context, p canonical.Pair, g string, n int) ([]v1.HistoryPoint, error) {
	if p.Quote.String() != h.quote {
		return nil, errChartReadBroke
	}
	return h.stubHistoryReader.HistoryPoints(ctx, p, g, n)
}

func TestHistoryWalk_DegradedIsNoStore(t *testing.T) {
	pts := []v1.HistoryPoint{{Bucket: time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour), VWAP: "0.1"}}
	for _, path := range []string{
		"/v1/chart?asset=native&quote=fiat:USD&timeframe=1y&granularity=1d",
		"/v1/history/since-inception?asset=native&quote=fiat:USD&granularity=1d",
	} {
		t.Run(path, func(t *testing.T) {
			h := proxyFailingHistory{stubHistoryReader: &stubHistoryReader{points: pts}, quote: "fiat:USD"}
			ts := httpTestServer(t, v1.New(v1.Options{History: h, VerifiedCurrencies: newTestCatalogue(t)}))
			resp := mustGet(t, ts.URL+path)
			assertNoStore(t, resp)
		})
	}
}
