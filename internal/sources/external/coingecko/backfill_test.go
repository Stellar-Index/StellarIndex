// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package coingecko

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func testPair(t *testing.T) canonical.Pair {
	t.Helper()
	base, err := canonical.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	quote, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("parse quote: %v", err)
	}
	return canonical.Pair{Base: base, Quote: quote}
}

// The window refusal is the case that decides a PURCHASE, so it must be
// distinguishable from a broken venue rather than folded into a generic
// error. CoinGecko serves it as a JSON body (under 200 or 401 depending on
// tier), which is why the code reads the body before the status.
func TestBackfillRange_FreeTierWindowRefusalIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK) // 200-shaped, error in the body
		_, _ = w.Write([]byte(`{"error":{"status":{"error_code":10012,"error_message":"Your request exceeds the allowed time range."}}}`))
	}))
	defer srv.Close()

	p := NewPoller()
	p.Endpoint = srv.URL
	from := time.Date(2017, 11, 1, 0, 0, 0, 0, time.UTC)
	got, err := p.BackfillRange(context.Background(), testPair(t), from, from.AddDate(0, 1, 0))
	if !errors.Is(err, ErrOutsideFreeTier) {
		t.Fatalf("want ErrOutsideFreeTier so the caller can tell a tier limit from an outage; got %v", err)
	}
	if got != nil {
		t.Errorf("want no updates on refusal, got %d", len(got))
	}
}

func TestBackfillRange_EmitsOracleUpdatesInsideTheWindow(t *testing.T) {
	from := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 2)
	// Three points: one before the window, one inside, one at `to` (which
	// is exclusive). Only the middle one may survive.
	body := `{"prices":[[` +
		itoaMillis(from.Add(-time.Hour)) + `,0.25],[` +
		itoaMillis(from.Add(6*time.Hour)) + `,0.2655],[` +
		itoaMillis(to) + `,0.30]]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := NewPoller()
	p.Endpoint = srv.URL
	got, err := p.BackfillRange(context.Background(), testPair(t), from, to)
	if err != nil {
		t.Fatalf("BackfillRange: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want exactly the in-window point (the API clamps to its own bucket edges and can overshoot); got %d", len(got))
	}
	u := got[0]
	if u.Source != SourceName {
		t.Errorf("source = %q, want %q", u.Source, SourceName)
	}
	// The provenance boundary: an index price carries no chain identity, and
	// must not pretend to. If these ever become non-zero this has drifted
	// toward looking like a venue fill.
	if u.Ledger != 0 || u.OpIndex != 0 || u.ContractID != "" {
		t.Errorf("index observation must carry no chain provenance; got ledger=%d op=%d contract=%q", u.Ledger, u.OpIndex, u.ContractID)
	}
	if u.TxHash == "" {
		t.Error("want the synthetic tx hash so a backfilled and a polled observation for the same second collapse rather than double-count")
	}
	if !u.Timestamp.Equal(from.Add(6 * time.Hour)) {
		t.Errorf("timestamp = %s, want %s", u.Timestamp, from.Add(6*time.Hour))
	}
}

// A backfilled point and a live-polled point for the same (ticker, currency,
// second) must produce the SAME synthetic hash, or a re-run double-counts.
func TestBackfillRange_TxHashMatchesTheLivePath(t *testing.T) {
	ts := time.Date(2025, 6, 1, 6, 0, 0, 0, time.UTC)
	body := `{"prices":[[` + itoaMillis(ts) + `,0.2655]]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	p := NewPoller()
	p.Endpoint = srv.URL
	got, err := p.BackfillRange(context.Background(), testPair(t), ts.Add(-time.Hour), ts.Add(time.Hour))
	if err != nil {
		t.Fatalf("BackfillRange: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 update, got %d", len(got))
	}
	if want := syntheticTxHash("XLM", "usd", ts.Unix()); got[0].TxHash != want {
		t.Errorf("tx hash = %q, want the live path's %q — a mismatch double-counts on re-run", got[0].TxHash, want)
	}
}

func TestBackfillRange_RejectsAnEmptyWindow(t *testing.T) {
	p := NewPoller()
	now := time.Now().UTC()
	if _, err := p.BackfillRange(context.Background(), testPair(t), now, now); err == nil {
		t.Error("want an error for an empty window rather than a silent zero-row success")
	}
}

func itoaMillis(t time.Time) string {
	return timeMillisString(t)
}
