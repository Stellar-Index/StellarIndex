package timescale

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"
)

// TestListDivergenceLatest_DaysFilterUsesMakeInterval: the window must be make_interval(days => $1), because
// `($1 || ' days')::interval` types $1 as text and pgx cannot encode a Go int to it. The canned driver
// does not encode args, so this only guards the SQL text against that form.
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
