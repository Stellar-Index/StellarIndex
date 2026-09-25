//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestUpshiftVaultShareSupplies_DeployedTiebreakWithinLedger pins the
// fix for CA2-A13-harden-6: when a single ledger carries two
// deployed_assets_changed events for the same vault (a rebalance that
// spans multiple ops/events in one transaction), the DISTINCT ON must
// resolve deterministically to the highest (op_index, event_index) —
// the last-emitted new_amount — never to whichever row the physical
// scan happens to return first.
func TestUpshiftVaultShareSupplies_DeployedTiebreakWithinLedger(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	vault := contractStrkeyFromSeed(t, 0xE0)
	closeTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// One ledger, one tx, two deployed_assets_changed events from the
	// same rebalance: op_index/event_index 3 fires first with
	// new_amount=1000, then op_index/event_index 7 supersedes it with
	// new_amount=0. Everything else ties (same contract, ledger,
	// ledger_close_time), so only the op/event tiebreak can resolve
	// which row is "latest".
	events := []timescale.UpshiftVaultEvent{
		{
			ContractID: vault, Ledger: 100, LedgerCloseTime: closeTime,
			TxHash: "deploytx", OpIndex: 3, EventIndex: 3,
			Kind: timescale.UpshiftDeployedAssetsChanged, Caller: vault,
			OldAmount: canonical.NewAmount(big.NewInt(0)),
			NewAmount: canonical.NewAmount(big.NewInt(1_000)),
		},
		{
			ContractID: vault, Ledger: 100, LedgerCloseTime: closeTime,
			TxHash: "deploytx", OpIndex: 7, EventIndex: 7,
			Kind: timescale.UpshiftDeployedAssetsChanged, Caller: vault,
			OldAmount: canonical.NewAmount(big.NewInt(1_000)),
			NewAmount: canonical.NewAmount(big.NewInt(0)),
		},
		{
			ContractID: vault, Ledger: 100, LedgerCloseTime: closeTime,
			TxHash: "deptx", OpIndex: 1, EventIndex: 1,
			Kind: timescale.UpshiftDeposit, Caller: vault,
			Receiver: vault, Owner: vault,
			Assets: canonical.NewAmount(big.NewInt(500)),
			Shares: canonical.NewAmount(big.NewInt(500)),
		},
	}
	for _, e := range events {
		if err := store.InsertUpshiftVaultEvent(ctx, e); err != nil {
			t.Fatalf("InsertUpshiftVaultEvent %+v: %v", e, err)
		}
	}

	got, err := store.UpshiftVaultShareSupplies(ctx)
	if err != nil {
		t.Fatalf("UpshiftVaultShareSupplies: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("UpshiftVaultShareSupplies returned %d rows, want 1: %#v", len(got), got)
	}
	row := got[0]
	if !row.HasDeployedTotal {
		t.Fatalf("row %#v: HasDeployedTotal = false, want true", row)
	}
	want := canonical.NewAmount(big.NewInt(0))
	if row.DeployedAssets.String() != want.String() {
		t.Errorf("DeployedAssets = %s, want %s (the higher op_index/event_index event within the tied ledger)",
			row.DeployedAssets, want)
	}
}
