// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestHandleSourceHealth_KeptSnapshotIsStale pins that /v1/sources/{name}/health carries the ingestion snapshot's
// staleness: a kept last-good snapshot (degraded, re-freshened each tick) must not read as the live trailing 24h, and a
// fresh build that is degraded only by reads the source rows do not use must not read as last known.
func TestHandleSourceHealth_KeptSnapshotIsStale(t *testing.T) {
	for _, tc := range []struct {
		name      string
		degraded  bool
		age       time.Duration // computedAt - builtAt; the refresher stamps both with one instant on a fresh build
		wantStale bool
	}{
		{"fresh build", false, 0, false},
		{"fresh but degraded build", true, 0, false},
		{"kept last-good snapshot", true, 3 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(Options{})
			snap := IngestionDiagnostics{degraded: tc.degraded}
			snap.Ledger.LatestLedger = 1000
			snap.Sources = []SourceHealthRow{{Name: "binance", TradeCount24h: 41000}}
			now := time.Now()
			srv.ingestionSnapshot.Store(&ingestionSnapshotEntry{
				snap:       snap,
				computedAt: now,
				builtAt:    now.Add(-tc.age),
			})

			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()
			resp, err := http.Get(ts.URL + "/v1/sources/binance/health") //nolint:noctx,gosec // test helper, fixed local URL
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			var env struct {
				Data struct {
					TradeCount24h int64 `json:"trade_count_24h"`
				} `json:"data"`
				Flags struct {
					Stale bool `json:"stale"`
				} `json:"flags"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Data.TradeCount24h != 41000 {
				t.Fatalf("trade_count_24h = %d, want the snapshot row's 41000", env.Data.TradeCount24h)
			}
			if env.Flags.Stale != tc.wantStale {
				t.Errorf("flags.stale = %v, want %v", env.Flags.Stale, tc.wantStale)
			}
		})
	}
}
