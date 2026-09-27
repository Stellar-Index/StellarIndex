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

// TestHandleDiagnosticsIngestion_StaleSnapshotFallsBackToInlineBuild pins
// the fix for a background refresher that has stopped: a frozen
// ingestionSnapshot must not be served forever. Once its computedAt is
// older than ingestionSnapshotMaxAge, the handler falls back to the
// inline build (the same path used before the refresher's first fire)
// rather than serving the dead snapshot's frozen ledger data.
func TestHandleDiagnosticsIngestion_StaleSnapshotFallsBackToInlineBuild(t *testing.T) {
	srv := New(Options{})

	// Simulate a refresher that fired once, long ago, and then died
	// (e.g. panicked out of its loop) — the snapshot is frozen at a
	// stale, distinguishable LatestLedger value.
	frozen := IngestionDiagnostics{}
	frozen.Ledger.LatestLedger = 999999999
	srv.ingestionSnapshot.Store(&ingestionSnapshotEntry{
		snap:       frozen,
		computedAt: time.Now().Add(-time.Hour),
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/diagnostics/ingestion") //nolint:noctx,gosec // test helper, fixed local URL
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var env struct {
		Data struct {
			Ledger struct {
				LatestLedger int64 `json:"latest_ledger"`
			} `json:"ledger"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.Ledger.LatestLedger == 999999999 {
		t.Errorf("served the frozen snapshot (latest_ledger=999999999) even though it is 1h stale; "+
			"want the handler to fall back to an inline rebuild once computedAt exceeds ingestionSnapshotMaxAge (%s)",
			ingestionSnapshotMaxAge)
	}
}

// TestHandleSourceHealth_StaleSnapshotFallsBackToInlineBuild pins the
// same fix for the sibling call site: handleSourceHealth reads the
// identical s.ingestionSnapshot and must go through freshIngestionSnapshot
// too, or a dead refresher freezes GET /v1/sources/{name}/health forever.
func TestHandleSourceHealth_StaleSnapshotFallsBackToInlineBuild(t *testing.T) {
	srv := New(Options{})

	frozen := IngestionDiagnostics{}
	frozen.Sources = []SourceHealthRow{
		{Name: "soroswap", TradeCount24h: 999999999},
	}
	srv.ingestionSnapshot.Store(&ingestionSnapshotEntry{
		snap:       frozen,
		computedAt: time.Now().Add(-time.Hour),
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/sources/soroswap/health") //nolint:noctx,gosec // test helper, fixed local URL
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var env struct {
		Data struct {
			TradeCount24h int64 `json:"trade_count_24h"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.TradeCount24h == 999999999 {
		t.Errorf("served the frozen snapshot (trade_count_24h=999999999) even though it is 1h stale; "+
			"want the handler to fall back to an inline rebuild once computedAt exceeds ingestionSnapshotMaxAge (%s)",
			ingestionSnapshotMaxAge)
	}
}
