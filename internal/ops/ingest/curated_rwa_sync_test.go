// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubDuneResults serves GET /api/v1/query/{id}/results for the two
// public queries a run reads. Rows are paged TWO per page regardless of
// the requested limit so the offset loop is exercised; the metadata
// declares the whole result's row count on every page, as the platform
// does. It records the key header and refuses anything that is not a
// GET, so a run that regressed to executing SQL fails here.
func stubDuneResults(t *testing.T, byQuery map[int64][]map[string]any, state string) (*httptest.Server, *atomic.Int32, *string) {
	t.Helper()
	var gets atomic.Int32
	var gotKey string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("%s %s: a run must only READ public results, never execute", r.Method, r.URL.Path)
			http.Error(w, "not a read", http.StatusMethodNotAllowed)
			return
		}
		gets.Add(1)
		gotKey = r.Header.Get("X-Dune-API-Key")
		var id int64
		if _, err := fmtSscanfPath(r.URL.Path, &id); err != nil {
			http.NotFound(w, r)
			return
		}
		rows, ok := byQuery[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		offset := 0
		if n, err := json.Number(r.URL.Query().Get("offset")).Int64(); err == nil {
			offset = int(n)
		}
		end := min(offset+2, len(rows))
		resp := map[string]any{
			"execution_id": "01PUBLIC", "query_id": id, "state": state,
			"is_execution_finished": true,
			"execution_ended_at":    "2026-09-17T04:58:12.345Z",
			"result": map[string]any{
				"rows":     rows[offset:end],
				"metadata": map[string]any{"total_row_count": len(rows), "datapoint_count": 3 * len(rows)},
			},
		}
		if end < len(rows) {
			resp["next_offset"] = end
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &gets, &gotKey
}

// fmtSscanfPath pulls the query id out of /api/v1/query/{id}/results.
func fmtSscanfPath(path string, id *int64) (int, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 5 || parts[2] != "query" || parts[4] != "results" {
		return 0, http.ErrNotSupported
	}
	n, err := json.Number(parts[3]).Int64()
	*id = n
	return 1, err
}

func TestCuratedRWAFetch_ReadsBothPublicResultsAndPages(t *testing.T) {
	byQuery := map[int64][]map[string]any{
		curatedRWAQueryMonthlyTotal: {
			// Out of order, as a query may print them; a decimal literal
			// that a float64 would re-render; a timestamp-shaped month.
			{"month_end": "2025-08-31", "total_rwa_market_cap_usd": json.Number("4004795860.00")},
			{"month_end": "2024-09-30 00:00:00.000 UTC", "total_rwa_market_cap_usd": json.Number("436023166.55")},
			{"month_end": "2025-07-31", "total_rwa_market_cap_usd": json.Number("3900000000.123456789")},
			// The NEWEST month, printed in exponent form — a rendering a
			// query engine picks for a large numeric whenever it likes.
			// This row is the curator's headline; refusing it would
			// publish the series dated to July.
			{"month_end": "2025-09-30", "total_rwa_market_cap_usd": json.Number("4.28e9")},
			{"month_end": "2024-10-31", "total_rwa_market_cap_usd": json.Number("5.135e8")},
		},
		curatedRWAQueryMonthlyBySubclass: {
			{"month_end": "2025-08-31", "asset_subclass": "US Treasuries", "market_cap_usd": 3100000000.0},
			{"month_end": "2025-08-31", "asset_subclass": "Private Credit", "market_cap_usd": 904795860},
			{"month_end": "2025-07-31", "asset_subclass": "US Treasuries", "market_cap_usd": json.Number("3000000000.10")},
			{"month_end": "2025-09-30", "asset_subclass": "US Treasuries", "market_cap_usd": json.Number("3.9E+9")},
			{"month_end": "2025-09-30", "asset_subclass": "Private Credit", "market_cap_usd": json.Number("3.8e8")},
		},
	}
	srv, gets, gotKey := stubDuneResults(t, byQuery, "QUERY_STATE_COMPLETED")
	c := newCuratedRWAClient(srv.URL, "k-test")

	rows, counts, err := c.fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if *gotKey != "k-test" {
		t.Errorf("API key header = %q, want the key from the environment", *gotKey)
	}
	// Five rows at two per page is three GETs per query.
	if gets.Load() != 6 {
		t.Errorf("GETs = %d, want 6 (three pages per query)", gets.Load())
	}
	if counts.Rows != 10 || counts.Kept != 10 || counts.Malformed != 0 {
		t.Errorf("counts = %+v, want rows=10 kept=10 malformed=0 — every row the curator printed is readable", counts)
	}
	// The headline is the NEWEST month, with its exponent form shifted
	// out into the figure the curator published. Refusing that literal
	// dated the whole series to August and understated it by $275M.
	if counts.Months != 5 || counts.LatestMonthEnd != "2025-09-30" || counts.LatestTotalUSD != "4280000000" {
		t.Errorf("headline = %d months, latest %s = %s; want 5 months, 2025-09-30 = 4280000000", counts.Months, counts.LatestMonthEnd, counts.LatestTotalUSD)
	}
	// Datapoints as the platform reported them per result: 3×5 + 3×5.
	if counts.Datapoints != 30 {
		t.Errorf("datapoints = %d, want 30 (what the platform metered, not an execution cost)", counts.Datapoints)
	}
	if want := time.Date(2026, 9, 17, 4, 58, 12, 345000000, time.UTC); !counts.ExecutedAt.Equal(want) {
		t.Errorf("executed_at = %v, want %v from execution_ended_at", counts.ExecutedAt, want)
	}
	if len(rows) != 10 {
		t.Fatalf("rows = %d, want 10", len(rows))
	}
	// The total series comes first, oldest month first, every value the
	// figure the curator printed — verbatim when it printed one plainly,
	// and with the point shifted out when it printed an exponent. No
	// float ever holds it, so the digits are the curator's own.
	total := rows[:5]
	for i, want := range []struct{ month, value string }{
		{"2024-09-30", "436023166.55"},
		{"2024-10-31", "513500000"},
		{"2025-07-31", "3900000000.123456789"},
		{"2025-08-31", "4004795860.00"},
		{"2025-09-30", "4280000000"},
	} {
		r := total[i]
		if r.Series != timescale.CuratedRWASeriesMonthlyTotal || r.MonthEnd.Format("2006-01-02") != want.month ||
			r.ValueUSD != want.value || r.SourceQuery != curatedRWAQueryMonthlyTotal || r.Subclass != "" {
			t.Errorf("total[%d] = %+v, want %s = %s from query %d", i, r, want.month, want.value, curatedRWAQueryMonthlyTotal)
		}
	}
	split := rows[5:]
	if split[0].MonthEnd.Format("2006-01-02") != "2025-07-31" || split[0].Subclass != "US Treasuries" || split[0].ValueUSD != "3000000000.10" {
		t.Errorf("split[0] = %+v", split[0])
	}
	// The split's newest month is exponent-printed too, in both cases.
	if split[3].ValueUSD != "3900000000" || split[4].ValueUSD != "380000000" {
		t.Errorf("split newest month = %q / %q, want 3900000000 / 380000000 — the exponent shifted out, not rounded", split[3].ValueUSD, split[4].ValueUSD)
	}
	for _, r := range split {
		if r.Series != timescale.CuratedRWASeriesMonthlyBySubclass || r.SourceQuery != curatedRWAQueryMonthlyBySubclass || !r.ExecutedAt.Equal(counts.ExecutedAt) {
			t.Errorf("split row = %+v, want the split series from query %d stamped with its execution", r, curatedRWAQueryMonthlyBySubclass)
		}
	}
}

func TestCuratedRWAFetch_RefusesAnIncompleteOrShortResult(t *testing.T) {
	good := []map[string]any{{"month_end": "2025-08-31", "total_rwa_market_cap_usd": 1}}
	splitGood := []map[string]any{{"month_end": "2025-08-31", "asset_subclass": "X", "market_cap_usd": 1}}

	// The latest execution is not completed: nothing is read from it.
	srv, _, _ := stubDuneResults(t, map[int64][]map[string]any{
		curatedRWAQueryMonthlyTotal: good, curatedRWAQueryMonthlyBySubclass: splitGood,
	}, "QUERY_STATE_FAILED")
	if _, _, err := newCuratedRWAClient(srv.URL, "k").fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "QUERY_STATE_FAILED") {
		t.Errorf("failed execution: err = %v, want a refusal naming the state", err)
	}

	// The total parses to nothing: the run fails before it reads the
	// split, and never writes an empty series.
	srv2, gets, _ := stubDuneResults(t, map[int64][]map[string]any{
		curatedRWAQueryMonthlyTotal:      {{"month_end": "not a month", "total_rwa_market_cap_usd": 1}},
		curatedRWAQueryMonthlyBySubclass: splitGood,
	}, "QUERY_STATE_COMPLETED")
	if _, _, err := newCuratedRWAClient(srv2.URL, "k").fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "no usable row") {
		t.Errorf("all-malformed total: err = %v", err)
	}
	if gets.Load() != 1 {
		t.Errorf("GETs = %d after an unusable total, want 1 (the split is not read)", gets.Load())
	}

	// A result that declares more rows than it prints and offers no next
	// page is short, not partial.
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state": "QUERY_STATE_COMPLETED", "execution_ended_at": "2026-09-17T04:58:12Z",
			"result": map[string]any{"rows": good, "metadata": map[string]any{"total_row_count": 3, "datapoint_count": 2}},
		})
	}))
	t.Cleanup(short.Close)
	if _, _, err := newCuratedRWAClient(short.URL, "k").fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "printed 1 of the 3 rows") {
		t.Errorf("short result: err = %v", err)
	}

	// A result past the bound is not the expected query.
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state": "QUERY_STATE_COMPLETED", "execution_ended_at": "2026-09-17T04:58:12Z",
			"result": map[string]any{"rows": good, "metadata": map[string]any{"total_row_count": curatedRWAMaxRows + 1}},
		})
	}))
	t.Cleanup(huge.Close)
	if _, _, err := newCuratedRWAClient(huge.URL, "k").fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "over the") {
		t.Errorf("oversized result: err = %v", err)
	}
}

