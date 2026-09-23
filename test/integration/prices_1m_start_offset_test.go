//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestPrices1mStartOffsetWidened executes migration 0165 up and down.
// prices_1m's refresh lookback must exceed the ch-live-catchup period
// (deploy/systemd/ch-live-catchup.timer, 10 minutes): trades drained after a
// lake-hole stall carry ledger-close timestamps up to that old, and a
// shorter lookback never re-aggregates their buckets.
func TestPrices1mStartOffsetWidened(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	const (
		before = 5 * time.Minute  // as 0147 left it
		want   = 15 * time.Minute // 0165
	)

	applyMigrationsUpTo(t, dsn, 164)
	if got := prices1mStartOffset(t, ctx, db); got != before {
		t.Fatalf("pre-0165 prices_1m start_offset = %s, want %s", got, before)
	}

	applyMigrationsUpTo(t, dsn, 165)
	got := prices1mStartOffset(t, ctx, db)
	if got != want {
		t.Fatalf("prices_1m start_offset after 0165 = %s, want %s", got, want)
	}
	if catchUpPeriod := 10 * time.Minute; got <= catchUpPeriod {
		t.Fatalf("prices_1m start_offset = %s, must exceed the %s ch-live-catchup period", got, catchUpPeriod)
	}

	applyMigrationsUpTo(t, dsn, 164)
	if got := prices1mStartOffset(t, ctx, db); got != before {
		t.Fatalf("prices_1m start_offset after 0165 down = %s, want %s", got, before)
	}
}

// prices1mStartOffset reads prices_1m's refresh-policy start_offset,
// requiring exactly one refresh policy on the view.
func prices1mStartOffset(t *testing.T, ctx context.Context, db *sql.DB) time.Duration {
	t.Helper()
	var jobs int
	var seconds int64
	err := db.QueryRowContext(ctx, `
		SELECT count(*),
		       coalesce(max(extract(epoch FROM (j.config->>'start_offset')::interval))::bigint, -1)
		  FROM timescaledb_information.jobs j
		  JOIN timescaledb_information.continuous_aggregates c
		    -- Refresh jobs name the cagg view on TimescaleDB 2.26 and its
		    -- materialization hypertable on older 2.x; match either.
		    ON j.hypertable_name IN (c.view_name, c.materialization_hypertable_name)
		 WHERE c.view_name = 'prices_1m'
		   AND j.proc_name = 'policy_refresh_continuous_aggregate'`).Scan(&jobs, &seconds)
	if err != nil {
		t.Fatalf("read prices_1m refresh policy start_offset: %v", err)
	}
	if jobs != 1 {
		t.Fatalf("prices_1m has %d refresh policies, want 1", jobs)
	}
	return time.Duration(seconds) * time.Second
}
