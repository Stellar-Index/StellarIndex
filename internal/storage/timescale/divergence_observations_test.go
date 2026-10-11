package timescale

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"
)

// TestListDivergenceLatest_DaysFilterUsesMakeInterval guards a pgx encode trap. A window
// written as
//
//	observed_at > now() - ($1 || ' days')::interval
//
// makes Postgres infer $1 as TEXT, but sinceDays is a Go int and pgx v5 has no int->text
// encode plan ("cannot find encode plan"), so every request failed before executing. The
// window must be make_interval(days => $1), which types $1 as an integer, and the emitted SQL
// must NOT reconstruct the text-concatenation form.
//
// Caveat: the canned driver captures query text but does not encode args through pgx, so it
// cannot reproduce the failure itself; the real encode-path proof is the integration/live
// endpoint. This is the cheap structural guard against reintroducing `$N || ' days'`.
func TestListDivergenceLatest_DaysFilterUsesMakeInterval(t *testing.T) {
	// 9-column empty result matching the SELECT; 0 rows so Scan never runs.
	script := []scriptedResult{{
		cols: []string{
			"asset_id", "quote_id", "reference", "observed_at",
			"observed_at_ledger", "our_price", "ref_price", "delta_pct", "status",
		},
		rows: [][]driver.Value{},
	}}
	s, conn := newScriptedStore(t, script...)

	if _, err := s.ListDivergenceLatest(context.Background(), 7, false, 100); err != nil {
		t.Fatalf("ListDivergenceLatest: %v", err)
	}
	if len(conn.statements()) != 1 {
		t.Fatalf("expected exactly 1 query, got %d", len(conn.statements()))
	}
	q := conn.statements()[0]
	if !strings.Contains(q, "make_interval(days => $1)") {
		t.Errorf("days window must use make_interval(days => $1) so $1 types as int; got:\n%s", q)
	}
	if strings.Contains(q, "|| ' days'") {
		t.Errorf("days window must NOT use the ($1 || ' days') text-concat form — "+
			"it types $1 as text and pgx cannot encode the int arg; got:\n%s", q)
	}
}
