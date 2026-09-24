//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// wantRealtimeCAGGs is the exact set of views the migrations leave
// real-time (0069 / 0076), sorted. Each must also be allowed by
// timescale.RealTimeCAGGAllowed, the list the runtime readiness check uses.
var wantRealtimeCAGGs = []string{"pools_per_source_1h", "source_volume_1h"}

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
// earlier flip left behind. Before 0172 the property held only because the
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
		if !mo && !timescale.RealTimeCAGGAllowed(v) {
			t.Errorf("%s: materialized_only = false and not allowed by timescale.RealTimeCAGGAllowed", v)
		}
		if !mo {
			realtime = append(realtime, v)
		}
	}
	sort.Strings(realtime)
	if !slices.Equal(realtime, wantRealtimeCAGGs) {
		t.Errorf("real-time CAGGs = %v, want exactly %v", realtime, wantRealtimeCAGGs)
	}

	assertClosedBucketReadiness(t, ctx, dsn, db)
}

// assertClosedBucketReadiness drives the runtime chokepoint against the
// migrated schema: clean, it is ready; after an out-of-band flip of a price
// and an oracle view (the 0069/0076 answer to "the current bucket is
// invisible"), Store.OpenBucketCAGGs names exactly those views and the
// critical /readyz checker fails, while the allowlisted real-time volume
// counters stay unreported.
func assertClosedBucketReadiness(t *testing.T, ctx context.Context, dsn string, db *sql.DB) {
	t.Helper()
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("timescale.Open: %v", err)
	}
	defer func() { _ = store.Close() }()
	checker := v1.NewClosedBucketChecker(store)

	open, err := store.OpenBucketCAGGs(ctx)
	if err != nil {
		t.Fatalf("OpenBucketCAGGs on migrated schema: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("OpenBucketCAGGs on migrated schema = %v, want none", open)
	}
	if err := checker.Ping(ctx); err != nil {
		t.Fatalf("closed-bucket readiness on migrated schema: %v", err)
	}

	flipped := []string{"oracle_prices_1d", "prices_1m"}
	for _, v := range flipped {
		q := fmt.Sprintf(`ALTER MATERIALIZED VIEW %s SET (timescaledb.materialized_only = false)`, v)
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("flip %s to real-time: %v", v, err)
		}
	}
	open, err = store.OpenBucketCAGGs(ctx)
	if err != nil {
		t.Fatalf("OpenBucketCAGGs after flip: %v", err)
	}
	if !slices.Equal(open, flipped) {
		t.Errorf("OpenBucketCAGGs after flip = %v, want %v", open, flipped)
	}
	if err := checker.Ping(ctx); err == nil || !strings.Contains(err.Error(), "oracle_prices_1d, prices_1m") {
		t.Errorf("closed-bucket readiness after flip = %v, want a failure naming both views", err)
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
