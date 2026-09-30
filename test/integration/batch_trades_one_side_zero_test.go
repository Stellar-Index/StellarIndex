//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestBatchInsertTrades_OneSideZeroFillIsStored pins that an SDEX
// one-side-zero fill (a leg that rounded to 0 stroops) is stored by the batch
// writer, one row each, beside the good trades of its batch, and counted on
// stellarindex_trades_zero_leg_admitted_total. A both-zero row is still
// malformed: it is dropped before the all-or-nothing INSERT (so it cannot
// sink the batch) and counted as an insert error.
func TestBatchInsertTrades_OneSideZeroFillIsStored(t *testing.T) {
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

	// Three ordinary trades, a zero-quote and a zero-base fill, and one
	// both-zero row, interleaved so no special row is conveniently first or last.
	good := []c.Trade{
		mkTrade(60_000_000, 1_000_000_000, 12_000_000),
		mkTrade(60_000_001, 2_000_000_000, 24_000_000),
		mkTrade(60_000_002, 3_000_000_000, 36_000_000),
	}
	zeroQuote := mkTrade(60_000_003, 5_000_000_000, 0)
	zeroBase := mkTrade(60_000_004, 0, 7_000_000)
	bothZero := mkTrade(60_000_005, 0, 0)
	batch := []c.Trade{good[0], zeroQuote, good[1], bothZero, zeroBase, good[2]}

	errKind := obs.InsertErrorKindTradeDropped
	zeroBefore := testutil.ToFloat64(obs.TradesZeroLegAdmittedTotal.WithLabelValues("sdex"))
	errBefore := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues("sdex", errKind))

	if err := store.BatchInsertTrades(ctx, batch); err != nil {
		t.Fatalf("BatchInsertTrades: %v", err)
	}

	stored := append(append([]c.Trade{}, good...), zeroQuote, zeroBase)
	if n := countTradesForSource(t, store, "sdex"); n != len(stored) {
		t.Fatalf("stored sdex trades = %d, want %d (3 good + 2 one-side-zero)", n, len(stored))
	}
	for _, g := range stored {
		if !tradeExists(t, store, g.Source, g.TxHash, g.OpIndex) {
			t.Errorf("trade ledger=%d base=%s quote=%s missing", g.Ledger, g.BaseAmount, g.QuoteAmount)
		}
	}
	if tradeExists(t, store, bothZero.Source, bothZero.TxHash, bothZero.OpIndex) {
		t.Errorf("both-zero row tx=%s was stored; Validate must reject it", bothZero.TxHash)
	}

	zeroDelta := testutil.ToFloat64(obs.TradesZeroLegAdmittedTotal.WithLabelValues("sdex")) - zeroBefore
	if zeroDelta != 2 {
		t.Errorf("TradesZeroLegAdmittedTotal{sdex} delta = %v, want 2 (one per stored one-side-zero fill)", zeroDelta)
	}
	t.Logf("TradesZeroLegAdmittedTotal{sdex} delta = %v", zeroDelta)
	if d := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues("sdex", errKind)) - errBefore; d != 1 {
		t.Errorf("SourceInsertErrorsTotal{sdex,%s} delta = %v, want 1 (the both-zero row only)", errKind, d)
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
