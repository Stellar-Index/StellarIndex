//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestOracleUpdate_ReDeriveThatMovesIdentityDoesNotDuplicate reproduces
// GH-1328: oracle_updates' primary key carries ts, and the reflector
// op_index formula changed (eventFanoutStride), so a replay over pre-change
// history writes a second row for the same observation instead of
// correcting the first. The assertions state the CORRECT outcome (one row
// per observation); they fail on the current schema and writer.
func TestOracleUpdate_ReDeriveThatMovesIdentityDoesNotDuplicate(t *testing.T) {
	t.Skip("GH-1328 open: the supersede design (identity lookup without a ts predicate " +
		"across an unretained hypertable) is unresolved; remove this skip to reproduce")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	xlm, err := c.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// Operation 1, event 0, vector slot 3 under the old formula
	// (op*1024 + i) and the current one ((op*64 + ev)*1024 + i).
	old := c.OracleUpdate{
		Source: "reflector-dex", Ledger: 50_000_321, TxHash: strings.Repeat("cd", 32),
		OpIndex: 1*1024 + 3, Timestamp: ts, Asset: xlm, Quote: usd,
		Price: c.NewAmount(big.NewInt(12_345_678_901_234)), Decimals: 14,
	}
	if err := store.InsertOracleUpdate(ctx, old); err != nil {
		t.Fatalf("insert pre-change row: %v", err)
	}

	rederived := old
	rederived.OpIndex = (1*64+0)*1024 + 3
	if err := store.InsertOracleUpdate(ctx, rederived); err != nil {
		t.Fatalf("insert re-derived row: %v", err)
	}
	moved := old
	moved.OpIndex = rederived.OpIndex
	moved.Timestamp = ts.Add(time.Second)
	if err := store.InsertOracleUpdate(ctx, moved); err != nil {
		t.Fatalf("insert ts-corrected row: %v", err)
	}

	var rows int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM oracle_updates WHERE source = $1 AND ledger = $2 AND tx_hash = $3`,
		old.Source, old.Ledger, old.TxHash).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("oracle_updates rows for one observation = %d after re-derives that moved op_index and ts, want 1", rows)
	}
}
