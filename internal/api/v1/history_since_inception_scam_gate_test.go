// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// /v1/chart withheld a directory-scam-flagged issuer's price SERIES
// (#366) while /v1/history/since-inception served the identical
// trajectory at 200 — the same pair, the same CAGG VWAP chain (the
// handler's own comment says so), differing only in the read closure.
// Gating one and not the other bought nothing: `?timeframe=all` and
// since-inception answer the same question (audit-2026-09-02 T012).
//
// scam.go promises the RAW surfaces stay visible — /v1/history's trade
// rows, /v1/observations, /v1/ohlc — and that promise is kept. The
// distinction is raw trades versus an AGGREGATED price claim, not the
// route prefix: this endpoint is named for the raw family but serves
// bucketed VWAP.

// A flagged base must be withheld — and withheld BEFORE the read, so a
// flagged issuer's series never leaves the store.
func TestHistorySinceInception_WithholdsScamFlaggedIssuer(t *testing.T) {
	base := chartFlaggedBase(t)
	gate := &chartScamGate{withheld: map[string]bool{base.String(): true}}
	reader := &stubHistoryReader{points: []v1.HistoryPoint{}}
	srv := v1.New(v1.Options{History: reader, Scam: gate})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset="+base.String()+"&quote=native&granularity=1d")
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — a flagged issuer's VWAP series must be withheld here "+
			"exactly as it is on /v1/chart?timeframe=all, which reads the same chain. Body: %s",
			resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body missing the price-withheld problem type: %s", body)
	}
	if len(gate.surfaces) == 0 || gate.surfaces[0] != "history_series" {
		t.Errorf("gate surfaces = %v, want the first to be \"history_series\" — the label is how an "+
			"operator attributes a withholding decision, and reusing \"chart\" would hide this "+
			"surface inside the other's counter", gate.surfaces)
	}
	if reader.lastCall.granularity != "" {
		t.Errorf("HistoryPoints was called (granularity=%q) despite the withholding — the gate must "+
			"short-circuit BEFORE the read", reader.lastCall.granularity)
	}
}

// Blast-radius guard: an UNFLAGGED pair must still be served in full.
func TestHistorySinceInception_ServesUnflaggedIssuer(t *testing.T) {
	gate := &chartScamGate{withheld: map[string]bool{}} // wired, flags nothing
	reader := &stubHistoryReader{points: []v1.HistoryPoint{}}
	srv := v1.New(v1.Options{History: reader, Scam: gate})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset=native&quote="+w2t2USDC+"&granularity=1d")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the gate must withhold only flagged bases. Body: %s",
			resp.StatusCode, body)
	}
	if reader.lastCall.granularity != "1d" {
		t.Errorf("HistoryPoints granularity = %q, want 1d — the read must still happen for an "+
			"unflagged pair", reader.lastCall.granularity)
	}
}

// A deployment with no gate wired must keep serving; every other gate
// in this package is nil-safe and this call site is not the exception.
func TestHistorySinceInception_NilGateServes(t *testing.T) {
	reader := &stubHistoryReader{points: []v1.HistoryPoint{}}
	srv := v1.New(v1.Options{History: reader}) // no Scam
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset=native&quote="+w2t2USDC+"&granularity=1d")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a nil scam gate broke since-inception: status = %d. Body: %s", resp.StatusCode, body)
	}
}

// The gate keys on the BASE, so a flagged asset cannot slip through by
// naming a different quote — the frontend triangulates through XLM.
func TestHistorySinceInception_GateKeysOnBaseNotQuote(t *testing.T) {
	base := chartFlaggedBase(t)
	gate := &chartScamGate{withheld: map[string]bool{base.String(): true}}
	reader := &stubHistoryReader{points: []v1.HistoryPoint{}}
	srv := v1.New(v1.Options{History: reader, Scam: gate})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset="+base.String()+"&quote="+w2t2USDC+"&granularity=1d")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("a different quote returned %d, want 404 — the gate keys on the BASE", resp.StatusCode)
	}
}

// The RAW trade surface stays visible — the promise scam.go, substance.go
// and the withheld problem's own escape-hatch guidance all make. If this
// ever 404s, the gate was pushed down into the shared reader and our own
// error message became a lie.
func TestHistorySinceInception_RawTradesSurfaceStaysVisible(t *testing.T) {
	base := chartFlaggedBase(t)
	gate := &chartScamGate{withheld: map[string]bool{base.String(): true}}
	srv := v1.New(v1.Options{History: &stubHistoryReader{}, Scam: gate})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history?asset="+base.String()+"&quote=native")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound && strings.Contains(string(body), "price-withheld") {
		t.Errorf("/v1/history (RAW trades) was withheld — scam.go promises the raw surfaces stay "+
			"visible; only the aggregated price claim is gated. Body: %s", body)
	}
}
