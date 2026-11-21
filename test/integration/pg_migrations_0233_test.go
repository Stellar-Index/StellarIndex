//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// TestMigrations0233to0235_GuardsOverCompressedChunks applies 0233-0235 over compressed fx_fixings and trades
// chunks: the chunks stay compressed, fx_fixings refuses UPDATE/DELETE/TRUNCATE but still takes the writer's
// ON CONFLICT insert, trades refuses a negative or 0/0 leg while keeping a one-zero-leg fill, and the downs undo it.
func TestMigrations0233to0235_GuardsOverCompressedChunks(t *testing.T) {
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

	t0 := time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour).Add(time.Hour)
	old := t0.Add(-30 * 24 * time.Hour)
	insertTxIndexTrade(t, ctx, db, "sdex", 60_000_001, "a1", t0)
	insertTxIndexTrade(t, ctx, db, "soroswap", 59_000_001, "b1", old)
	compressTradesChunks(t, ctx, db, 2)

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

	applyMigrationsUpTo(t, dsn, 235)
	requireSchemaVersion(t, ctx, db, 235)
	requireCompressedTradesChunks(t, ctx, db, 2, "after 0235 up")

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

	const tradeInsert = `
		INSERT INTO trades (source, ledger, tx_hash, op_index, ts, base_asset, quote_asset, base_amount, quote_amount)
		VALUES ('sdex', $1, $2, 0, $3, 'native', $4, $5, $6)`
	legs := []struct {
		name       string
		base, quot string
		ok         bool
	}{
		{"negative base", "-1", "5", false},
		{"negative quote", "5", "-1", false},
		{"both zero", "0", "0", false},
		{"zero base leg", "0", "5", true},
		{"zero quote leg", "5", "0", true},
	}
	for i, l := range legs {
		_, err := db.ExecContext(ctx, tradeInsert, 60_100_000+i, txIndexHash("leg"+l.name), t0, "TXI-"+priceableIssuer, l.base, l.quot)
		if l.ok && err != nil {
			t.Errorf("%s: insert refused: %v", l.name, err)
		}
		if !l.ok && (err == nil || !strings.Contains(err.Error(), "trades_amount_legs_check")) {
			t.Errorf("%s: err = %v, want trades_amount_legs_check to refuse it", l.name, err)
		}
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
	if _, err := db.ExecContext(ctx, tradeInsert, 60_200_000, txIndexHash("after-down"), t0, "TXI-"+priceableIssuer, "-1", "5"); err != nil {
		t.Errorf("negative leg after 0235 down: %v, want the CHECK gone", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE fx_fixings SET source = 'massive' WHERE ticker = 'EUR'`); err != nil {
		t.Errorf("fx_fixings update after 0233 down: %v, want the trigger gone", err)
	}
}
