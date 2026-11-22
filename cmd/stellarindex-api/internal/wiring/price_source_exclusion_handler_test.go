package wiring

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// exclHandlerCache is a VWAP cache holding a value for every pair, the layer a withheld closed bucket must not fall to.
type exclHandlerCache struct{}

func (exclHandlerCache) LookupTriangulatedVWAP(context.Context, canonical.Asset, canonical.Asset, time.Duration) (v1.CachedVWAP, bool, error) {
	return v1.CachedVWAP{Value: "0.9753", ObservedAt: time.Now().UTC().Truncate(time.Minute)}, true, nil
}

func exclServe(t *testing.T, store exclPriceStore, gate *pricingguard.SourceExclusionGate, path string) (int, string) {
	t.Helper()
	srv := v1.New(v1.Options{
		Prices:       StorePriceReader{S: store, Now: func() time.Time { return exclNow }, Exclusions: gate},
		Triangulated: exclHandlerCache{},
		Exclusions:   gate,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + path) //nolint:noctx // test server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestPriceHandler_SourceExclusionThroughTheRealReader runs /v1/price over the production reader: a bucket an excluded source
// pooled, including the older bucket the serving guard swaps in for an outlier, is a source_excluded 404 that never falls
// through to the cache layer.
func TestPriceHandler_SourceExclusionThroughTheRealReader(t *testing.T) {
	const path = "/v1/price?asset=native&quote=fiat:USD"
	direct := exclRow(0, "1.1", "kraken", "sdex")
	var swapTrailing []timescale.Vwap1mRow
	for i := 1; i < 16; i++ {
		swapTrailing = append(swapTrailing, exclRow(i, "1.0", "kraken", "sdex"))
	}
	outlier := exclRow(0, "100", "sdex")
	for name, store := range map[string]exclPriceStore{
		"direct bucket":     {latest: &direct, trailing: exclTrailing()},
		"swapped-in bucket": {latest: &outlier, trailing: swapTrailing, recent: swapTrailing},
	} {
		status, body := exclServe(t, store, nil, path)
		if status != http.StatusOK || strings.Contains(body, `"price":"100"`) {
			t.Fatalf("%s, no gate: %d %s, want a served (and, for the outlier, swapped) price", name, status, body)
		}
		status, body = exclServe(t, store, exclGateOver(exclRows{rows: krakenExcluded()}), path)
		if status != http.StatusNotFound || !strings.Contains(body, `"source_excluded"`) || strings.Contains(body, "0.9753") {
			t.Fatalf("%s, kraken excluded: %d %s, want a source_excluded 404 and no cache fallback", name, status, body)
		}
		other := []timescale.SourceExclusion{{ID: 2, Source: "binance", From: exclNow.Add(-time.Hour)}}
		if status, body = exclServe(t, store, exclGateOver(exclRows{rows: other}), path); status != http.StatusOK {
			t.Fatalf("%s, another source excluded: %d %s, want served", name, status, body)
		}
	}
}