// TestCuratedRWAFetch_RefusesToPublishAPartiallyParsedResult pins the
// fail-closed half of the drop.
//
// A row the curator printed that this run cannot read used to be
// counted and skipped: the run exited 0 and published the survivors.
// The cache is replaced series-whole, so that does not merely fail to
// add the unread row — it DELETES the month already cached and serves
// the hole, and when the unread row is the newest month the headline
// is re-dated to an older one and understates. Nothing was red
// anywhere. So a result this run could only read part of is now a
// refusal, before anything is written.
func TestCuratedRWAFetch_RefusesToPublishAPartiallyParsedResult(t *testing.T) {
	goodTotal := []map[string]any{
		{"month_end": "2025-07-31", "total_rwa_market_cap_usd": json.Number("3900000000")},
		{"month_end": "2025-08-31", "total_rwa_market_cap_usd": json.Number("4004795860.00")},
	}
	goodSplit := []map[string]any{
		{"month_end": "2025-08-31", "asset_subclass": "US Treasuries", "market_cap_usd": json.Number("3100000000")},
	}

	for _, tc := range []struct {
		name          string
		total, split  []map[string]any
		wantQuery     int64
		wantGets      int32
		wantSubstring string
	}{
		{
			// The newest month carries no value at all: the survivors
			// would publish August as the curator's latest figure.
			name: "the newest month of the total is unreadable",
			total: append(append([]map[string]any{}, goodTotal...),
				map[string]any{"month_end": "2025-09-30", "total_rwa_market_cap_usd": nil}),
			split:         goodSplit,
			wantQuery:     curatedRWAQueryMonthlyTotal,
			wantGets:      2, // the split is never read, so it is never billed
			wantSubstring: "printed 1 of 3 rows this run could not parse",
		},
		{
			// A hole in the middle is just as fatal: the replace would
			// delete a month already cached and serve the gap.
			name: "a month in the middle of the total is unreadable",
			total: append([]map[string]any{
				{"month_end": "not a month", "total_rwa_market_cap_usd": json.Number("1")},
			}, goodTotal...),
			split:         goodSplit,
			wantQuery:     curatedRWAQueryMonthlyTotal,
			wantGets:      2,
			wantSubstring: "printed 1 of 3 rows this run could not parse",
		},
		{
			// The split is the same series under another name: a
			// subclass row lost is a month whose parts stop summing.
			name:  "a subclass row is unreadable",
			total: goodTotal,
			split: append(append([]map[string]any{}, goodSplit...),
				map[string]any{"month_end": "2025-08-31", "asset_subclass": "", "market_cap_usd": json.Number("1")}),
			wantQuery:     curatedRWAQueryMonthlyBySubclass,
			wantGets:      2, // total (1 page of 2) + split (1 page of 2)
			wantSubstring: "printed 1 of 2 rows this run could not parse",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, gets, _ := stubDuneResults(t, map[int64][]map[string]any{
				curatedRWAQueryMonthlyTotal: tc.total, curatedRWAQueryMonthlyBySubclass: tc.split,
			}, "QUERY_STATE_COMPLETED")

			rows, counts, err := newCuratedRWAClient(srv.URL, "k").fetch(context.Background())
			if err == nil {
				t.Fatalf("fetch returned %d rows and no error; a result read only in part must refuse, not publish the survivors (counts %+v)", len(rows), counts)
			}
			if rows != nil {
				t.Errorf("rows = %d on a refusal, want none to reach the store", len(rows))
			}
			for _, want := range []string{fmt.Sprintf("query %d", tc.wantQuery), tc.wantSubstring, "refusing to publish a partial series"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to name %q", err, want)
				}
			}
			if got := gets.Load(); got != tc.wantGets {
				t.Errorf("GETs = %d, want %d — a refused total must not go on to bill the split", got, tc.wantGets)
			}
		})
	}
}

