//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
)

const migration0191File = "0191_trades_drop_amount_checks.down.sql"

// TestMigration0191_DropsTradesAmountChecksOverCompressedChunks runs 0191
// against the shape production is in: two populated, compressed trades
// chunks. Up must be catalog-only (every chunk still compressed), remove
// both of 0001's inline CHECKs and admit a one-side-zero fill into a
// compressed chunk's range while prices_1m's 0187 filter keeps it out of
// every price expression. Down must refuse LOUDLY over zero-leg rows and
// over compressed chunks, and restore both CHECKs by their 0001 names once
// neither obstacle remains.
func TestMigration0191_DropsTradesAmountChecksOverCompressedChunks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 190)
	requireSchemaVersion(t, ctx, db, 190)
	requireTradesAmountChecks(t, ctx, db, true, "at 190")
	quiesceCAGGRefreshPolicies(t, ctx, db)

	pair := ohlcDustPair{base: "PRZL-" + priceableIssuer, quote: "native"}
	// Two priceable fills at t0 (two days ago) and one thirty days earlier,
	// so the table spans two 1-day chunks.
	t0 := time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour).Add(time.Hour)
	seed(t, db, ctx, pair, []seedTrade{
		{off: 0, base: "1000", quote: "5000", usd: "1"},
		{off: time.Second, base: "1000", quote: "6000", usd: "1"},
		{off: -30 * 24 * time.Hour, base: "1000", quote: "5000", usd: "1"},
	}, t0)
	compressTradesChunks(t, ctx, db, 2)
	requireCompressedTradesChunks(t, ctx, db, 2, "before 0191")

	// At 190 the CHECK is still what rejects a zero leg, for a row aimed
	// at the compressed chunk too.
	assertInsertRejected(t, db, ctx, "zero quote_amount at 190", zeroLegInsert(pair, t0, "ad", "1", "0"))

	applyMigrationsUpTo(t, dsn, 191)
	requireSchemaVersion(t, ctx, db, 191)
	requireCompressedTradesChunks(t, ctx, db, 2, "after 0191 up")
	requireTradesAmountChecks(t, ctx, db, false, "after 0191 up")

	// One zero leg each way, into the compressed chunk's range.
	seed(t, db, ctx, pair, []seedTrade{
		{off: 10 * time.Second, base: "0", quote: "7000"},
		{off: 20 * time.Second, base: "3000", quote: "0"},
	}, t0)
	if n := countZeroLegTrades(t, ctx, db); n != 2 {
		t.Fatalf("zero-leg trades stored after 0191 = %d, want 2", n)
	}
	requireCompressedTradesChunks(t, ctx, db, 2, "after zero-leg inserts")

	if _, err := db.ExecContext(ctx, priceableRefreshRecipe[0]); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}
	got := readPriceableRow(t, ctx, db, "1m", pair, t0)
	assertNumeric(t, "prices_1m.vwap", got.vwap, "5.5")
	assertNumeric(t, "prices_1m.twap", got.twap, "5.5")
	assertNumeric(t, "prices_1m.high_price", got.high, "6")
	assertNumeric(t, "prices_1m.low_price", got.low, "5")
	assertExact(t, "prices_1m.volume_priced", got.volumePriced, "2000")
	assertExact(t, "prices_1m.volume_quote", got.volumeQuote, "18000")
	assertExact(t, "prices_1m.volume", got.volume, "5000")
	if got.tradeCount != 4 {
		t.Errorf("prices_1m.trade_count = %d, want 4 (zero-leg fills are trades, just unpriceable)", got.tradeCount)
	}
	var notional int64
	if err := db.QueryRowContext(ctx, `
		SELECT notional_trade_count FROM prices_1m
		 WHERE base_asset = $1 AND quote_asset = $2 AND bucket <= $3
		 ORDER BY bucket DESC LIMIT 1`, pair.base, pair.quote, t0).Scan(&notional); err != nil {
		t.Fatalf("read prices_1m.notional_trade_count: %v", err)
	}
	if notional != 2 {
		t.Errorf("prices_1m.notional_trade_count = %d, want 2 (only the priceable fills)", notional)
	}

	// twap_1h is hierarchical over prices_1m; the zero-leg fills must not
	// reach it either.
	if _, err := db.ExecContext(ctx, "CALL refresh_continuous_aggregate('twap_1h', now() - INTERVAL '7 days', now(), force => true)"); err != nil {
		t.Fatalf("refresh twap_1h: %v", err)
	}
	var twap sql.NullString
	var samples int64
	if err := db.QueryRowContext(ctx, `
		SELECT twap, sample_count FROM twap_1h
		 WHERE base_asset = $1 AND quote_asset = $2 AND bucket <= $3
		 ORDER BY bucket DESC LIMIT 1`, pair.base, pair.quote, t0).Scan(&twap, &samples); err != nil {
		t.Fatalf("read twap_1h: %v", err)
	}
	assertNumeric(t, "twap_1h.twap", twap, "5.5")
	if samples != 1 {
		t.Errorf("twap_1h.sample_count = %d, want 1 (one priced minute; zero-leg fills add none)", samples)
	}

	// Down with zero-leg rows present: refused before any DDL.
	refuse0191Down(t, ctx, db, dsn, "zero or negative leg")
	if n := countZeroLegTrades(t, ctx, db); n != 2 {
		t.Fatalf("zero-leg trades after refused down = %d, want 2 (the down deleted data)", n)
	}

	// Rows gone, chunks still compressed: refused on the compression guard.
	if _, err := db.ExecContext(ctx, `DELETE FROM trades WHERE base_amount = 0 OR quote_amount = 0`); err != nil {
		t.Fatalf("delete zero-leg rows: %v", err)
	}
	refuse0191Down(t, ctx, db, dsn, "compressed chunks")
	requireCompressedTradesChunks(t, ctx, db, 2, "after refused downs")

	// Neither obstacle: down restores both CHECKs under their 0001 names.
	if _, err := db.ExecContext(ctx, `
		SELECT count(*) FROM (
			SELECT decompress_chunk(c, if_compressed => true) FROM show_chunks('trades') c
		) s`); err != nil {
		t.Fatalf("decompress trades chunks: %v", err)
	}
	requireCompressedTradesChunks(t, ctx, db, 0, "before the clean down")
	applyMigrationsUpTo(t, dsn, 190)
	requireSchemaVersion(t, ctx, db, 190)
	requireTradesAmountChecks(t, ctx, db, true, "after 0191 down")
	assertInsertRejected(t, db, ctx, "zero base_amount after 0191 down", zeroLegInsert(pair, t0, "ae", "0", "1"))
}

