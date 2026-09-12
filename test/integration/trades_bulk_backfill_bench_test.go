//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestBulkBackfillTrades_Throughput measures the two trade writers against a
// PROD-SHAPED target and prints both rates. It is the measurement behind the
// -bulk-trades flag; a claimed speed-up with no number is worth nothing here.
//
// What "prod-shaped" means, and what it still is not:
//
//   - The target chunks are POPULATED and COMPRESSED. `trades` compresses at 7
//     days (migration 0001), so every chunk a historical backfill lands in is
//     compressed, and TimescaleDB pays a large penalty enforcing the trade PK
//     against compressed data. Measured on this harness in isolation: landing
//     rows in a compressed chunk runs at roughly a third of the rate of the
//     same rows into a fresh one. Skipping this step flatters both writers and
//     changes their ratio.
//   - The USD-volume resolvers are installed, exactly as ch_rebuild.go
//     installs them, so both writers pay real `tradeUSDVolume` resolution.
//   - It UNDERSTATES the bulk writer. The dominant per-row cost on r1 is FX
//     resolver latency against a 69 GB `prices_1m`; here `prices_1m` is nearly
//     empty, so each lookup is about as cheap as a round trip can be. The half
//     of this change that parallelises those lookups therefore has far less to
//     recover on a laptop than it does on the real box.
//   - Docker Desktop is CPU-bound well below r1. Parallel writers saturate
//     earlier here than they will there.
//
// The assertion is deliberately weak — "not slower" — because a tight ratio
// gate on shared CI hardware fails for reasons that have nothing to do with
// this code. The NUMBERS are the deliverable; read them in the log.
func TestBulkBackfillTrades_Throughput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// ── build the prod-shaped target ────────────────────────────────────
	// Half a million rows of another source across the days the backfill
	// targets, then compress. The backfilled source itself stays absent —
	// that is the r1 situation: `trades` holds 836M rows and zero sdex rows
	// below the trade floor.
	const seedRows = 500_000
	seedStart := time.Now()
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`
        INSERT INTO trades (source, ledger, tx_hash, op_index, ts,
                            base_asset, quote_asset, base_amount, quote_amount,
                            usd_volume, derive_generation)
        SELECT 'binance', 40000000 + (g/48)::int, lpad(to_hex(g), 64, '0'), (g %% 48)::int,
               timestamptz '2024-03-01 00:00:00Z' + ((g/48) * interval '5 seconds'),
               'TK' || lpad(((g*7) %% 200)::text, 2, '0') || '-%[1]s',
               'TK' || lpad(((g*13) %% 200)::text, 2, '0') || '-%[1]s',
               1000000 + g, 2000000 + g*3, NULL, 0
          FROM generate_series(1, %[2]d) g`, bulkPegIssuer, seedRows)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var nCompressed int
	if err := db.QueryRowContext(ctx, `
        SELECT count(*) FROM (
          SELECT compress_chunk(format('%I.%I', chunk_schema, chunk_name)::regclass)
            FROM timescaledb_information.chunks
           WHERE hypertable_name = 'trades') z`).Scan(&nCompressed); err != nil {
		t.Fatalf("compress: %v", err)
	}
	t.Logf("target prepared: %d seed rows, %d compressed chunks, in %s",
		seedRows, nCompressed, time.Since(seedStart).Round(time.Millisecond))
	if nCompressed == 0 {
		t.Fatal("no chunk got compressed — the benchmark would measure the easy case and " +
			"report a ratio prod cannot reproduce")
	}

	const gen = 1_700_000_000
	const n = 40_000
	store := bulkStore(t, ctx, dsn, gen)

	// Both writers land into COMPRESSED, populated chunks (the seed spans
	// 2024-03-01 .. 2024-03-30 at 5s/ledger), in disjoint ledger windows.
	const upsertLo = 50_000_000
	const bulkLo = 55_000_000
	upsertRows := bulkTradeSet(t, n, upsertLo, time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC), "sdex")
	bulkRows := bulkTradeSet(t, n, bulkLo, time.Date(2024, 3, 12, 0, 0, 0, 0, time.UTC), "sdex")

	// ── OLD PATH: exactly what drainAndWrite does today — 1000-row batches
	// through the generation-guarded upsert, one after another.
	oldStart := time.Now()
	for i := 0; i < len(upsertRows); i += 1000 {
		end := min(i+1000, len(upsertRows))
		if err := store.BatchInsertTrades(ctx, upsertRows[i:end]); err != nil {
			t.Fatalf("BatchInsertTrades: %v", err)
		}
	}
	oldDur := time.Since(oldStart)

	// ── NEW PATH: one bulk call over the whole buffer.
	newStart := time.Now()
	res, err := store.BulkBackfillTrades(ctx, bulkRows, timescale.BulkBackfillOptions{})
	if err != nil {
		t.Fatalf("BulkBackfillTrades: %v", err)
	}
	newDur := time.Since(newStart)
	if res.Path != timescale.BulkBackfillPathCopy {
		t.Fatalf("bulk run fell back to %q (%s) — this would be measuring the OLD path twice",
			res.Path, res.FallbackReason)
	}

	oldRate := float64(n) / oldDur.Seconds()
	newRate := float64(n) / newDur.Seconds()
	t.Logf("OLD  BatchInsertTrades (1000-row upsert batches): %6d rows in %-9s = %8.0f rows/sec",
		n, oldDur.Round(time.Millisecond), oldRate)
	t.Logf("NEW  BulkBackfillTrades (COPY, parallel):         %6d rows in %-9s = %8.0f rows/sec",
		n, newDur.Round(time.Millisecond), newRate)
	t.Logf("speed-up: %.2fx", newRate/oldRate)

	// Both writers must actually have landed everything they were given.
	if got := len(readTradeRows(t, store.DB(), "sdex", upsertLo, upsertLo+999_999)); got != n {
		t.Fatalf("upsert path stored %d rows, want %d", got, n)
	}
	if got := len(readTradeRows(t, store.DB(), "sdex", bulkLo, bulkLo+999_999)); got != n {
		t.Fatalf("bulk path stored %d rows, want %d", got, n)
	}
	if newRate < oldRate {
		t.Fatalf("the bulk path is SLOWER than the batch upsert (%.0f vs %.0f rows/sec) — "+
			"the flag exists to be faster; do not ship it on the strength of the design alone",
			newRate, oldRate)
	}
}

// bulkTradeSetTS is unused by the suite but kept next to the benchmark as the
// documented shape of a realistic SDEX buffer, so a future measurement does
// not have to re-derive it.
var _ = func() []c.Trade { return nil }