// TestCuratedRWAParse_CountsEveryUnreadableRow keeps the per-shape
// accounting the refusal above reports: each of these is one row the
// curator printed and this run cannot read, counted once.
func TestCuratedRWAParse_CountsEveryUnreadableRow(t *testing.T) {
	var counts curatedRWACounts
	total := parseCuratedRWAMonthlyTotal(duneQueryResult{Rows: rawRows(t, []map[string]any{
		{"month_end": "2025-08-31", "total_rwa_market_cap_usd": json.Number("4004795860.00")},
		// A renamed column is not guessed at.
		{"month_end": "2025-06-30", "market_cap_usd": json.Number("1")},
		// No month.
		{"month_end": nil, "total_rwa_market_cap_usd": json.Number("5")},
		// A value that is not a figure.
		{"month_end": "2025-05-31", "total_rwa_market_cap_usd": "n/a"},
	})}, &counts)
	if len(total) != 1 || counts.Kept != 1 || counts.Malformed != 3 || counts.Rows != 4 {
		t.Errorf("total parse = %d rows, counts %+v; want 1 kept, 3 unreadable of 4", len(total), counts)
	}

	counts = curatedRWACounts{}
	split := parseCuratedRWAMonthlyBySubclass(duneQueryResult{Rows: rawRows(t, []map[string]any{
		{"month_end": "2025-08-31", "asset_subclass": "US Treasuries", "market_cap_usd": json.Number("3100000000")},
		// An empty subclass would collide with the total series.
		{"month_end": "2025-08-31", "asset_subclass": "  ", "market_cap_usd": json.Number("1")},
		{"month_end": "2025-08-31", "asset_subclass": "Private Credit", "market_cap_usd": "n/a"},
	})}, &counts)
	if len(split) != 1 || counts.Kept != 1 || counts.Malformed != 2 || counts.Rows != 3 {
		t.Errorf("split parse = %d rows, counts %+v; want 1 kept, 2 unreadable of 3", len(split), counts)
	}
}

