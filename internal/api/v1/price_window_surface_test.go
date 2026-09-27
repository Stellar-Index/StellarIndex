// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// ADR-0018 §"URL discipline": a query parameter must not change a
// surface's consistency contract, and a request whose intent does not
// match the URL's contract returns 400. /v1/price is the closed-bucket
// surface; `?window=300|3600|86400` used to serve the aggregator's
// rolling per-tick VWAP under that URL, which is the tip contract.
// Rolling windows belong on /v1/price/tip. GH-762 retired the parameter.
func TestPriceWindow_DoesNotSelectARollingSurface(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD&window=300")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400: a rolling window is a tip-surface request and /v1/price is closed-bucket", resp.StatusCode)
	}
}
