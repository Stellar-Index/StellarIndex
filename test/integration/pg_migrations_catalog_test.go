//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func openMigrated(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db
}

// A recreate sizes materialization chunks at 10x the source interval; every trades-derived view must stay at 7 days.
func TestCAGGMaterializationChunkInterval(t *testing.T) {
	ctx, db := openMigrated(t)
	const q = `
		SELECT d.time_interval::text
		  FROM timescaledb_information.continuous_aggregates ca
		  JOIN timescaledb_information.dimensions d
		    ON d.hypertable_schema = ca.materialization_hypertable_schema
		   AND d.hypertable_name = ca.materialization_hypertable_name
		 WHERE ca.view_schema = 'public' AND ca.view_name = $1 AND d.dimension_type = 'Time'`
	for _, v := range []string{
		"prices_1m", "prices_15m", "prices_1h", "prices_4h", "prices_1d", "prices_1w", "prices_1mo",
		"twap_1h", "twap_1d", "pools_per_source_1h", "trade_sums_1m",
	} {
		var w string
		if err := db.QueryRowContext(ctx, q, v).Scan(&w); err != nil {
			t.Fatalf("read %s materialization interval: %v", v, err)
		}
		if w != "7 days" {
			t.Errorf("%s materialization chunk interval = %q, want \"7 days\"", v, w)
		}
	}
}

// The stored comment must tell a \d+ reader that rows drive the price correction, not a decline.
func TestNonstandardDecimalsAssetsComment(t *testing.T) {
	ctx, db := openMigrated(t)
	var c string
	if err := db.QueryRowContext(ctx,
		`SELECT obj_description('nonstandard_decimals_assets'::regclass, 'pg_class')`).Scan(&c); err != nil {
		t.Fatalf("read comment: %v", err)
	}
	if !strings.Contains(c, "AdjustPrice") || strings.Contains(c, "declines") {
		t.Fatalf("nonstandard_decimals_assets comment still describes a decline guard: %q", c)
	}
}
