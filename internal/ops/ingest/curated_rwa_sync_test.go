// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	tCuratedC1 = "CBUBVYRKTQLMDRUBPP6SH4GO33KZCEEYBIWB5AWNGKODP4A6KPKM2VJ4"
	tCuratedC2 = "CBOOCGZSVRSZFRE4U2NWR2B4RXYVJWRCBTGOUD2JPI2TDJPWMTJX7FZP"
)

// stubDune serves the three endpoints a run touches. It records the SQL
// and the key header, finishes the execution on the second status poll,
// and pages the rows two per page so the offset loop is exercised.
func stubDune(t *testing.T, rows []map[string]any, state string) (*httptest.Server, *atomic.Int32, *string, *string) {
	t.Helper()
	var polls atomic.Int32
	var gotSQL, gotKey string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/sql/execute", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotSQL, _ = body["sql"].(string)
		gotKey = r.Header.Get("X-Dune-API-Key")
		_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": "01EXEC", "state": "QUERY_STATE_PENDING"})
	})
	mux.HandleFunc("GET /api/v1/execution/01EXEC/status", func(w http.ResponseWriter, _ *http.Request) {
		n := polls.Add(1)
		if n < 2 {
			_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": "01EXEC", "is_execution_finished": false, "state": "QUERY_STATE_EXECUTING"})
			return
		}
		resp := map[string]any{"execution_id": "01EXEC", "is_execution_finished": true, "state": state, "execution_cost_credits": 0.25}
		if state != "QUERY_STATE_COMPLETED" {
			resp["error"] = map[string]any{"type": "FAILED_TYPE_EXECUTION_FAILED", "message": "Column 'x' cannot be resolved"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /api/v1/execution/01EXEC/results", func(w http.ResponseWriter, r *http.Request) {
		offset := 0
		if v := r.URL.Query().Get("offset"); v != "" {
			_, _ = json.Number(v).Int64()
			if n, err := json.Number(v).Int64(); err == nil {
				offset = int(n)
			}
		}
		end := min(offset+2, len(rows))
		page := rows[offset:end]
		resp := map[string]any{
			"is_execution_finished": true, "state": "QUERY_STATE_COMPLETED",
			"result": map[string]any{"rows": page},
		}
		if end < len(rows) {
			resp["next_offset"] = end
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &polls, &gotSQL, &gotKey
}

func TestCuratedRWAFetch_ReadsBothUploadsAndPagesTheResult(t *testing.T) {
	rows := []map[string]any{
		{"address": tCuratedC1, "asset_code": "TPT30", "company": "Realiz", "asset_subclass": "Corporate Credit", "close_usd": "1.1174", "priced_day": "2026-09-15"},
		{"address": tCuratedC2, "asset_code": "eurSAFO", "company": "Spiko", "asset_subclass": "Active Strategies", "close_usd": "1.17", "priced_day": "2026-09-15 00:00:00.000 UTC"},
		{"address": "not-an-address", "company": "Bogus", "close_usd": "9", "priced_day": "2026-09-15"},
		{"address": "GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5", "company": "bare G-address is not a form the table admits"},
		{"address": "BENJI-GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5", "company": "Franklin Templeton", "close_usd": "1.0", "priced_day": ""},
	}
	srv, polls, gotSQL, gotKey := stubDune(t, rows, "QUERY_STATE_COMPLETED")
	c := newCuratedRWAClient(srv.URL, "k-test")
	c.poll = time.Millisecond

	entries, counts, err := c.fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if *gotKey != "k-test" {
		t.Errorf("API key header = %q, want the key from the environment", *gotKey)
	}
	for _, want := range []string{"dune.stellar.dataset_recognized_assets", "dune.stellar.dataset_asset_prices", "asset_class = 'RWA'", "CAST(p.close_usd AS VARCHAR)"} {
		if !strings.Contains(*gotSQL, want) {
			t.Errorf("executed SQL does not contain %q", want)
		}
	}
	if polls.Load() < 2 {
		t.Errorf("status polled %d times, want the loop to wait for is_execution_finished", polls.Load())
	}
	if counts.Rows != 5 || counts.Kept != 3 || counts.Malformed != 2 {
		t.Errorf("counts = %+v, want rows=5 kept=3 malformed=2", counts)
	}
	if counts.Priced != 2 || counts.Unpriced != 1 {
		t.Errorf("counts = %+v, want priced=2 unpriced=1 (a price with no day is dropped, not dated)", counts)
	}
	if counts.Credits != 0.25 || counts.ExecutionID != "01EXEC" {
		t.Errorf("credits/execution = %v/%q, want 0.25/01EXEC from the status endpoint", counts.Credits, counts.ExecutionID)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	// The decimal literal is carried as printed; the day is midnight UTC
	// whichever layout the curator used.
	if entries[0].PriceUSD != "1.1174" || !entries[0].PricedAt.Equal(time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("entry 0 = %+v", entries[0])
	}
	if entries[1].PriceUSD != "1.17" || !entries[1].PricedAt.Equal(time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("entry 1 = %+v", entries[1])
	}
	if entries[2].PriceUSD != "" || !entries[2].PricedAt.IsZero() {
		t.Errorf("entry 2 = %+v, want unpriced", entries[2])
	}
}

func TestCuratedRWAFetch_FailedExecutionIsAnError(t *testing.T) {
	srv, _, _, _ := stubDune(t, nil, "QUERY_STATE_FAILED")
	c := newCuratedRWAClient(srv.URL, "k")
	c.poll = time.Millisecond
	_, counts, err := c.fetch(context.Background())
	if err == nil {
		t.Fatal("a failed execution returned no error")
	}
	if !strings.Contains(err.Error(), "cannot be resolved") {
		t.Errorf("error does not carry the curator's message: %v", err)
	}
	if counts.ExecutionID != "01EXEC" {
		t.Errorf("execution id not recorded on failure: %+v", counts)
	}
}

func TestCuratedRWAPrice_Layouts(t *testing.T) {
	want := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	for _, day := range []string{"2026-09-15", "2026-09-15 00:00:00.000 UTC", "2026-09-15 13:45:00", "2026-09-15T13:45:00Z"} {
		p, d, ok := curatedRWAPrice("1.00", day)
		if !ok || p != "1.00" || !d.Equal(want) {
			t.Errorf("day %q -> (%q, %v, %v), want (1.00, %v, true)", day, p, d, ok, want)
		}
	}
	for _, tc := range [][2]string{{"", "2026-09-15"}, {"1.00", ""}, {"abc", "2026-09-15"}, {"1.00", "yesterday"}} {
		if _, _, ok := curatedRWAPrice(tc[0], tc[1]); ok {
			t.Errorf("(%q, %q) accepted", tc[0], tc[1])
		}
	}
}

func TestCuratedRWASync_RefusesWithoutKeyOrConfig(t *testing.T) {
	// No key is a REFUSAL, not a failure: the run stamps its textfile so
	// the timer's cadence stays measurable, says so on the gauge, and
	// exits clean rather than leaving the unit red every day until an
	// operator sets the key.
	t.Setenv("DUNE_API_KEY", "")
	prom := filepath.Join(t.TempDir(), "curated.prom")
	if err := curatedRWASync([]string{"-config", "/nonexistent.toml", "-textfile", prom}); err != nil {
		t.Errorf("no key: err = %v, want a clean refusal", err)
	}
	if raw, err := os.ReadFile(prom); err != nil || !strings.Contains(string(raw), `stellarindex_curated_rwa_sync_refused{curator="dune:stellar"} 1`) {
		t.Errorf("no key: textfile = %q, %v; want the refused gauge stamped", raw, err)
	}
	t.Setenv("DUNE_API_KEY", "k")
	if err := curatedRWASync(nil); err == nil || !strings.Contains(err.Error(), "-config") {
		t.Errorf("no config: err = %v", err)
	}
	if err := curatedRWASync([]string{"-config", "x", "-base-url", "http://insecure"}); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("http base url: err = %v", err)
	}
}

func TestCuratedRWATextfile_ShapeAndAtomicity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "curated_rwa_sync.prom")
	c := curatedRWACounts{Kept: 42, Priced: 30, Credits: 0.25}
	if err := writeCuratedRWATextfile(path, c, true, false); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, _ := os.ReadFile(path)
	s := string(b)
	for _, want := range []string{
		`stellarindex_curated_rwa_sync_rows{curator="dune:stellar"} 42`,
		`stellarindex_curated_rwa_sync_priced{curator="dune:stellar"} 30`,
		`stellarindex_curated_rwa_sync_execution_cost_credits{curator="dune:stellar"} 0.25`,
		`stellarindex_curated_rwa_sync_written{curator="dune:stellar"} 0`,
		`# TYPE stellarindex_curated_rwa_sync_last_run_unix gauge`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("textfile lacks %q:\n%s", want, s)
		}
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp.*")); len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

// A run with no API key stamps the textfile like any other — the
// staleness alert measures the timer, not the key — and says it refused.
func TestCuratedRWATextfileStampsARefusedRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "curated.prom")
	if err := writeCuratedRWATextfile(path, curatedRWACounts{}, true, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{
		`stellarindex_curated_rwa_sync_refused{curator="dune:stellar"} 1`,
		`stellarindex_curated_rwa_sync_written{curator="dune:stellar"} 0`,
		`stellarindex_curated_rwa_sync_last_run_unix{curator="dune:stellar"} `,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("textfile lacks %q:\n%s", want, got)
		}
	}
}
