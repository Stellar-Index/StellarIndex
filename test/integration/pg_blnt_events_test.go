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
