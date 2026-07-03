// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// fillCoinsStub returns n classic rows and records the options it was
// called with (so the test can assert Q pass-through).
type fillCoinsStub struct {
	CoinsReader // nil — only ListCoinsExt is called on this path
	n           int
	lastOpts    timescale.ListCoinsOptions
}

func (s *fillCoinsStub) ListCoinsExt(_ context.Context, opts timescale.ListCoinsOptions) ([]timescale.CoinRow, error) {
	s.lastOpts = opts
	rows := make([]timescale.CoinRow, 0, s.n)
	for i := 0; i < s.n && i < opts.Limit; i++ {
		rows = append(rows, timescale.CoinRow{AssetID: "TOK" + string(rune('A'+i)) + "-GBASE", Code: "TOK"})
	}
	return rows, nil
}

// TestUnifiedPage1Fill pins S-002: when the catalogue phase is shorter
// than the requested limit, page 1 fills the remainder from the
// classic stream instead of returning the catalogue tail alone.
func TestUnifiedPage1Fill(t *testing.T) {
	stub := &fillCoinsStub{n: 50}
	s := &Server{coins: stub}
	// No verifiedCurrencies wired → catalogue phase empty → the fill
	// path must still serve `limit` classic rows on page 1.
	req := httptest.NewRequest(http.MethodGet, "/v1/assets?asset_class=all&limit=25&q=tok", nil)
	rec := httptest.NewRecorder()
	s.serveCatalogueUnifiedPage(rec, req, 25, "")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data) != 25 {
		t.Fatalf("page-1 rows = %d, want 25 (the fill)", len(env.Data))
	}
	if stub.lastOpts.Q != "tok" {
		t.Fatalf("Q pass-through = %q, want %q (S-011: search was server-ignored)", stub.lastOpts.Q, "tok")
	}
}
