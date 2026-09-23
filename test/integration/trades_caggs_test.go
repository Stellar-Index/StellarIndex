//go:build integration

package integration_test

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestTradesCAGGsMatchCatalog holds timescale.TradesCAGGs in lockstep
// with the migrated schema: every continuous aggregate whose ROOT
// hypertable is `trades` (hierarchical ones resolved through their
// parent) must be in the list, and nothing else may be. A migration
// that adds a trades-rooted aggregate without listing it leaves a
// served surface the restamp follow-up and the refresh allow-list
// cannot reach; a stale name in the list would fail at refresh time.
func TestTradesCAGGsMatchCatalog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Walk each aggregate up its parent chain to the raw hypertable it
	// is ultimately built on (twap_* → prices_1m → trades).
	const q = `
		WITH RECURSIVE chain AS (
		    SELECT ca.user_view_name, ca.raw_hypertable_id, ca.parent_mat_hypertable_id
		      FROM _timescaledb_catalog.continuous_agg ca
		    UNION ALL
		    SELECT c.user_view_name, p.raw_hypertable_id, p.parent_mat_hypertable_id
		      FROM chain c
		      JOIN _timescaledb_catalog.continuous_agg p ON p.mat_hypertable_id = c.parent_mat_hypertable_id
		)
		SELECT DISTINCT c.user_view_name
		  FROM chain c
		  JOIN _timescaledb_catalog.hypertable h ON h.id = c.raw_hypertable_id
		 WHERE c.parent_mat_hypertable_id IS NULL
		   AND h.table_name = 'trades'
		 ORDER BY 1
	`
	rows, err := store.DB().QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("catalog query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var inCatalog []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		inCatalog = append(inCatalog, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(inCatalog) == 0 {
		t.Fatal("catalog walk found no trades-rooted continuous aggregate — the query, not the list, is broken")
	}

	var listed []string
	for _, c := range timescale.TradesCAGGs {
		listed = append(listed, c.Name)
	}
	sort.Strings(listed)

	catalogSet := map[string]bool{}
	for _, n := range inCatalog {
		catalogSet[n] = true
	}
	listedSet := map[string]bool{}
	for _, n := range listed {
		if listedSet[n] {
			t.Errorf("TradesCAGGs lists %s twice", n)
		}
		listedSet[n] = true
		if !catalogSet[n] {
			t.Errorf("TradesCAGGs lists %s, which is not a trades-rooted continuous aggregate in the migrated schema", n)
		}
	}
	for _, n := range inCatalog {
		if !listedSet[n] {
			t.Errorf("continuous aggregate %s is rooted on trades but missing from TradesCAGGs", n)
		}
	}
	t.Logf("trades-rooted aggregates in catalog: %d, listed: %d", len(inCatalog), len(listed))

	// CAGGsOnPrices1m decides which views the trades refresh forces after
	// a forced prices_1m; a hierarchical view missing from it would be
	// refreshed over minute rows a retention drop removed.
	onMinute := catalogNames(t, ctx, store, `
		SELECT c.user_view_name
		  FROM _timescaledb_catalog.continuous_agg c
		  JOIN _timescaledb_catalog.continuous_agg p ON p.mat_hypertable_id = c.parent_mat_hypertable_id
		 WHERE p.user_view_name = 'prices_1m'
		 ORDER BY 1`)
	want := append([]string(nil), timescale.CAGGsOnPrices1m...)
	sort.Strings(want)
	if strings.Join(onMinute, ",") != strings.Join(want, ",") {
		t.Errorf("views built on prices_1m in catalog = %v, timescale.CAGGsOnPrices1m = %v", onMinute, want)
	}
}

func catalogNames(t *testing.T, ctx context.Context, store *timescale.Store, q string) []string {
	t.Helper()
	rows, err := store.DB().QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("catalog query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}
