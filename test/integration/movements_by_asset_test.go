//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"
)

// TestClickHouseMovementsByAsset pins INV-2140's table: the MV copies live
// account_movements rows, both participants of one movement survive FINAL,
// an Int128 amount above 2^64 stays exact, and the operator catch-up
// statement (partition-bounded INSERT..SELECT) is idempotent over MV rows.
func TestClickHouseMovementsByAsset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		asset  = "TESTA-GISSUERMOVEMENTSBYASSET"
		other  = "TESTB-GISSUERMOVEMENTSBYASSET"
		ledger = uint32(78_000_001)
		tx     = "mba-tx-1"
	)
	big128 := new(big.Int).Lsh(big.NewInt(1), 100)
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

	b, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.account_movements
		(address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
		 movement_kind, provenance, asset, counterparty, amount)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	rows := [][]any{
		{"GSENDER", ledger, at, tx, uint32(0), uint32(0), "sent", "payment", "classic", asset, "GRECV", big128},
		{"GRECV", ledger, at, tx, uint32(0), uint32(0), "received", "payment", "classic", asset, "GSENDER", big128},
		{"GSENDER", ledger, at, "mba-tx-2", uint32(0), uint32(0), "sent", "payment", "classic", other, "GRECV", big.NewInt(5)},
	}
	for _, r := range rows {
		if err := b.Append(r...); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send: %v", err)
	}

	count := func(t *testing.T) (n uint64, sum string) {
		t.Helper()
		if err := raw.QueryRow(ctx, `SELECT count(), toString(sum(amount)) FROM stellar.movements_by_asset FINAL
			WHERE asset = ? AND ledger = ?`, asset, ledger).Scan(&n, &sum); err != nil {
			t.Fatalf("query: %v", err)
		}
		return n, sum
	}
	want := new(big.Int).Mul(big128, big.NewInt(2)).String()
	if n, sum := count(t); n != 2 || sum != want {
		t.Fatalf("after MV: n=%d sum=%s, want 2 rows summing %s", n, sum, want)
	}

	const catchUp = `INSERT INTO stellar.movements_by_asset
		SELECT address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
		       movement_kind, provenance, asset, counterparty, amount, attributes, ingested_at
		FROM stellar.account_movements
		WHERE ledger >= 78000000 AND ledger < 79000000`
	if err := raw.Exec(ctx, catchUp); err != nil {
		t.Fatalf("catch-up: %v", err)
	}
	if n, sum := count(t); n != 2 || sum != want {
		t.Fatalf("after catch-up: n=%d sum=%s, want 2 rows summing %s", n, sum, want)
	}
}
