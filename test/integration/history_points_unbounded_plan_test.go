//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestHistoryPointsUnboundedPopulatedReadStreams measures the one shape
// the unknown-pair cost test does not: /v1/history/since-inception with
// limit=0 over a pair that HAS a long history. That statement carries no
// LIMIT and an outer `ORDER BY bucket ASC, base_asset` over a UNION ALL
// whose branches are index-ordered on bucket alone, so the question is
// whether the outer sort streams or materialises the pair's whole series.
//
// Under the serving pool's plan_cache_mode = force_custom_plan (see
// OpenServing) it streams: a Merge Append on bucket feeding an
// Incremental Sort whose groups are one bucket (at most two rows). No
// full Sort node may appear. Measured on this fixture (3,120 hourly
// buckets over ~14 chunks): 3.2 ms, 29 kB peak sort memory. Adding
// base_asset to each branch's ORDER BY changes nothing — within a branch
// base_asset is bound to a parameter, so the planner drops it as a
// redundant sort key. A GENERIC plan (not used by the serving pool) is
// logged for the record: it chose Parallel Append + a full Sort, 17 ms.
func TestHistoryPointsUnboundedPopulatedReadStreams(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()
	// Prepared statements and SET are per-session: one backend throughout.
	db.SetMaxOpenConns(1)
	seedCostFixture(t, ctx, db)
	pairs := newCostPairs(t)

	pts, err := store.HistoryPoints(ctx, pairs.traded, timescale.Granularity1h, 0)
	if err != nil {
		t.Fatalf("HistoryPoints(limit=0): %v", err)
	}
	if want := costFixtureDays * 24; len(pts) != want {
		t.Fatalf("HistoryPoints(limit=0) returned %d buckets, want %d — not the populated since-inception read", len(pts), want)
	}
	stmt := capturePreparedStatement(t, ctx, db, nil, "FROM prices_1h", "ORDER BY bucket ASC")
	args := []string{pairs.traded.Base.String(), pairs.traded.Quote.String()}

	// Instrument check first: with incremental sort disabled the same
	// statement has no way to stream, so the walker must see a full Sort.
	mustExecPlan(t, ctx, db, `SET enable_incremental_sort = off`)
	control := planNodeTypes(t, ctx, db, "force_custom_plan", stmt, args)
	mustExecPlan(t, ctx, db, `RESET enable_incremental_sort`)
	if !slices.Contains(control, "Sort") {
		t.Fatalf("instrument check failed: with incremental sort off the plan %v has no Sort node — "+
			"the walker is not reading the plan", control)
	}

	served := planNodeTypes(t, ctx, db, "force_custom_plan", stmt, args)
	t.Logf("force_custom_plan: %v", served)
	if slices.Contains(served, "Sort") {
		t.Errorf("serving-mode plan materialises the whole series in a full Sort: %v", served)
	}
	if !slices.Contains(served, "Merge Append") {
		t.Errorf("serving-mode plan does not merge the two index-ordered branches: %v", served)
	}
	t.Logf("force_generic_plan (not the serving pool's mode): %v",
		planNodeTypes(t, ctx, db, "force_generic_plan", stmt, args))
}

func mustExecPlan(t *testing.T, ctx context.Context, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// planNodeTypes prepares stmt under planMode and returns every node type
// of its EXPLAIN ANALYZE plan, depth-first. args are string literals.
func planNodeTypes(t *testing.T, ctx context.Context, db *sql.DB, planMode, stmt string, args []string) []string {
	t.Helper()
	const name = "unbounded_plan_probe"
	mustExecPlan(t, ctx, db, `SET plan_cache_mode = `+planMode)
	mustExecPlan(t, ctx, db, `PREPARE `+name+` AS `+stmt)
	defer mustExecPlan(t, ctx, db, `DEALLOCATE `+name)
	lits := ""
	for i, a := range args {
		if i > 0 {
			lits += ", "
		}
		lits += "'" + a + "'"
	}
	var raw string
	if err := db.QueryRowContext(ctx,
		`EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE `+name+`(`+lits+`)`).Scan(&raw); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	var doc []struct {
		Plan explainNode `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || len(doc) != 1 {
		t.Fatalf("parse EXPLAIN json (%v): %s", err, raw)
	}
	var out []string
	var walk func(n explainNode)
	walk = func(n explainNode) {
		out = append(out, n.NodeType)
		for _, ch := range n.Plans {
			walk(ch)
		}
	}
	walk(doc[0].Plan)
	return out
}
