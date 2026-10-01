//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestMigration0196_AddsNullableTxIndexOverCompressedChunks runs 0196 against
// two populated, compressed trades chunks: the ADD COLUMN must be
// catalog-only, nullable integer, NULL on every existing row, invisible to an
// INSERT that does not name it, and taggable inside a compressed chunk
// without tripping the per-DML decompression cap.
func TestMigration0196_AddsNullableTxIndexOverCompressedChunks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 195)
	requireSchemaVersion(t, ctx, db, 195)
	quiesceCAGGRefreshPolicies(t, ctx, db)

	t0 := time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour).Add(time.Hour)
	old := t0.Add(-30 * 24 * time.Hour)
	insertTxIndexTrade(t, ctx, db, "sdex", 60_000_001, "a1", t0)
	insertTxIndexTrade(t, ctx, db, "sdex", 60_000_001, "f1", t0)
	insertTxIndexTrade(t, ctx, db, "soroswap", 59_000_001, "b1", old)
	compressTradesChunks(t, ctx, db, 2)

	applyMigrationsUpTo(t, dsn, 196)
	requireSchemaVersion(t, ctx, db, 196)
	requireCompressedTradesChunks(t, ctx, db, 2, "after 0196 up")

	var dataType, nullable string
	if err := db.QueryRowContext(ctx, `
		SELECT data_type, is_nullable FROM information_schema.columns
		 WHERE table_name = 'trades' AND column_name = 'tx_index'`).Scan(&dataType, &nullable); err != nil {
		t.Fatalf("read trades.tx_index: %v", err)
	}
	if dataType != "integer" || nullable != "YES" {
		t.Errorf("trades.tx_index is (%s, nullable=%s), want (integer, YES)", dataType, nullable)
	}
	if n := countTxIndexTagged(t, ctx, db); n != 0 {
		t.Errorf("rows with a tx_index right after 0196 = %d, want 0 (no backfill in the migration)", n)
	}

	// The previous binary's INSERT does not name the column (rule 9).
	insertTxIndexTrade(t, ctx, db, "sdex", 60_000_002, "c1", t0.Add(time.Minute))

	// Tag the soroswap row inside the compressed 30-day-old chunk through the
	// production store method, bounded by its ts like the tagger bounds it.
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("timescale.Open: %v", err)
	}
	defer func() { _ = store.Close() }()
	n, err := store.TagTradesTxIndex(ctx, old, old.Add(time.Second), []timescale.TxIndexTag{
		{Ledger: 59_000_001, TxHash: txIndexHash("b1"), TxIndex: 5},
	})
	if err != nil {
		t.Fatalf("TagTradesTxIndex inside a compressed chunk: %v", err)
	}
	if n != 1 {
		t.Errorf("tagged %d rows in the compressed chunk, want 1", n)
	}

	applyMigrationsUpTo(t, dsn, 195)
	requireSchemaVersion(t, ctx, db, 195)
	var present bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                WHERE table_name = 'trades' AND column_name = 'tx_index')`).Scan(&present); err != nil {
		t.Fatalf("read trades columns after down: %v", err)
	}
	if present {
		t.Error("trades.tx_index still present after 0196 down")
	}
}

// TestTagTradesTxIndex_FillsOnChainRowsFirstWins drives the production walk
// (pipeline.TagTxIndexWindow → timescale.Store) against real SQL. The two
// same-ledger SDEX txs are applied in the REVERSE of their tx_hash order, so
// ORDER BY ledger, tx_index must disagree with the tx_hash tie key.
func TestTagTradesTxIndex_FillsOnChainRowsFirstWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	applyMigrationsUpTo(t, dsn, 196)
	quiesceCAGGRefreshPolicies(t, ctx, db)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("timescale.Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	t0 := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	insertTxIndexTrade(t, ctx, db, "sdex", 61_000_000, "aa", t0)
	insertTxIndexTrade(t, ctx, db, "sdex", 61_000_000, "ff", t0)
	insertTxIndexTrade(t, ctx, db, "soroswap", 61_000_001, "bb", t0.Add(5*time.Second))
	insertTxIndexTrade(t, ctx, db, "sdex", 60_999_000, "dd", t0.Add(-2*time.Hour))
	if _, err := db.ExecContext(ctx, `
		INSERT INTO trades (source, ledger, tx_hash, op_index, ts,
		                    base_asset, quote_asset, base_amount, quote_amount)
		VALUES ('binance', 0, $1, 0, $2, 'crypto:BTC', 'fiat:USD', 1, 60000)`,
		txIndexHash("cc"), t0); err != nil {
		t.Fatalf("seed off-chain trade: %v", err)
	}

	from, to := t0.Add(-time.Minute), t0.Add(time.Minute)
	keys, err := store.UntaggedTxIndexTrades(ctx, from, to, 0, "", 10)
	if err != nil {
		t.Fatalf("UntaggedTxIndexTrades: %v", err)
	}
	if got := keyHashes(keys); got != "aa,ff,bb" {
		t.Errorf("untagged keys = %s, want aa,ff,bb (on-chain, in-window, (ledger, tx_hash) order)", got)
	}
	page, err := store.UntaggedTxIndexTrades(ctx, from, to, keys[0].Ledger, keys[0].TxHash, 1)
	if err != nil || keyHashes(page) != "ff" {
		t.Errorf("keyset page after aa = %s (err %v), want ff", keyHashes(page), err)
	}

	lake := txIndexLake{
		txIndexHash("ff"): {{Ledger: 61_000_000, TxIndex: 0}},
		txIndexHash("aa"): {{Ledger: 61_000_000, TxIndex: 1}},
		txIndexHash("bb"): {{Ledger: 61_000_001, TxIndex: 3}},
		txIndexHash("cc"): {{Ledger: 0, TxIndex: 7}},
		txIndexHash("dd"): {{Ledger: 60_999_000, TxIndex: 2}},
	}
	tagged, err := pipeline.TagTxIndexWindow(ctx, lake, store, from, to, pipeline.TxIndexPageSize)
	if err != nil {
		t.Fatalf("TagTxIndexWindow: %v", err)
	}
	if tagged != 3 {
		t.Errorf("tagged = %d, want 3 (two sdex + one soroswap)", tagged)
	}

	rows, err := db.QueryContext(ctx, `
		SELECT rtrim(tx_hash) FROM trades
		 WHERE ledger = 61000000 ORDER BY ledger, tx_index`)
	if err != nil {
		t.Fatalf("read apply order: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var order []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			t.Fatalf("scan: %v", err)
		}
		order = append(order, strings.TrimLeft(h, "0"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read apply order rows: %v", err)
	}
	if strings.Join(order, ",") != "ff,aa" {
		t.Errorf("ORDER BY ledger, tx_index = %v, want [ff aa] (apply order, not tx_hash order)", order)
	}

	for _, c := range []struct {
		name   string
		ledger int
		hash   string
	}{{"off-chain", 0, "cc"}, {"out-of-window", 60_999_000, "dd"}} {
		var idx sql.NullInt64
		if err := db.QueryRowContext(ctx,
			`SELECT tx_index FROM trades WHERE ledger = $1 AND tx_hash = $2`, c.ledger, txIndexHash(c.hash)).Scan(&idx); err != nil {
			t.Fatalf("read %s row: %v", c.name, err)
		}
		if idx.Valid {
			t.Errorf("%s row tagged with tx_index %d, want NULL", c.name, idx.Int64)
		}
	}

	// First-wins: swapped values change nothing.
	n, err := store.TagTradesTxIndex(ctx, from, to, []timescale.TxIndexTag{
		{Ledger: 61_000_000, TxHash: txIndexHash("aa"), TxIndex: 0},
		{Ledger: 61_000_000, TxHash: txIndexHash("ff"), TxIndex: 1},
	})
	if err != nil || n != 0 {
		t.Errorf("re-tag with swapped values: n=%d err=%v, want 0, nil", n, err)
	}
	if rest, err := store.UntaggedTxIndexTrades(ctx, from, to, 0, "", 10); err != nil || len(rest) != 0 {
		t.Errorf("untagged after the walk = %s (err %v), want none", keyHashes(rest), err)
	}
}

type txIndexLake map[string][]clickhouse.TxLedgerIndex

func (m txIndexLake) TxLedgerIndexes(_ context.Context, hashes []string) (map[string][]clickhouse.TxLedgerIndex, error) {
	out := map[string][]clickhouse.TxLedgerIndex{}
	for _, h := range hashes {
		if v, ok := m[h]; ok {
			out[h] = v
		}
	}
	return out, nil
}

// txIndexHash left-pads a short label to trades.tx_hash's char(64).
func txIndexHash(label string) string {
	return fmt.Sprintf("%064s", label)
}

func keyHashes(keys []timescale.TxIndexKey) string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = strings.TrimLeft(k.TxHash, "0")
	}
	return strings.Join(out, ",")
}

// insertTxIndexTrade writes one on-chain trade with the pre-0196 column list.
func insertTxIndexTrade(t *testing.T, ctx context.Context, db *sql.DB, source string, ledger int, label string, ts time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO trades (source, ledger, tx_hash, op_index, ts,
		                    base_asset, quote_asset, base_amount, quote_amount)
		VALUES ($1, $2, $3, 0, $4, 'native', $5, 1000, 5000)`,
		source, ledger, txIndexHash(label), ts, "TXI-"+priceableIssuer); err != nil {
		t.Fatalf("seed %s trade %s: %v", source, label, err)
	}
}

func countTxIndexTagged(t *testing.T, ctx context.Context, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM trades WHERE tx_index IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("count tagged trades: %v", err)
	}
	return n
}
