//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestBLNTEvents_AmountsSurviveStorageExactly pins invariant 1 on the blnt_events write path: an i128
// above 2^63 comes back digit for digit from the NUMERIC columns and from inside `attributes`, an
// absent field is NULL rather than 0, and a replay of the same event does not add a row.
func TestBLNTEvents_AmountsSurviveStorageExactly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		huge   = "170141183460469231731687303715884105727" // i128 max
		neg    = "-170141183460469231731687303715884105728"
		txHash = "5859bfb31336e8f7b98f15d5fac82893953e043393c17b7e9baf465274ce1ed1"
	)
	ts := time.Now().UTC().Truncate(time.Second)
	row := timescale.BLNTEvent{
		ContractID: blnt.MainnetBackstop, Ledger: 65_379_171, LedgerCloseTime: ts, TxHash: txHash,
		OpIndex: 0, EventIndex: 1, ContractRole: string(blnt.RoleBackstop), EventKind: "claim",
		Pool: blnt.MainnetFixedPool, UserAddress: "GDEA2WMOWYQ3T56A2PZH5QYYEQBAORW2AUUJYX4FFD5GDPESAICTFTAL",
		Tier: "FirstLoss", Amount: huge, Amount2: neg,
		Attributes: map[string]any{"shares": huge},
	}
	for range 2 {
		if err := store.InsertBLNTEvent(ctx, row); err != nil {
			t.Fatalf("InsertBLNTEvent: %v", err)
		}
	}
	if n := countRows(t, store, `SELECT COUNT(*) FROM blnt_events WHERE tx_hash = $1`, txHash); n != 1 {
		t.Fatalf("rows after a replayed insert = %d, want 1", n)
	}

	var (
		amount, amount2, tier string
		asset                 sql.NullString
		attrs                 []byte
	)
	err = store.DB().QueryRowContext(ctx,
		`SELECT amount::text, amount2::text, tier, asset, attributes FROM blnt_events WHERE tx_hash = $1`, txHash).
		Scan(&amount, &amount2, &tier, &asset, &attrs)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if amount != huge || amount2 != neg || tier != "FirstLoss" {
		t.Errorf("read back amount=%s amount2=%s tier=%s", amount, amount2, tier)
	}
	if asset.Valid {
		t.Errorf("asset = %q, want NULL for an event that carries none", asset.String)
	}
	var decoded map[string]any
	if err := json.Unmarshal(attrs, &decoded); err != nil {
		t.Fatalf("attributes: %v", err)
	}
	if decoded["shares"] != huge {
		t.Errorf("attributes.shares = %v, want the decimal string %s", decoded["shares"], huge)
	}

	row.TxHash = strings.Repeat("ab", 32)
	row.Tier = "FourthLoss"
	if err := store.InsertBLNTEvent(ctx, row); err == nil {
		t.Error("a tier the contract does not define was accepted")
	}
}

// TestMigration0226_BLNTEventsTxIndex: the column arrives nullable and NULL on existing rows, the previous binary's
// INSERT (which omits it) still works, a write without the index never clears a stored one, and the down drops it.
func TestMigration0226_BLNTEventsTxIndex(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	const (
		oldTx = "1111111111111111111111111111111111111111111111111111111111111111"
		newTx = "2222222222222222222222222222222222222222222222222222222222222222"
	)
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	legacyInsert := func(txHash string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO blnt_events (contract_id, ledger, ledger_close_time, tx_hash, op_index, event_index, contract_role, event_kind)
			VALUES ($1, 65800000, $2, $3, 0, 0, 'pool', 'set_status')`, blnt.MainnetFixedPool, ts, txHash); err != nil {
			t.Fatalf("pre-0226 insert: %v", err)
		}
	}
	applyMigrationsUpTo(t, dsn, 225)
	requireSchemaVersion(t, ctx, db, 225)
	legacyInsert(oldTx)

	applyMigrationsUpTo(t, dsn, 226)
	requireSchemaVersion(t, ctx, db, 226)
	var dataType, nullable string
	if err := db.QueryRowContext(ctx, `
		SELECT data_type, is_nullable FROM information_schema.columns
		 WHERE table_name = 'blnt_events' AND column_name = 'tx_index'`).Scan(&dataType, &nullable); err != nil {
		t.Fatalf("read blnt_events.tx_index: %v", err)
	}
	if dataType != "integer" || nullable != "YES" {
		t.Errorf("blnt_events.tx_index is (%s, nullable=%s), want (integer, YES)", dataType, nullable)
	}
	legacyInsert(newTx)
	txIndexOf := func(txHash string) sql.NullInt64 {
		t.Helper()
		var v sql.NullInt64
		if err := db.QueryRowContext(ctx, `SELECT tx_index FROM blnt_events WHERE tx_hash = $1`, txHash).Scan(&v); err != nil {
			t.Fatalf("read tx_index: %v", err)
		}
		return v
	}
	if a, b := txIndexOf(oldTx), txIndexOf(newTx); a.Valid || b.Valid {
		t.Errorf("tx_index after 0226 = (%v, %v), want NULL on both (no backfill, no default)", a, b)
	}

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	write := func(ti *uint32) {
		t.Helper()
		if err := store.InsertBLNTEvent(ctx, timescale.BLNTEvent{
			ContractID: blnt.MainnetFixedPool, Ledger: 65_800_000, LedgerCloseTime: ts, TxHash: oldTx,
			ContractRole: string(blnt.RolePool), EventKind: "set_status", TxIndex: ti,
		}); err != nil {
			t.Fatalf("InsertBLNTEvent: %v", err)
		}
	}
	three, zero := uint32(3), uint32(0)
	for _, step := range []struct {
		ti   *uint32
		want int64
	}{{&three, 3}, {nil, 3}, {&zero, 0}} {
		write(step.ti)
		if got := txIndexOf(oldTx); !got.Valid || got.Int64 != step.want {
			t.Fatalf("tx_index after writing %v = %v, want %d", step.ti, got, step.want)
		}
	}

	applyMigrationsUpTo(t, dsn, 225)
	requireSchemaVersion(t, ctx, db, 225)
	var present bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                WHERE table_name = 'blnt_events' AND column_name = 'tx_index')`).Scan(&present); err != nil {
		t.Fatalf("read blnt_events columns after down: %v", err)
	}
	if present {
		t.Error("blnt_events.tx_index still present after 0226 down")
	}
}
