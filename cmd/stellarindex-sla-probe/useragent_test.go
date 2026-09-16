// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// The API decides what counts as customer traffic by User-Agent prefix:
// obs.IsSyntheticUA keeps first-party probes out of the availability
// ratio, out of the latency histogram, and out of the access log's INFO
// stream. `stellarindex-probe/` is the prefix it reserves for operator
// probes — and for as long as this binary sent no User-Agent at all, Go
// supplied `Go-http-client/1.1` and every one of its requests was
// counted as a customer's.
//
// That is not a cosmetic mislabel. This probe drives ~800 requests per
// endpoint per run across ten endpoints every 15 minutes, which on a
// deployment with no consumer traffic is essentially the entire
// denominator of the published availability figure, and is enough to
// clear the burn alerts' own "don't burn on synthetic traffic" floor of
// 5 req/s.
//
// This test asserts the two ends agree, against the real request the
// probe builds rather than against a copy of the header string.
func TestProbeIdentifiesItselfAsSynthetic(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.UserAgent()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, ok, _ := hit(ctx, client, srv.URL, "", endpoint{Path: "/v1/price"}); !ok {
		t.Fatalf("probe request to the stub did not succeed")
	}

	if got == "" {
		t.Fatal("the probe sent no User-Agent — the API cannot tell its load from a customer's")
	}
	if !obs.IsSyntheticUA(got) {
		t.Errorf("User-Agent %q is not recognised as synthetic; its requests would land in the "+
			"customer-facing availability ratio and latency histogram", got)
	}
}
