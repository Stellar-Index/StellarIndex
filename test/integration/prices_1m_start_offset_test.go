//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestPrices1mStartOffsetWidened pins RLT-154 / GH #623 (b): prices_1m's
// refresh policy must look back far enough to re-cover trades drained
// after a ch-live-catchup stall (worst case ~10 minutes,
// deploy/systemd/ch-live-catchup.timer). Migration 0002 shipped it at 5
// minutes, sized only for Postgres backfills; migration 0165 widens it
// to 15. A regression back to 5 (or anything <= the 10-minute catch-up
// period) would silently under-report the earliest drained buckets on
// /v1/ohlc, /v1/history and /v1/chart at the persist_per_source=false
// flip, with /v1/coverage still reporting the range complete.
func TestPrices1mStartOffsetWidened(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	var startOffset time.Duration
	err = db.QueryRowContext(ctx, `
		SELECT (config->>'start_offset')::interval
		  FROM timescaledb_information.jobs
		 WHERE proc_name = 'policy_refresh_continuous_aggregate'
		   AND hypertable_name = 'prices_1m'`).Scan(&startOffset)
	if err != nil {
		t.Fatalf("read prices_1m refresh policy start_offset: %v", err)
	}

	const catchUpPeriod = 10 * time.Minute
	if startOffset <= catchUpPeriod {
		t.Fatalf("prices_1m start_offset = %s, want > %s (the ch-live-catchup period) — "+
			"a stall that drains right before the timer fires would land trades outside "+
			"the refresh lookback and they would never be re-aggregated", startOffset, catchUpPeriod)
	}
	const want = 15 * time.Minute
	if startOffset != want {
		t.Fatalf("prices_1m start_offset = %s, want %s (migration 0165)", startOffset, want)
	}
}