// zeroLegInsert is a literal INSERT (assertInsertRejected takes no bind
// parameters) aimed at the seeded chunk.
func zeroLegInsert(p ohlcDustPair, ts time.Time, txHash, base, quote string) string {
	return fmt.Sprintf(`
		INSERT INTO trades (source, ledger, tx_hash, op_index, ts,
		                    base_asset, quote_asset, base_amount, quote_amount)
		VALUES ('sdex', 1, '%s', 0, TIMESTAMPTZ '%s', '%s', '%s', %s, %s)`,
		txHash, ts.Format(time.RFC3339Nano), p.base, p.quote, base, quote)
}

// refuse0191Down runs the 191→190 down expecting the DO-block guard to RAISE
// with the file name and "LOUD", asserts the DDL was rolled back (no CHECK
// re-added) and that golang-migrate left schema_migrations dirty at 190 —
// the state an operator sees — then forces it back to a clean 191.
func refuse0191Down(t *testing.T, ctx context.Context, db *sql.DB, dsn, guard string) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	err = m.Migrate(190)
	_, _ = m.Close() // a failed Migrate keeps its advisory lock; Force needs a fresh instance
	if err == nil || !strings.Contains(err.Error(), migration0191File) ||
		!strings.Contains(err.Error(), "LOUD") || !strings.Contains(err.Error(), guard) {
		t.Fatalf("0191 down with %s: err = %v, want the %s guard to refuse", guard, err, migration0191File)
	}
	requireTradesAmountChecks(t, ctx, db, false, "after refused down ("+guard+")")

	var version int
	var dirty bool
	if err := db.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if version != 190 || !dirty {
		t.Fatalf("schema_migrations after refused down = %d dirty=%v, want 190 dirty", version, dirty)
	}
	f, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = f.Close() }()
	if err := f.Force(191); err != nil {
		t.Fatalf("force 191: %v", err)
	}
	requireSchemaVersion(t, ctx, db, 191)
}

func requireTradesAmountChecks(t *testing.T, ctx context.Context, db *sql.DB, want bool, when string) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_constraint
		 WHERE conrelid = 'trades'::regclass
		   AND conname IN ('trades_base_amount_check', 'trades_quote_amount_check')`).Scan(&n); err != nil {
		t.Fatalf("read pg_constraint: %v", err)
	}
	wantN := 0
	if want {
		wantN = 2
	}
	if n != wantN {
		t.Fatalf("%s: trades amount CHECKs present = %d, want %d", when, n, wantN)
	}
}

// compressTradesChunks compresses every trades chunk, counted from
// compress_chunk itself rather than the information view.
func compressTradesChunks(t *testing.T, ctx context.Context, db *sql.DB, want int) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM (
			SELECT compress_chunk(c, if_not_compressed => true) FROM show_chunks('trades') c
		) s`).Scan(&n); err != nil {
		t.Fatalf("compress trades chunks: %v", err)
	}
	if n != want {
		t.Fatalf("compress_chunk over trades touched %d chunks, want %d", n, want)
	}
}

func requireCompressedTradesChunks(t *testing.T, ctx context.Context, db *sql.DB, want int, when string) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM timescaledb_information.chunks
		 WHERE hypertable_name = 'trades' AND is_compressed`).Scan(&n); err != nil {
		t.Fatalf("read timescaledb_information.chunks: %v", err)
	}
	if n != want {
		t.Fatalf("%s: compressed trades chunks = %d, want %d", when, n, want)
	}
}

func countZeroLegTrades(t *testing.T, ctx context.Context, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM trades WHERE base_amount = 0 OR quote_amount = 0`).Scan(&n); err != nil {
		t.Fatalf("count zero-leg trades: %v", err)
	}
	return n
}
