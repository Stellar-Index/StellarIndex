package v1_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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

func TestChart_DegradedFailureExitsAreNoStore(t *testing.T) {
	fxFail := &stubFXHistoryReader{err: errors.New("fx_quotes: broke")}
	cases := []struct {
		name, path string
		opts       v1.Options
	}{
		{
			"fiat fx failure", "/v1/chart?asset=fiat:EUR&quote=fiat:USD&timeframe=1y&granularity=1d",
			v1.Options{History: &stubHistoryReader{}, FXHistory: fxFail},
		},
		{
			"fiat-cross fx failure", "/v1/chart?asset=fiat:EUR&quote=fiat:GBP&timeframe=1y&granularity=1d",
			v1.Options{History: &stubHistoryReader{}, FXHistory: fxFail},
		},
		{
			"market cap price read failure", "/v1/chart?asset=native&quote=fiat:USD&price_type=market_cap&timeframe=1y&granularity=1d",
			v1.Options{History: &stubHistoryReader{pointsErr: errChartReadBroke}, Supply: &stubSupplyLooker{}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.VerifiedCurrencies = newTestCatalogue(t)
			ts := httpTestServer(t, v1.New(tc.opts))
			assertNoStore(t, mustGet(t, ts.URL+tc.path))
		})
	}
}

func TestHandleProtocolTVL_CarriedForwardIsNoStore(t *testing.T) {
	reader := &countingAquariusReader{pools: []timescale.AquariusPoolReserve{aquariusPool(
		"CBQDHNBFBZYE4MECPHNQCLM7F5FRZ4R7HZWQZXAK7NZYYUR3ILWSKDMV", 7, 400_000_000)}}
	cache := v1.NewDEXTVLCache(v1.DEXTVLSources{AquariusReserves: reader, Pricer: stubTVLPricerT{}})
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	reader.err = errors.New("lake unavailable")
	_ = cache.Refresh(context.Background())
	ts := httpTestServer(t, v1.New(v1.Options{DEXTVL: cache}))
	assertNoStore(t, mustGet(t, ts.URL+"/v1/protocols/aquarius/tvl"))
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
