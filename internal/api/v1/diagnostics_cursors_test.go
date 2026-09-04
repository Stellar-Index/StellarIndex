package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubCursorsReader returns a fixed slice on ListCursors.
type stubCursorsReader struct {
	rows []timescale.Cursor
	err  error
}

func (s *stubCursorsReader) ListCursors(_ context.Context) ([]timescale.Cursor, error) {
	return s.rows, s.err
}

func mkCursor(source, sub string, ledger uint32, age time.Duration) timescale.Cursor {
	return timescale.Cursor{
		Source:     source,
		Sub:        sub,
		LastLedger: ledger,
		UpdatedAt:  time.Now().UTC().Add(-age),
	}
}

// Happy path: no filters → every live + stale row, lag computed, each
// row carrying its derived state.
func TestHandleCursors_AllRows(t *testing.T) {
	srv := v1.New(v1.Options{
		Cursors: &stubCursorsReader{
			rows: []timescale.Cursor{
				mkCursor("ledgerstream", "", 100, 5*time.Second),
				mkCursor("backfill", "0-1000:soroswap", 50, 2*time.Hour),
			},
		},
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/diagnostics/cursors")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.Cursor
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 2 {
		t.Fatalf("got %d rows, want 2 (both inside the abandoned boundary)", len(env.Data))
	}
	states := map[string]string{}
	for _, c := range env.Data {
		states[c.Source] = c.State
	}
	if states["ledgerstream"] != "live" {
		t.Errorf("ledgerstream state = %q, want live", states["ledgerstream"])
	}
	if states["backfill"] != "stale" {
		t.Errorf("backfill state = %q, want stale (2h old, inside the 7d boundary)", states["backfill"])
	}
}

// The defect this endpoint carried: `ingestion_cursors` keeps one
// permanent row per one-shot job shard and has no retention, so the
// unfiltered response served every abandoned shard forever — on r1,
// 4,703 of 4,815 rows, the oldest an SDEX backfill shard last touched
// 112 days earlier. The default response is now the set something is
// plausibly still writing; the dead set is opt-in.
func TestHandleCursors_AbandonedExcludedByDefault(t *testing.T) {
	srv := v1.New(v1.Options{
		Cursors: &stubCursorsReader{
			rows: []timescale.Cursor{
				mkCursor("ledgerstream", "", 100, 5*time.Second),
				mkCursor("backfill", "2-15300000:sdex", 98333, 112*24*time.Hour),
				mkCursor("backfill", "30600000-45899998:sdex", 30635813, 112*24*time.Hour),
			},
		},
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/diagnostics/cursors")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.Cursor `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Fatalf("got %d rows, want 1 — the two 112-day-old sdex shards must not be in the default response", len(env.Data))
	}
	if env.Data[0].Source != "ledgerstream" || env.Data[0].State != "live" {
		t.Errorf("row = %+v, want the live ledgerstream cursor", env.Data[0])
	}

	// ?include_abandoned=true is the explicit opt-in, and marks the
	// dead shards as such so they can't be mistaken for live ones.
	resp = mustGet(t, ts.URL+"/v1/diagnostics/cursors?include_abandoned=true")
	env.Data = nil
	mustDecode(t, resp, &env)
	if len(env.Data) != 3 {
		t.Fatalf("include_abandoned=true: got %d rows, want 3", len(env.Data))
	}
	abandoned := 0
	for _, c := range env.Data {
		if c.State == "abandoned" {
			abandoned++
		}
	}
	if abandoned != 2 {
		t.Errorf("got %d abandoned-state rows, want 2 (the sdex shards)", abandoned)
	}

	// ?status=abandoned is the reap-planning view: the dead set alone,
	// no second parameter needed.
	resp = mustGet(t, ts.URL+"/v1/diagnostics/cursors?status=abandoned")
	env.Data = nil
	mustDecode(t, resp, &env)
	if len(env.Data) != 2 {
		t.Fatalf("status=abandoned: got %d rows, want 2", len(env.Data))
	}
	for _, c := range env.Data {
		if c.State != "abandoned" {
			t.Errorf("status=abandoned returned a %q row (%+v)", c.State, c)
		}
	}
}

// The response is bounded. Without a cap the row count is whatever
// history left behind — every sharded ops job mints permanent rows —
// so a single GET could grow without limit again. Pages are stable
// (ListCursors' (source, sub_source) order) and reachable through
// `pagination.next`.
func TestHandleCursors_LimitBoundsResponse(t *testing.T) {
	rows := make([]timescale.Cursor, 0, 1200)
	for i := range 1200 {
		rows = append(rows, mkCursor("projected-rebuild", "shard-"+strconv.Itoa(i), uint32(i), time.Hour))
	}
	srv := v1.New(v1.Options{Cursors: &stubCursorsReader{rows: rows}})
	ts := httpTestServer(t, srv)

	// Default cap: 500 rows + a next token, not all 1,200.
	resp := mustGet(t, ts.URL+"/v1/diagnostics/cursors")
	var env struct {
		Data       []v1.Cursor    `json:"data"`
		Pagination *v1.Pagination `json:"pagination"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 500 {
		t.Fatalf("got %d rows, want the 500-row default cap", len(env.Data))
	}
	if env.Pagination == nil || env.Pagination.Next != "500" {
		t.Fatalf("pagination = %+v, want next=500", env.Pagination)
	}

	// The token pages forward, and the final page carries no token.
	resp = mustGet(t, ts.URL+"/v1/diagnostics/cursors?cursor=1000")
	env.Data, env.Pagination = nil, nil
	mustDecode(t, resp, &env)
	if len(env.Data) != 200 {
		t.Fatalf("cursor=1000: got %d rows, want the remaining 200", len(env.Data))
	}
	if env.Pagination != nil {
		t.Errorf("last page carried pagination = %+v, want none", env.Pagination)
	}

	// An explicit limit is honoured up to the ceiling; past it, 400.
	resp = mustGet(t, ts.URL+"/v1/diagnostics/cursors?limit=10")
	env.Data = nil
	mustDecode(t, resp, &env)
	if len(env.Data) != 10 {
		t.Errorf("limit=10: got %d rows, want 10", len(env.Data))
	}
	resp = mustGet(t, ts.URL+"/v1/diagnostics/cursors?limit=5000")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("limit=5000: status = %d, want 400 (above the 2000 ceiling)", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// A malformed pagination token is refused rather than silently
	// serving page 1 under a token the client thinks means something.
	resp = mustGet(t, ts.URL+"/v1/diagnostics/cursors?cursor=nonsense")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("cursor=nonsense: status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// The one condition this endpoint exists to surface is stuck ingest,
// and the abandoned rule must not swallow it. `ledgerstream` and
// `projector` hold RESUME state something still reads, so an old row
// there is an incident, not a leftover — the same premise that makes
// `reap-cursors` refuse to delete them, read off the same list. An age
// filter that hides a dead live cursor is a worse outcome than the
// 4,815-row response this whole change replaced.
func TestHandleCursors_StuckLiveCursorSurvivesTheAbandonedRule(t *testing.T) {
	srv := v1.New(v1.Options{
		Cursors: &stubCursorsReader{
			rows: []timescale.Cursor{
				mkCursor("ledgerstream", "", 63302110, 8*24*time.Hour),
				mkCursor("projector", "soroswap", 63302110, 30*24*time.Hour),
				mkCursor("backfill", "2-15300000:sdex", 98333, 112*24*time.Hour),
			},
		},
	})
	ts := httpTestServer(t, srv)

	// Default response: both stuck live cursors, and only they.
	resp := mustGet(t, ts.URL+"/v1/diagnostics/cursors")
	var env struct {
		Data []v1.Cursor `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 2 {
		t.Fatalf("got %d rows, want the 2 stuck live cursors — a dead ledgerstream/projector row must never age out of the default response; got %+v",
			len(env.Data), env.Data)
	}
	for _, c := range env.Data {
		if c.State != "stale" {
			t.Errorf("%s state = %q, want stale (a live namespace is never abandoned)", c.Source, c.State)
		}
		if c.LagSeconds < int64((8 * 24 * time.Hour).Seconds()) {
			t.Errorf("%s lag_seconds = %d, want the full stuck lag", c.Source, c.LagSeconds)
		}
	}

	// ?status=stale is documented as the view for spotting dead ingest
	// paths, so it must carry them too.
	resp = mustGet(t, ts.URL+"/v1/diagnostics/cursors?status=stale")
	env.Data = nil
	mustDecode(t, resp, &env)
	if len(env.Data) != 2 {
		t.Fatalf("status=stale: got %d rows, want the 2 stuck live cursors", len(env.Data))
	}

	// The reap-planning view is the complement: it must NOT offer up a
	// row `reap-cursors` would refuse to delete.
	resp = mustGet(t, ts.URL+"/v1/diagnostics/cursors?status=abandoned")
	env.Data = nil
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 || env.Data[0].Source != "backfill" {
		t.Fatalf("status=abandoned = %+v, want only the sdex backfill shard", env.Data)
	}
}

// max_age=1h excludes the 3-hour-old backfill row. The fixture is
// deliberately inside the abandoned boundary: a row the default rule
// would drop anyway proves nothing about max_age.
func TestHandleCursors_MaxAgeExcludesStale(t *testing.T) {
	srv := v1.New(v1.Options{
		Cursors: &stubCursorsReader{
			rows: []timescale.Cursor{
				mkCursor("ledgerstream", "", 100, 5*time.Second),
				mkCursor("backfill", "0-1000:soroswap", 50, 3*time.Hour),
			},
		},
	})
	ts := httpTestServer(t, srv)

	// Without max_age both rows are served — so the single row below is
	// max_age's doing, not the abandoned default's.
	resp := mustGet(t, ts.URL+"/v1/diagnostics/cursors")
	var env struct {
		Data []v1.Cursor
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 2 {
		t.Fatalf("unfiltered: got %d rows, want 2 — the fixture must be inside the abandoned boundary for this test to isolate max_age", len(env.Data))
	}

	resp = mustGet(t, ts.URL+"/v1/diagnostics/cursors?max_age=1h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	env.Data = nil
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 1 {
		t.Fatalf("got %d rows, want 1 (only the live ledgerstream row)", len(env.Data))
	}
	if env.Data[0].Source != "ledgerstream" {
		t.Errorf("source = %q, want ledgerstream", env.Data[0].Source)
	}
}

// max_age accepts every Go-duration unit operators reach for.
func TestHandleCursors_MaxAgeUnits(t *testing.T) {
	srv := v1.New(v1.Options{
		Cursors: &stubCursorsReader{
			rows: []timescale.Cursor{
				mkCursor("ledgerstream", "", 100, 30*time.Second),
			},
		},
	})
	ts := httpTestServer(t, srv)

	for _, max := range []string{"1m", "60s", "0.001h"} {
		resp := mustGet(t, ts.URL+"/v1/diagnostics/cursors?max_age="+max)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("max_age=%q status = %d, want 200", max, resp.StatusCode)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// Invalid max_age → 400 with the documented type URL.
func TestHandleCursors_InvalidMaxAge(t *testing.T) {
	srv := v1.New(v1.Options{
		Cursors: &stubCursorsReader{rows: nil},
	})
	ts := httpTestServer(t, srv)

	for _, bad := range []string{"garbage", "0", "-5m"} {
		resp := mustGet(t, ts.URL+"/v1/diagnostics/cursors?max_age="+bad)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("max_age=%q: status = %d, want 400", bad, resp.StatusCode)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(body), "/invalid-max-age") {
			t.Errorf("max_age=%q: body should reference invalid-max-age, got %q", bad, string(body))
		}
	}
}

// ?source=<name> isolates one source — the live indexer cursor
// without the ~50 backfill rows alongside it. Caught from a r1
// audit: the param was being silently ignored, so an operator
// asking for ?source=ledgerstream got everything.
func TestHandleCursors_SourceFilter(t *testing.T) {
	srv := v1.New(v1.Options{
		Cursors: &stubCursorsReader{
			rows: []timescale.Cursor{
				mkCursor("ledgerstream", "", 100, 5*time.Second),
				mkCursor("backfill", "0-1000:soroswap", 50, 1*time.Hour),
				mkCursor("backfill", "1000-2000:soroswap", 75, 30*time.Minute),
			},
		},
	})
	ts := httpTestServer(t, srv)

	// source=ledgerstream → only the live cursor.
	resp := mustGet(t, ts.URL+"/v1/diagnostics/cursors?source=ledgerstream")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.Cursor
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 1 {
		t.Fatalf("source=ledgerstream: got %d rows, want 1", len(env.Data))
	}
	if env.Data[0].Source != "ledgerstream" {
		t.Errorf("Source = %q, want ledgerstream", env.Data[0].Source)
	}

	// source=backfill → only backfill rows.
	resp = mustGet(t, ts.URL+"/v1/diagnostics/cursors?source=backfill")
	env.Data = nil
	json.NewDecoder(resp.Body).Decode(&env)
	if len(env.Data) != 2 {
		t.Errorf("source=backfill: got %d rows, want 2", len(env.Data))
	}
	for _, c := range env.Data {
		if c.Source != "backfill" {
			t.Errorf("Source = %q, want backfill", c.Source)
		}
	}

	// source=unknown → empty array (not 400). Keeps the surface
	// predictable when an operator typos vs. a brand-new source we
	// haven't seen yet.
	resp = mustGet(t, ts.URL+"/v1/diagnostics/cursors?source=does-not-exist")
	env.Data = nil
	json.NewDecoder(resp.Body).Decode(&env)
	if len(env.Data) != 0 {
		t.Errorf("source=does-not-exist: got %d rows, want 0 (empty array)", len(env.Data))
	}
}

// source + max_age compose: only ledgerstream rows AND only fresh.
func TestHandleCursors_SourceAndMaxAgeCompose(t *testing.T) {
	srv := v1.New(v1.Options{
		Cursors: &stubCursorsReader{
			rows: []timescale.Cursor{
				mkCursor("ledgerstream", "", 100, 5*time.Second),  // fresh, kept
				mkCursor("backfill", "stale", 50, 7*24*time.Hour), // wrong source, dropped
				mkCursor("ledgerstream", "old", 75, 24*time.Hour), // right source but stale, dropped
			},
		},
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/diagnostics/cursors?source=ledgerstream&max_age=1h")
	var env struct{ Data []v1.Cursor }
	json.NewDecoder(resp.Body).Decode(&env)
	if len(env.Data) != 1 {
		t.Fatalf("got %d rows, want 1 (fresh ledgerstream only)", len(env.Data))
	}
	if env.Data[0].LastLedger != 100 {
		t.Errorf("LastLedger = %d, want 100 (the fresh row)", env.Data[0].LastLedger)
	}
}

// 503 when no reader is wired — preserves the legacy contract.
func TestHandleCursors_NoReaderReturns503(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/diagnostics/cursors")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (no Cursors reader)", resp.StatusCode)
	}
}

// TestHandleCursors_StatusActive — `?status=active` filters out
// rows older than 10 minutes (the active/stale boundary). Matches
// the R-015 ask: completed backfill cursors that linger in the
// table shouldn't dominate the listing.
func TestHandleCursors_StatusActive(t *testing.T) {
	srv := v1.New(v1.Options{
		Cursors: &stubCursorsReader{
			rows: []timescale.Cursor{
				mkCursor("ledgerstream", "", 100, 5*time.Second),    // active
				mkCursor("backfill", "stale-1", 50, 12*time.Minute), // stale
				mkCursor("backfill", "stale-2", 51, 7*24*time.Hour), // stale
				mkCursor("backfill", "fresh", 99, 1*time.Minute),    // active
			},
		},
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/diagnostics/cursors?status=active")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []v1.Cursor `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 2 {
		t.Fatalf("got %d rows, want 2 (active only)", len(env.Data))
	}
	for _, c := range env.Data {
		if c.LagSeconds > 600 {
			t.Errorf("lag_seconds = %d > 600 leaked through active filter (sub=%q)",
				c.LagSeconds, c.SubSource)
		}
	}
}

// TestHandleCursors_StatusStale — complement; rows older than the
// 10-min boundary but not yet abandoned, i.e. the set still worth
// resuming. The abandoned row stays out until asked for.
func TestHandleCursors_StatusStale(t *testing.T) {
	srv := v1.New(v1.Options{
		Cursors: &stubCursorsReader{
			rows: []timescale.Cursor{
				mkCursor("ledgerstream", "", 100, 5*time.Second),
				mkCursor("backfill", "stale-1", 50, 12*time.Minute),
				mkCursor("backfill", "stale-2", 51, 3*24*time.Hour),
				mkCursor("backfill", "abandoned", 52, 30*24*time.Hour),
			},
		},
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/diagnostics/cursors?status=stale")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.Cursor `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 2 {
		t.Fatalf("got %d rows, want 2 stale (the 30-day row is abandoned, not stale)", len(env.Data))
	}
	for _, c := range env.Data {
		if c.LagSeconds <= 600 {
			t.Errorf("lag_seconds = %d <= 600 leaked through stale filter", c.LagSeconds)
		}
		if c.State != "stale" {
			t.Errorf("state = %q, want stale (sub=%q)", c.State, c.SubSource)
		}
	}

	// Composed with the opt-in, stale means "everything past 10 min".
	resp = mustGet(t, ts.URL+"/v1/diagnostics/cursors?status=stale&include_abandoned=true")
	env.Data = nil
	mustDecode(t, resp, &env)
	if len(env.Data) != 3 {
		t.Errorf("status=stale&include_abandoned=true: got %d rows, want 3", len(env.Data))
	}
}

// TestHandleCursors_StatusInvalid — bad value → 400.
func TestHandleCursors_StatusInvalid(t *testing.T) {
	srv := v1.New(v1.Options{
		Cursors: &stubCursorsReader{rows: nil},
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/diagnostics/cursors?status=bogus")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 on invalid status", resp.StatusCode)
	}
}

// TestHandleCursors_StatusActiveCombinesWithMaxAge — passing both
// tightens the window to whichever bound is tighter. status=active
// caps at 10m; an explicit max_age=5m wins.
func TestHandleCursors_StatusActiveCombinesWithMaxAge(t *testing.T) {
	srv := v1.New(v1.Options{
		Cursors: &stubCursorsReader{
			rows: []timescale.Cursor{
				mkCursor("ledgerstream", "fresh", 100, 1*time.Second),
				mkCursor("ledgerstream", "between", 99, 7*time.Minute),
				mkCursor("ledgerstream", "stale", 98, 30*time.Minute),
			},
		},
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/diagnostics/cursors?status=active&max_age=5m")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.Cursor `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Fatalf("got %d rows, want 1 (only 'fresh' inside the 5m window)", len(env.Data))
	}
	if env.Data[0].SubSource != "fresh" {
		t.Errorf("sub_source = %q, want fresh", env.Data[0].SubSource)
	}
}

// F-0094 closure (2026-05-28): under the cascade, /v1/diagnostics/cursors
// returned a generic 500 that didn't distinguish "postgres briefly
// stalled" from "endpoint permanently broken." Operators couldn't tell
// whether to retry or escalate. Map transient + timeout shapes to
// 503 with a clearer detail string.
func TestHandleCursors_TransientStorageError_Returns503(t *testing.T) {
	srv := v1.New(v1.Options{
		Cursors: &stubCursorsReader{
			err: errors.New("driver: bad connection"),
		},
	})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/diagnostics/cursors")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (transient storage err)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "cursors-transient") {
		t.Errorf("body should mention cursors-transient; got %s", string(body))
	}
}

func TestHandleCursors_NonTransientError_Returns500(t *testing.T) {
	srv := v1.New(v1.Options{
		Cursors: &stubCursorsReader{
			err: errors.New("malformed SQL"),
		},
	})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/diagnostics/cursors")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (non-transient err)", resp.StatusCode)
	}
}
