//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestHypertableChunkIntervalFloor executes 0062 + 0171 against real
// TimescaleDB: after the full migration set no public hypertable may be
// left with chunks narrower than 7 days (the chunk-count / lock-pressure
// pathology 0062 names). The textual twin is
// internal/storage/timescale/chunk_interval_floor_test.go.
func TestHypertableChunkIntervalFloor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 167)
	if w := chunkInterval(t, ctx, db, "sep41_transfers"); w != "1 day" {
		t.Fatalf("pre-0171 sep41_transfers chunk interval = %q, want \"1 day\" — the fixture no longer reproduces", w)
	}

	applyMigrations(t, dsn)
	const q = `
		SELECT hypertable_name, time_interval::text
		  FROM timescaledb_information.dimensions
		 WHERE hypertable_schema = 'public'
		   AND dimension_type = 'Time'
		   AND time_interval < INTERVAL '7 days'
		 ORDER BY hypertable_name`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query dimensions: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, width string
		if err := rows.Scan(&name, &width); err != nil {
			t.Fatalf("scan: %v", err)
		}
		t.Errorf("hypertable %s has chunk interval %s, want >= 7 days", name, width)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	for _, rel := range []string{
		"oracle_updates", "api_usage_events", "soroswap_skim_events",
		"blend_positions", "blend_emissions", "blend_admin",
		"sep41_transfers", "blend_backstop_events", "aquarius_rewards_events",
	} {
		if w := chunkInterval(t, ctx, db, rel); w != "7 days" {
			t.Errorf("%s chunk interval after 0171 = %q, want \"7 days\"", rel, w)
		}
	}
}

func chunkInterval(t *testing.T, ctx context.Context, db *sql.DB, rel string) string {
	t.Helper()
	const q = `
		SELECT time_interval::text
		  FROM timescaledb_information.dimensions
		 WHERE hypertable_schema = 'public'
		   AND hypertable_name = $1
		   AND dimension_type = 'Time'`
	var w string
	if err := db.QueryRowContext(ctx, q, rel).Scan(&w); err != nil {
		t.Fatalf("read %s chunk interval: %v", rel, err)
	}
	return w
}