// rawRows renders fixture rows as the decoder receives them.
func rawRows(t *testing.T, rows []map[string]any) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, 0, len(rows))
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal fixture row: %v", err)
		}
		out = append(out, b)
	}
	return out
}

func TestCuratedRWAMonthEndAndDecimal(t *testing.T) {
	want := time.Date(2025, 8, 31, 0, 0, 0, 0, time.UTC)
	for _, s := range []string{"2025-08-31", "2025-08-31 00:00:00.000 UTC", "2025-08-31 13:45:00", "2025-08-31T13:45:00Z"} {
		if got, ok := curatedRWAMonthEnd(s); !ok || !got.Equal(want) {
			t.Errorf("month %q -> (%v, %v), want (%v, true)", s, got, ok, want)
		}
	}
	for _, s := range []string{"", "yesterday", "2025-13-01"} {
		if _, ok := curatedRWAMonthEnd(s); ok {
			t.Errorf("month %q accepted", s)
		}
	}
	// A plainly-printed literal is stored byte-for-byte as printed:
	// trailing zeros, every digit of the fraction, the sign.
	for _, s := range []string{"4004795860.00", "0", "436023166.55", "-1.5"} {
		if got, ok := curatedRWADecimal(json.Number(s)); !ok || got != s {
			t.Errorf("decimal %q -> (%q, %v), want itself", s, got, ok)
		}
	}
	// An exponent form is a RENDERING of the curator's figure, not a
	// different figure: the point is shifted out exactly, so the row is
	// kept and the stored literal is readable. Refusing it dropped the
	// row — and when it was the newest month, published the series
	// dated to an older one under a green run.
	for _, tc := range []struct{ in, want string }{
		{"4.0e9", "4000000000"},
		{"4.00479586e9", "4004795860"},
		{"1e2", "100"},
		{"1E+2", "100"},
		{"-2.5e3", "-2500"},
		{"1.23e1", "12.3"},
		{"1.5e-3", "0.0015"},
		{"5e-1", "0.5"},
		{"1.500e3", "1500"},
		{"0e0", "0"},
		// Exact to the last digit: a float64 round-trip of this literal
		// loses the tail, and the store carries the curator's digits.
		{"1.234567890123456789e18", "1234567890123456789"},
	} {
		if got, ok := curatedRWADecimal(json.Number(tc.in)); !ok || got != tc.want {
			t.Errorf("decimal %q -> (%q, %v), want (%q, true)", tc.in, got, ok, tc.want)
		}
	}
	// What is still not a published figure. "5/3" and "0x1p-2" are the
	// reason this parses the literal itself rather than deferring to
	// big.Rat, which accepts both; the last two are exponents wide
	// enough to be an allocation rather than a market capitalisation.
	for _, s := range []string{"", "abc", "1,000", "5/3", "0x1p-2", "1e", "1e+", ".", "1.2.3", "1 000", "1e99999", "9e400"} {
		if got, ok := curatedRWADecimal(json.Number(s)); ok {
			t.Errorf("decimal %q accepted as %q", s, got)
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
	c := curatedRWACounts{Kept: 224, Datapoints: 448, ExecutedAt: time.Unix(1789000000, 0)}
	if err := writeCuratedRWATextfile(path, c, true, false); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, _ := os.ReadFile(path)
	s := string(b)
	for _, want := range []string{
		`stellarindex_curated_rwa_sync_rows{curator="dune:stellar"} 224`,
		`stellarindex_curated_rwa_sync_datapoints_read{curator="dune:stellar"} 448`,
		`stellarindex_curated_rwa_sync_executed_at_unix{curator="dune:stellar"} 1789000000`,
		`stellarindex_curated_rwa_sync_written{curator="dune:stellar"} 0`,
		`# TYPE stellarindex_curated_rwa_sync_last_run_unix gauge`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("textfile lacks %q:\n%s", want, s)
		}
	}
	// The old gauge claimed an execution cost; a read has none, and the
	// name must not survive to be graphed as one.
	if strings.Contains(s, "execution_cost_credits") {
		t.Errorf("textfile still exposes execution_cost_credits:\n%s", s)
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
		`stellarindex_curated_rwa_sync_executed_at_unix{curator="dune:stellar"} 0`,
		`stellarindex_curated_rwa_sync_last_run_unix{curator="dune:stellar"} `,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("textfile lacks %q:\n%s", want, got)
		}
	}
}

