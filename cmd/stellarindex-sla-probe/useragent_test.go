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

// obs.IsSyntheticUA classifies traffic by the reserved `stellarindex-probe/` prefix; Go's default UA would count as customer
// traffic and dominate the availability denominator. This asserts the probe's real request matches that prefix, not a copy of
// the header string.
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
