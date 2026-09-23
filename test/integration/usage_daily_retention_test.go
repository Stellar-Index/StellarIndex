//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestUsageDailyRetention pins GH-1282: `usage_daily` holds per-account
// request history and kept it forever. 0167 must attach exactly one
// armed 12-month retention job, and its down must remove it.
func TestUsageDailyRetention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 165)
	if n, _, _ := usageDailyRetention(t, ctx, db); n != 0 {
		t.Fatalf("pre-0167 usage_daily retention jobs = %d, want 0", n)
	}

	applyMigrationsUpTo(t, dsn, 167)
	n, dropAfter, scheduled := usageDailyRetention(t, ctx, db)
	if n != 1 {
		t.Fatalf("usage_daily retention jobs after 0167 = %d, want 1", n)
	}
	if dropAfter != "1 year" {
		t.Errorf("usage_daily drop_after = %q, want \"1 year\" (12 months)", dropAfter)
	}
	if !scheduled {
		t.Error("usage_daily retention job is not scheduled — 0167 ships it armed")
	}

	applyMigrationsUpTo(t, dsn, 165)
	if n, _, _ := usageDailyRetention(t, ctx, db); n != 0 {
		t.Fatalf("usage_daily retention jobs after 0167 down = %d, want 0", n)
	}
}

func usageDailyRetention(t *testing.T, ctx context.Context, db *sql.DB) (jobs int, dropAfter string, scheduled bool) {
	t.Helper()
	const q = `
		SELECT COUNT(*),
		       COALESCE(MAX(config->>'drop_after'), ''),
		       COALESCE(BOOL_AND(scheduled), false)
		  FROM timescaledb_information.jobs
		 WHERE proc_name = 'policy_retention'
		   AND hypertable_name = 'usage_daily'`
	if err := db.QueryRowContext(ctx, q).Scan(&jobs, &dropAfter, &scheduled); err != nil {
		t.Fatalf("read usage_daily retention job: %v", err)
	}
	return jobs, dropAfter, scheduled
}