// curatedRWATestResultsPath is the route every read in this file takes:
// one public query's latest result.
const curatedRWATestResultsPath = "/api/v1/query/6961845/results"

// TestCuratedRWAClient_RefusesARedirectThatWouldCarryTheKey pins the
// paid key to the origin the run dialled.
//
// Go's redirect header copier strips ONLY Authorization,
// WWW-Authenticate and Cookie when a hop crosses hosts, so
// X-Dune-API-Key is otherwise re-sent verbatim to whatever a 302 names
// — a vendor redirect, a hijacked edge, or a mistyped -base-url,
// including an https:// -> http:// downgrade. The https:// check on
// -base-url only ever saw the CONFIGURED URL, never the dialled one.
func TestCuratedRWAClient_RefusesARedirectThatWouldCarryTheKey(t *testing.T) {
	var elsewhereHits atomic.Int32
	var elsewhereSawKey atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereHits.Add(1)
		if r.Header.Get("X-Dune-API-Key") != "" {
			elsewhereSawKey.Store(true)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state": "QUERY_STATE_COMPLETED", "execution_ended_at": "2026-09-17T04:58:12Z",
			"result": map[string]any{"rows": []map[string]any{}, "metadata": map[string]any{"total_row_count": 0}},
		})
	}))
	t.Cleanup(elsewhere.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	_, err := newCuratedRWAClient(origin.URL, "k-secret").get(context.Background(), curatedRWATestResultsPath)
	if err == nil {
		t.Fatal("a cross-host redirect was followed — the run must refuse it, not carry the key over")
	}
	if !strings.Contains(err.Error(), "refusing to follow a redirect") {
		t.Errorf("err = %v, want a refusal naming the redirect", err)
	}
	if elsewhereHits.Load() != 0 || elsewhereSawKey.Load() {
		t.Errorf("the redirect target was dialled %d time(s) and saw the key = %v; X-Dune-API-Key must never leave the origin the run dialled",
			elsewhereHits.Load(), elsewhereSawKey.Load())
	}
}

// TestCuratedRWAClient_FollowsASameOriginRedirect — the policy is "the
// key does not leave this origin", not "no redirects": a hop that keeps
// the scheme and host is still followed, carrying the key as before.
func TestCuratedRWAClient_FollowsASameOriginRedirect(t *testing.T) {
	var gotKey string
	mux := http.NewServeMux()
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Dune-API-Key")
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "QUERY_STATE_COMPLETED"})
	})
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/moved", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	b, err := newCuratedRWAClient(srv.URL, "k-secret").get(context.Background(), curatedRWATestResultsPath)
	if err != nil {
		t.Fatalf("same-origin redirect: %v", err)
	}
	if !strings.Contains(string(b), "QUERY_STATE_COMPLETED") {
		t.Errorf("body = %q, want the moved resource", string(b))
	}
	if gotKey != "k-secret" {
		t.Errorf("key at the same-origin hop = %q, want it carried", gotKey)
	}
}
