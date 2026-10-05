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
// statement (partition-bounded INSERT..SELECT) is idempotent over MV rows and skips superseded source rows (FINAL).
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
		FROM stellar.account_movements FINAL
		WHERE ledger >= 78000000 AND ledger < 79000000`
	if err := raw.Exec(ctx, catchUp); err != nil {
		t.Fatalf("catch-up: %v", err)
	}
	if n, sum := count(t); n != 2 || sum != want {
		t.Fatalf("after catch-up: n=%d sum=%s, want 2 rows summing %s", n, sum, want)
	}

	// A superseded source row (same key, older ingested_at, different asset)
	// is still unmerged in account_movements; FINAL must keep it out.
	stale := "TESTSTALE-GISSUERMOVEMENTSBYASSET"
	if err := raw.Exec(ctx, `SYSTEM STOP MERGES stellar.account_movements`); err != nil {
		t.Fatalf("stop merges: %v", err)
	}
	t.Cleanup(func() { _ = raw.Exec(context.Background(), `SYSTEM START MERGES stellar.account_movements`) })
	// Two inserts = two parts, so the pair stays unmerged (merges are stopped).
	for _, r := range []struct{ asset, ts string }{{stale, "2026-09-01 00:00:00"}, {asset, "2026-09-02 00:00:00"}} {
		if err := raw.Exec(ctx, `INSERT INTO stellar.account_movements
			(address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
			 movement_kind, provenance, asset, counterparty, amount, ingested_at)
			VALUES ('GSTALE', 78000002, ?, 'mba-tx-stale', 0, 0, 'sent', 'payment', 'classic', ?, '', 1, ?)`,
			at, r.asset, r.ts); err != nil {
			t.Fatalf("insert stale pair: %v", err)
		}
	}
	if err := raw.Exec(ctx, `DELETE FROM stellar.movements_by_asset WHERE ledger = 78000002 SETTINGS mutations_sync = 2`); err != nil {
		t.Fatalf("clear MV rows: %v", err)
	}
	if err := raw.Exec(ctx, catchUp); err != nil {
		t.Fatalf("catch-up 2: %v", err)
	}
	var n uint64
	if err := raw.QueryRow(ctx, `SELECT count() FROM stellar.movements_by_asset WHERE ledger = 78000002 AND asset = ?`, stale).Scan(&n); err != nil {
		t.Fatalf("stale query: %v", err)
	}
	if n != 0 {
		t.Fatalf("stale superseded row copied by catch-up: %d rows under asset %s", n, stale)
	}
}
