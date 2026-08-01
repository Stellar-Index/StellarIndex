//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestBatchInsertTrades_OneSideZeroFillDoesNotSinkBatch is the W1-defi-1
// proof. The SDEX decoder KEEPS one-side-zero fills (a leg that rounded to 0
// is still a real on-chain trade effect the ADR-0033 census counts and the
// ClickHouse substrate retains — internal/sources/sdex/decode.go), but the
// served `trades` tier's CHECK (base_amount > 0 AND quote_amount > 0, INV-6 /
// migration 0001) cannot hold a zero leg.
//
// BatchInsertTrades issues ONE all-or-nothing multi-row INSERT, so before the
// fix a single one-side-zero row aborted the whole statement: BatchInsertTrades
// returned a CHECK-violation error, the entire batch of otherwise-good trades
// rolled back (0 rows landed), and the downstream per-row isolation counted the
// expected fill as an insert error — tripping stellarindex_source_insert_errors_total
// (the spurious alert).
//
// Post-fix, filterStorableTrades drops the unstorable row up front: the batch
// commits, every good trade lands in ONE round-trip, and the zero-leg row is
// simply absent from the served tier (consistent with the single-row
// InsertTrade Validate gate AND the authoritative completeness reconcile's
// own Validate gate, reDeriveSDEXCensusViaDecoder).
func TestBatchInsertTrades_OneSideZeroFillDoesNotSinkBatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const issuerG = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", issuerG)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	ts := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	mkTrade := func(ledger uint32, base, quote int64) c.Trade {
		return c.Trade{
			Source:      "sdex",
			Ledger:      ledger,
			TxHash:      fmt.Sprintf("%064x", ledger),
			OpIndex:     0,
			Timestamp:   ts,
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(base)),
			QuoteAmount: c.NewAmount(big.NewInt(quote)),
		}
	}

	// Three ordinary trades PLUS one one-side-zero fill (quote leg rounded to
	// 0), interleaved so the unstorable row is not conveniently first or last.
	good := []c.Trade{
		mkTrade(60_000_000, 1_000_000_000, 12_000_000),
		mkTrade(60_000_001, 2_000_000_000, 24_000_000),
		mkTrade(60_000_002, 3_000_000_000, 36_000_000),
	}
	zeroLeg := mkTrade(60_000_003, 5_000_000_000, 0)
	batch := []c.Trade{good[0], zeroLeg, good[1], good[2]}

	// Pre-fix: this returns a CHECK-violation error and NOTHING lands.
	if err := store.BatchInsertTrades(ctx, batch); err != nil {
		t.Fatalf("BatchInsertTrades returned error on a batch containing a one-side-zero fill: %v\n"+
			"the zero-leg row must be filtered before the all-or-nothing INSERT, not abort it", err)
	}

	// Every GOOD trade landed in the single batch round-trip.
	if n := countTradesForSource(t, store, "sdex"); n != len(good) {
		t.Fatalf("stored sdex trades = %d, want %d — a one-side-zero fill rolled back the whole batch", n, len(good))
	}
	for _, g := range good {
		if !tradeExists(t, store, g.Source, g.TxHash, g.OpIndex) {
			t.Errorf("good trade ledger=%d tx=%s missing — batch-wide failure dropped it", g.Ledger, g.TxHash)
		}
	}

	// The one-side-zero row is intentionally NOT stored (served tier holds no
	// price for it; it lives on in the CH substrate and is census-counted).
	if tradeExists(t, store, zeroLeg.Source, zeroLeg.TxHash, zeroLeg.OpIndex) {
		t.Errorf("one-side-zero fill tx=%s was stored — the served CHECK (base/quote > 0) must not hold it", zeroLeg.TxHash)
	}
}

// countTradesForSource returns the number of rows in `trades` for a source.
func countTradesForSource(t *testing.T, store *timescale.Store, source string) int {
	t.Helper()
	const q = `SELECT COUNT(*) FROM trades WHERE source = $1`
	var n int
	if err := store.DB().QueryRow(q, source).Scan(&n); err != nil {
		t.Fatalf("countTradesForSource %s: %v", source, err)
	}
	return n
}

// tradeExists reports whether a specific (source, tx_hash, op_index) trade row
// is present.
func tradeExists(t *testing.T, store *timescale.Store, source, txHash string, opIndex uint32) bool {
	t.Helper()
	const q = `SELECT COUNT(*) FROM trades WHERE source = $1 AND tx_hash = $2 AND op_index = $3`
	var n int
	if err := store.DB().QueryRow(q, source, txHash, opIndex).Scan(&n); err != nil {
		t.Fatalf("tradeExists %s/%s/%d: %v", source, txHash, opIndex, err)
	}
	return n > 0
}
