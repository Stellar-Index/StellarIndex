//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestLendingLiquidations_BlendFillsNewestFirstWithAmounts: user-liquidation and bad-debt fills are paged newest first with the
// filled amounts exact above 2^64; an interest fill is not a liquidation; a fill with no amounts on the wire has none, not zero.
func TestLendingLiquidations_BlendFillsNewestFirstWithAmounts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store := openLendingStore(t, ctx)

	pool, user, filler := mustContractStrkey(t, 51), mustAccountStrkey(t, 52), mustAccountStrkey(t, 53)
	asset, err := canonical.NewSorobanAsset(mustContractStrkey(t, 54))
	if err != nil {
		t.Fatal(err)
	}
	big64, _ := new(big.Int).SetString("18446744073709551621", 10)
	data := blend.AuctionData{Bid: []blend.AssetAmount{{Asset: asset, Amount: big64}}, Lot: []blend.AssetAmount{{Asset: asset, Amount: big.NewInt(3)}}, Block: 100}
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for i, f := range []struct {
		typ  uint32
		data *blend.AuctionData
	}{{0, &data}, {2, &data}, {1, &data}, {0, nil}} {
		if err := store.InsertBlendFillAuction(ctx, blend.FillAuctionEvent{
			Pool: pool, AuctionType: f.typ, User: user, Filler: filler, FillPercent: big.NewInt(100), Data: f.data,
			Ledger: uint32(200 + i), TxHash: pad64("c1", i), Timestamp: ts.Add(time.Duration(i) * time.Second), //nolint:gosec // i < 4
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.LendingLiquidations(ctx, pool, 10, timescale.LendingConfigCursor{})
	if err != nil || len(got) != 3 {
		t.Fatalf("liquidations = %+v (%v), want 3 (the interest fill excluded)", got, err)
	}
	noAmounts, badDebt, first := got[0], got[1], got[2]
	if noAmounts.Ledger != 203 || noAmounts.HasAmounts || noAmounts.Bid != nil {
		t.Errorf("newest fill = %+v, want ledger 203 with no amounts", noAmounts)
	}
	if badDebt.AuctionType != 1 || first.AuctionType != 0 || first.Bid[asset.String()] != "18446744073709551621" ||
		first.Lot[asset.String()] != "3" || first.Filler != filler || first.Block == nil || *first.Block != 100 {
		t.Errorf("bad debt %+v first %+v", badDebt, first)
	}
	page, err := store.LendingLiquidations(ctx, pool, 1, timescale.LendingConfigCursor{
		Ts: badDebt.Ts, Ledger: badDebt.Ledger, TxHash: badDebt.TxHash, OpIndex: badDebt.OpIndex, EventIndex: badDebt.EventIndex,
	})
	if err != nil || len(page) != 1 || page[0].Ledger != first.Ledger {
		t.Fatalf("page after cursor = %+v (%v), want the first fill", page, err)
	}
}

// TestLendingLiquidations_BLNTFilledAmounts: a BLNT pool's fills come from blnt_events with filled_auction_data's maps.
func TestLendingLiquidations_BLNTFilledAmounts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store := openLendingStore(t, ctx)

	const pool = blnt.MainnetFixedPool
	user, filler, asset := mustAccountStrkey(t, 61), mustAccountStrkey(t, 62), mustContractStrkey(t, 63)
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := store.InsertBLNTEvent(ctx, timescale.BLNTEvent{
		ContractID: pool, Ledger: 65_900_001, LedgerCloseTime: ts, TxHash: pad64("d1", 1), ContractRole: string(blnt.RolePool),
		EventKind: "fill_auction", Pool: pool, UserAddress: user, Attributes: map[string]any{
			"auction_type": 0, "filler": filler, "fill_percent": "50", "filled_auction_data": map[string]any{
				"bid": map[string]any{asset: "18446744073709551621"}, "lot": map[string]any{asset: "5"}, "block": 65_900_000,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.LendingLiquidations(ctx, pool, 10, timescale.LendingConfigCursor{})
	if err != nil || len(got) != 1 {
		t.Fatalf("liquidations = %+v (%v)", got, err)
	}
	f := got[0]
	if !f.HasAmounts || f.Bid[asset] != "18446744073709551621" || f.Lot[asset] != "5" || f.FillPercent != "50" || f.Block == nil {
		t.Fatalf("BLNT fill = %+v", f)
	}
}
