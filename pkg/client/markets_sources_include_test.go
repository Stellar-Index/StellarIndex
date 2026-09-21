// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/Stellar-Index/StellarIndex/pkg/client"
)

// TestSources_IncludeSendsCommaJoinedParam — T532. [Source] documents
// TradeCount24h/VolumeUSD24h/MarketsCount24h/VolumeHistory24h as
// populated only when the request used `?include=stats` (etc), but
// pre-fix SourcesOptions had no way to set it — the SDK could never
// request those fields.
func TestSources_IncludeSendsCommaJoinedParam(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("include"); got != "stats,sparkline" {
			t.Errorf("include = %q, want %q", got, "stats,sparkline")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [], "as_of": "2026-04-28T10:00:00Z", "flags": {}}`))
	})
	_, err := c.Sources(context.Background(), client.SourcesOptions{
		Include: []string{"stats", "sparkline"},
	})
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
}

// TestMarkets_IncludeSourceAssetQueryParams — T532. MarketsOptions
// had no field for the spec's `include` (sparkline/inception),
// `source`, or `asset` query parameters despite [Market] documenting
// include-gated fields and the OpenAPI spec documenting `source` +
// `asset` filters on GET /v1/markets.
func TestMarkets_IncludeSourceAssetQueryParams(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if got := q.Get("include"); got != "sparkline,inception" {
			t.Errorf("include = %q, want %q", got, "sparkline,inception")
		}
		if got := q.Get("source"); got != "kraken" {
			t.Errorf("source = %q, want %q", got, "kraken")
		}
		if got := q.Get("asset"); got != "native" {
			t.Errorf("asset = %q, want %q", got, "native")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [], "as_of": "2026-04-28T10:00:00Z", "flags": {}}`))
	})
	_, err := c.Markets(context.Background(), client.MarketsOptions{
		Include: []string{"sparkline", "inception"},
		Source:  "kraken",
		Asset:   "native",
	})
	if err != nil {
		t.Fatalf("Markets: %v", err)
	}
}
