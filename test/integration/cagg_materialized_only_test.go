//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// realtimeCAGGs are the only continuous aggregates allowed to serve the
// open bucket (real-time aggregation, migrations 0069 / 0076). They are
// volume counters, not prices. Adding a view here is a decision to let
// every reader of it see a partially-filled bucket.
var realtimeCAGGs = map[string]bool{
	"source_volume_1h":    true,
	"pools_per_source_1h": true,
}

// pinnedCAGGs are the served price / TWAP / oracle / supply / DEX-volume
// views whose unguarded readers (the catalogue snapshot, /v1/markets, the
// DEX pages, FX resolution, RWA daily history) depend on the view exposing
// CLOSED buckets only. Listed so the test fails if one is renamed away.
var pinnedCAGGs = []string{
	"prices_1m", "prices_15m", "prices_1h", "prices_4h",
	"prices_1d", "prices_1w", "prices_1mo",
	"twap_1h", "twap_1d",
	"oracle_prices_1m", "oracle_prices_15m", "oracle_prices_1h", "oracle_prices_4h",
	"oracle_prices_1d", "oracle_prices_1w", "oracle_prices_1mo",
	"supply_1d", "dex_volume_by_pair_1d",
}

// TestCAGGMaterializedOnlyPinned pins ADR-0015's closed-bucket invariant at
// the schema: every served CAGG outside realtimeCAGGs is materialized_only,
// whatever the TimescaleDB default was when it was created and whatever an
// earlier flip left behind. Before 0167 the property held only because the
// current image defaults it on and 0115/0126/0147 carried the prior value
// forward, so a view that was ever real-time stayed real-time.
func TestCAGGMaterializedOnlyPinned(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 164)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Simulate a pre-2.13 TimescaleDB (real-time aggregation on by default)
	// or a 0069-style flip on a price view: every served view is real-time
	// going into the remaining migrations.
	for _, v := range pinnedCAGGs {
		q := fmt.Sprintf(`ALTER MATERIALIZED VIEW %s SET (timescaledb.materialized_only = false)`, v)
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("flip %s to real-time: %v", v, err)
		}
	}

	applyMigrations(t, dsn)

	got := caggMaterializedOnly(t, ctx, db)
	for _, v := range pinnedCAGGs {
		mo, ok := got[v]
		if !ok {
			t.Errorf("continuous aggregate %s is missing after migrating", v)
			continue
		}
		if !mo {
			t.Errorf("%s: materialized_only = false after migrating; its readers would serve the open bucket", v)
		}
	}
	var realtime []string
	for v, mo := range got {
		if !mo && !realtimeCAGGs[v] {
			t.Errorf("%s: materialized_only = false and not in realtimeCAGGs", v)
		}
		if !mo {
			realtime = append(realtime, v)
		}
	}
	sort.Strings(realtime)
	if len(realtime) != len(realtimeCAGGs) {
		t.Errorf("real-time CAGGs = %v, want exactly the realtimeCAGGs allowlist", realtime)
	}
}

func caggMaterializedOnly(t *testing.T, ctx context.Context, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT view_name, materialized_only
		  FROM timescaledb_information.continuous_aggregates
		 WHERE view_schema = 'public'`)
	if err != nil {
		t.Fatalf("list continuous aggregates: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		var mo bool
		if err := rows.Scan(&name, &mo); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[name] = mo
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no continuous aggregates found — the assertion would be vacuous")
	}
	return out
}
