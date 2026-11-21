//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// TestMigrations0233to0234_GuardsOverCompressedChunks applies 0233-0234 over a compressed fx_fixings chunk: the
// chunk stays compressed, fx_fixings refuses UPDATE/DELETE/TRUNCATE but still takes the writer's ON CONFLICT
// insert, the aquarius_rewards_events comment is corrected, and the downs undo it.
func TestMigrations0233to0234_GuardsOverCompressedChunks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 232)
	requireSchemaVersion(t, ctx, db, 232)
	quiesceCAGGRefreshPolicies(t, ctx, db)

	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour).Add(time.Hour).Add(-30 * 24 * time.Hour)
	const fxInsert = `
		INSERT INTO fx_fixings (ticker, grain, bar_start, bar_end, rate_usd, source, generation)
		VALUES ('EUR', '1h', $1, $1::timestamptz + interval '1 hour', $2, 'massive', $3)
		ON CONFLICT DO NOTHING`
	if _, err := db.ExecContext(ctx, fxInsert, old, "0.91", 0); err != nil {
		t.Fatalf("seed fx_fixings: %v", err)
	}
	if _, err := db.ExecContext(ctx, `SELECT compress_chunk(c) FROM show_chunks('fx_fixings') c`); err != nil {
		t.Fatalf("compress fx_fixings: %v", err)
	}
	requireCompressedFxChunk(t, ctx, db, "before 0233 up")

	applyMigrationsUpTo(t, dsn, 234)
	requireSchemaVersion(t, ctx, db, 234)
	requireCompressedFxChunk(t, ctx, db, "after 0234 up")

	refused := map[string]string{
		"update":   `UPDATE fx_fixings SET rate_usd = 0.5 WHERE ticker = 'EUR'`,
		"delete":   `DELETE FROM fx_fixings WHERE ticker = 'EUR'`,
		"truncate": `TRUNCATE fx_fixings`,
	}
	for name, q := range refused {
		if _, err := db.ExecContext(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("fx_fixings %s: err = %v, want the append-only trigger to refuse it", name, err)
		}
	}
	// The writer's shape: a duplicate is skipped, a correction lands at a higher generation.
	for gen, rate := range []string{"0.99", "0.92"} {
		if _, err := db.ExecContext(ctx, fxInsert, old, rate, gen); err != nil {
			t.Fatalf("fx_fixings insert generation %d: %v", gen, err)
		}
	}
	var rates string
	if err := db.QueryRowContext(ctx, `
		SELECT string_agg(rate_usd::text, ',' ORDER BY generation) FROM fx_fixings WHERE ticker = 'EUR'`).Scan(&rates); err != nil {
		t.Fatalf("read fx_fixings: %v", err)
	}
	if rates != "0.91,0.92" {
		t.Errorf("fx_fixings EUR rates by generation = %q, want 0.91,0.92", rates)
	}

	var comment string
	if err := db.QueryRowContext(ctx, `SELECT obj_description('aquarius_rewards_events'::regclass)`).Scan(&comment); err != nil {
		t.Fatalf("read aquarius_rewards_events comment: %v", err)
	}
	if !strings.Contains(comment, "12 kinds") || !strings.Contains(comment, "config_rewards") {
		t.Errorf("aquarius_rewards_events comment = %q, want 12 kinds including config_rewards", comment)
	}

	applyMigrationsUpTo(t, dsn, 232)
	requireSchemaVersion(t, ctx, db, 232)
	if _, err := db.ExecContext(ctx, `UPDATE fx_fixings SET source = 'massive' WHERE ticker = 'EUR'`); err != nil {
		t.Errorf("fx_fixings update after 0233 down: %v, want the trigger gone", err)
	}
}

func requireCompressedFxChunk(t *testing.T, ctx context.Context, db *sql.DB, when string) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM timescaledb_information.chunks
		 WHERE hypertable_name = 'fx_fixings' AND is_compressed`).Scan(&n); err != nil {
		t.Fatalf("read timescaledb_information.chunks: %v", err)
	}
	if n != 1 {
		t.Fatalf("%s: compressed fx_fixings chunks = %d, want 1", when, n)
	}
}
