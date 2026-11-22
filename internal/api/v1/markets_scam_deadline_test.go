package v1_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// flagAllDirectory flags every address, and errors like Postgres once ctx is done.
type flagAllDirectory struct{}

func (flagAllDirectory) DirectoryEntryByAddress(ctx context.Context, _ string) (timescale.DirectoryEntry, bool, error) {
	if err := ctx.Err(); err != nil {
		return timescale.DirectoryEntry{}, false, err
	}
	return timescale.DirectoryEntry{Tags: []string{"unsafe"}}, true, nil
}

// lateMarketsReader answers successfully only once the handler's budget has run out: the read that lands
// at 7.9s of 8, leaving the scam gate a dead context.
type lateMarketsReader struct{ *stubMarketsReader }

func (r lateMarketsReader) DistinctPairsExt(ctx context.Context, c string, l int, o timescale.MarketsOrder) ([]v1.Market, string, error) {
	<-ctx.Done()
	return r.stubMarketsReader.DistinctPairsExt(ctx, c, l, o) // the stub ignores ctx
}

func (r lateMarketsReader) AllPools(ctx context.Context, f timescale.PoolsFilter, c string, l int, o timescale.MarketsOrder) ([]v1.Pool, string, error) {
	<-ctx.Done()
	return r.stubMarketsReader.AllPools(ctx, f, c, l, o)
}

func (r lateMarketsReader) PairMarket(ctx context.Context, b, q canonical.Asset) (v1.Market, bool, error) {
	<-ctx.Done()
	return r.stubMarketsReader.PairMarket(ctx, b, q)
}

// TestListingsNeverServeAnUngatedPageAfterTheDeadline: the scam gate fails open on a dead context, so a listing
// whose budget expired before gating finished must not go out as a clean 200 carrying a flagged pair's price.
func TestListingsNeverServeAnUngatedPageAfterTheDeadline(t *testing.T) {
	price := "1.5"
	rows := []v1.Market{{Base: fallbackFlaggedAsset, Quote: "native", LastPrice: &price, TradeCount24h: 3}}
	reader := lateMarketsReader{&stubMarketsReader{pairs: rows, pair: rows[0], pairFound: true}}
	gate := pricingguard.NewScamGate(flagAllDirectory{}, pricingguard.ScamGateOptions{})
	h := v1.New(v1.Options{Markets: reader, Scam: gate}).Handler()

	for _, path := range []string{"/v1/markets", "/v1/pools", "/v1/pairs?base=" + fallbackFlaggedAsset + "&quote=native"} {
		t.Run(path, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("GET %s after the deadline = %d (%s); want the retryable 503, the scam gate never ran", path, rec.Code, rec.Body.String())
			}
		})
	}
}
