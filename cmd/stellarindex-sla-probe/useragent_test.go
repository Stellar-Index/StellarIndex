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

// The API classifies customer traffic by User-Agent prefix: obs.IsSyntheticUA keeps
// first-party probes out of the availability ratio, the latency histogram and the access log's
// INFO stream, and `stellarindex-probe/` is the prefix it reserves for operator probes. A
// probe sending Go's default `Go-http-client/1.1` would be counted as customer traffic and
// dominate the availability denominator, and could clear the burn alerts' synthetic-traffic
// floor.
//
// This test asserts the two ends agree, against the real request the probe builds rather than
// a copy of the header string.
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

	if _, failure, _, _ := hit(ctx, client, srv.URL, "", endpoint{Path: "/v1/price"}); failure != "" {
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
