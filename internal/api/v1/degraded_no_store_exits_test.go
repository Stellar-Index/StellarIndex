package v1_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

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
