// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// pairValidatingPriceReader builds the pair first, as the production store reader does, so an unmapped leg fails the read.
type pairValidatingPriceReader struct{ *stubPriceReader }

func (r pairValidatingPriceReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	if _, err := canonical.NewPair(a, q); err != nil {
		return v1.PriceSnapshot{}, nil, false, err
	}
	return r.stubPriceReader.LatestPrice(ctx, a, q)
}

func (r pairValidatingPriceReader) RecentClosedSnapshots(ctx context.Context, a, q canonical.Asset, n int) ([]v1.PriceSnapshot, error) {
	if _, err := canonical.NewPair(a, q); err != nil {
		return nil, err
	}
	return r.stubPriceReader.RecentClosedSnapshots(ctx, a, q, n)
}

// TestPriceLegRoutes_RawSymbolIsBadRequest: every route that takes a price leg answers an unmapped raw:<symbol> leg with
// 400 invalid-asset-id, as /v1/price does, never a 500 from the read underneath.
func TestPriceLegRoutes_RawSymbolIsBadRequest(t *testing.T) {
	reader := pairValidatingPriceReader{&stubPriceReader{}}
	ts := startHTTPTest(t, v1.New(v1.Options{Prices: reader}).Handler())
	for _, path := range []string{
		"/v1/price?asset=raw:BTC",
		"/v1/oracle/lastprice?asset=raw:BTC",
		"/v1/oracle/prices?asset=raw:BTC&records=5",
		"/v1/oracle/x_last_price?base=raw:BTC&quote=fiat:USD",
		"/v1/oracle/x_last_price?base=native&quote=raw:BTC",
	} {
		status, body := fetch(t, ts.URL+path)
		if status != http.StatusBadRequest || !strings.Contains(string(body), "/errors/invalid-") {
			t.Errorf("%s: status %d body %s, want 400 invalid-asset-id", path, status, body)
		}
	}
}
