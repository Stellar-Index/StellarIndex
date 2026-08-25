package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"
)

// TestListDivergenceLatest_DaysFilterUsesMakeInterval guards the pgx
// encode trap that took the /v1/divergence board 100% down in
// production: every request 500'd with
//
//	timescale: ListDivergenceLatest: failed to encode args[0]:
//	unable to encode 7 into text format for text (OID 25):
//	cannot find encode plan
//
// The bug was the trailing-window filter written as
//
//	observed_at > now() - ($1 || ' days')::interval
//
// The `$1 || ' days'` concatenation makes Postgres infer $1 as TEXT,
// but ListDivergenceLatest passes sinceDays as a Go int. pgx v5 has no
// int->text encode plan, so the query failed before it ever executed.
//
// The fix expresses the window as make_interval(days => $1), which
// types $1 as an integer. This test locks that in: the emitted SQL
// must use make_interval and must NOT reconstruct the fragile
// text-concatenation form.
//
// Caveat: the canned driver captures query text but does not encode
// args through pgx, so it cannot itself reproduce the original
// failure — the real encode-path proof is the integration/live
// endpoint. This is the cheap structural guard against anyone
// reintroducing the `$N || ' days'` pattern.
func TestListDivergenceLatest_DaysFilterUsesMakeInterval(t *testing.T) {
	// 9-column empty result matching the SELECT; 0 rows so Scan never runs.
	conn := &cannedConn{plan: []cannedResult{{
		cols: []string{
			"asset_id", "quote_id", "reference", "observed_at",
			"observed_at_ledger", "our_price", "ref_price", "delta_pct", "status",
		},
		rows: [][]driver.Value{},
	}}}
	s := &Store{db: sql.OpenDB(&cannedConnector{conn: conn})}

	if _, err := s.ListDivergenceLatest(context.Background(), 7, false, 100); err != nil {
		t.Fatalf("ListDivergenceLatest: %v", err)
	}
	if len(conn.queries) != 1 {
		t.Fatalf("expected exactly 1 query, got %d", len(conn.queries))
	}
	q := conn.queries[0]
	if !strings.Contains(q, "make_interval(days => $1)") {
		t.Errorf("days window must use make_interval(days => $1) so $1 types as int; got:\n%s", q)
	}
	if strings.Contains(q, "|| ' days'") {
		t.Errorf("days window must NOT use the ($1 || ' days') text-concat form — "+
			"it types $1 as text and pgx cannot encode the int arg; got:\n%s", q)
	}
}
