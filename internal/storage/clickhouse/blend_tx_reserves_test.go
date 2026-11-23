package clickhouse

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
)

// TestFoldTxReserves_OnlyTheRequestedTransactionsImage: the fill's own transaction's ResData is kept; a later transaction in the
// same ledger (its own rate), an unrequested pair, a removal and a foreign key are not the fill's state.
func TestFoldTxReserves_OnlyTheRequestedTransactionsImage(t *testing.T) {
	fill := LendingTxRef{Ledger: 10, TxHash: "aa"}
	later := LendingTxRef{Ledger: 10, TxHash: "bb"}
	removed := LendingTxRef{Ledger: 11, TxHash: "cc"}
	rows := []txReserveRow{
		{ref: fill, key: "kUSDC", entry: histResData(t, "1100000000000", "1000000000000", histBSupply, histDSupply)},
		{ref: later, key: "kUSDC", entry: histResData(t, "900000000000", "1000000000000", histBSupply, histDSupply)},
		{ref: removed, key: "kUSDC", entry: ""},
		{ref: fill, key: "kOTHER", entry: histResData(t, "5000000000000", "1000000000000", histBSupply, histDSupply)},
	}
	want := map[LendingTxRef]bool{fill: true, removed: true}
	got := foldTxReserves(rows, want, map[string]string{"kUSDC": "CUSDC"}, blendModel(blend.PoolV2))
	if len(got) != 1 {
		t.Fatalf("got %d states, want only the fill transaction's: %+v", len(got), got)
	}
	rd, ok := got[LendingTxReserve{LendingTxRef: fill, Asset: "CUSDC"}]
	if !ok || rd.BRate == nil || rd.BRate.String() != "1100000000000" {
		t.Fatalf("fill state = %+v (ok %v), want b_rate 1100000000000", rd, ok)
	}
}

// TestLendingReservesAtTxs_UnknownModelRefused: a protocol with no reserve model is an error, never a guessed rate scale.
func TestLendingReservesAtTxs_UnknownModelRefused(t *testing.T) {
	if _, err := (&ExplorerReader{}).LendingReservesAtTxs(t.Context(), blendTestPool, "blend", []string{blendTestAssetSAC},
		[]LendingTxRef{{Ledger: 1, TxHash: "aa"}}); err == nil {
		t.Fatal("want an error for a protocol with no reserve model")
	}
}
