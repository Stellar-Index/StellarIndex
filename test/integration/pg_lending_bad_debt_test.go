//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestLendingBadDebtEvents_BlendWriterEncodingsReadBack feeds each writer's output through the reader: an empty bid map written by
// InsertBlendFillAuction reads back as a zero-bid fill (covered nothing), a fill with no auction data (Blend V1) as unrecorded, a
// bid entry exact above 2^64; only bad-debt fills, this pool's credit events and the backstop's own repays are read.
func TestLendingBadDebtEvents_BlendWriterEncodingsReadBack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store := openLendingStore(t, ctx)

	pool, other, backstop, user := mustContractStrkey(t, 71), mustContractStrkey(t, 72), mustContractStrkey(t, 73), mustAccountStrkey(t, 74)
	assetC := mustContractStrkey(t, 75)
	asset, err := canonical.NewSorobanAsset(assetC)
	if err != nil {
		t.Fatal(err)
	}
	big64, _ := new(big.Int).SetString("18446744073709551621", 10)
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for i, e := range []domain.BlendEmissionEvent{
		{Pool: pool, Kind: domain.BlendEventBadDebt, Asset: assetC, User: user, Amount: big64},
		{Pool: pool, Kind: domain.BlendEventDefaultedDebt, Asset: assetC, Amount: big.NewInt(7)},
		{Pool: pool, Kind: domain.BlendEventGulp, Asset: assetC, Amount: big.NewInt(9)},
		{Pool: other, Kind: domain.BlendEventBadDebt, Asset: assetC, User: user, Amount: big.NewInt(9)},
	} {
		e.Ledger, e.TxHash, e.Timestamp = uint32(300+i), pad64("e1", i), ts //nolint:gosec // i < 4
		if err := store.InsertBlendEmissionEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	bid := blend.AuctionData{Bid: []blend.AssetAmount{{Asset: asset, Amount: big64}}, Lot: []blend.AssetAmount{{Asset: asset, Amount: big.NewInt(1)}}, Block: 100}
	empty := blend.AuctionData{Bid: []blend.AssetAmount{}, Lot: []blend.AssetAmount{{Asset: asset, Amount: big.NewInt(1)}}, Block: 100}
	for i, f := range []struct {
		pool string
		typ  uint32
		data *blend.AuctionData
	}{{pool, 1, &bid}, {pool, 1, &empty}, {pool, 1, nil}, {pool, 0, &bid}, {other, 1, &bid}} {
		if err := store.InsertBlendFillAuction(ctx, blend.FillAuctionEvent{
			Pool: f.pool, AuctionType: f.typ, User: backstop, Filler: user, FillPercent: big.NewInt(100), Data: f.data,
			Ledger: uint32(310 + i), TxHash: pad64("f1", i), Timestamp: ts, //nolint:gosec // i < 5
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i, who := range []string{backstop, user} {
		if err := store.InsertBlendPositionEvent(ctx, domain.BlendPositionEvent{
			Pool: pool, Kind: "repay", Asset: assetC, User: who, TokenAmount: big.NewInt(4), BOrDAmount: big.NewInt(3),
			Ledger: uint32(320 + i), TxHash: pad64("p1", i), Timestamp: ts, //nolint:gosec // i < 2
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.LendingBadDebtEvents(ctx, pool, backstop, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []timescale.LendingBadDebtEvent{
		{Kind: timescale.BadDebtKindMoved, Asset: assetC, DTokens: "18446744073709551621", Ledger: 300},
		{Kind: timescale.BadDebtKindDefaulted, Asset: assetC, DTokens: "7", Ledger: 301},
		{Kind: timescale.BadDebtKindFill, Asset: assetC, DTokens: "18446744073709551621", Ledger: 310},
		{Kind: timescale.BadDebtKindFillZeroBid, Ledger: 311},
		{Kind: timescale.BadDebtKindFillBidUnrecorded, Ledger: 312},
		{Kind: timescale.BadDebtKindRepay, Asset: assetC, DTokens: "3", Ledger: 320},
	}
	if len(got) != len(want) {
		t.Fatalf("events = %+v, want %d", got, len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.Kind != w.Kind || g.Asset != w.Asset || g.DTokens != w.DTokens || g.Ledger != w.Ledger || len(g.TxHash) != 64 {
			t.Errorf("event %d = %+v, want %+v", i, g, w)
		}
	}
	if noBackstop, err := store.LendingBadDebtEvents(ctx, pool, "", false); err != nil || len(noBackstop) != len(want)-1 {
		t.Errorf("without a backstop: %d events (%v), want no repay", len(noBackstop), err)
	}
}

// TestLendingBadDebtEvents_BLNTFills: an empty filled bid object covered nothing; a fill without one is unrecorded.
func TestLendingBadDebtEvents_BLNTFills(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store := openLendingStore(t, ctx)

	const pool = blnt.MainnetFixedPool
	user, asset := mustAccountStrkey(t, 81), mustContractStrkey(t, 82)
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for i, attrs := range []map[string]any{
		{"auction_type": 1, "filled_auction_data": map[string]any{"bid": map[string]any{asset: "5"}, "lot": map[string]any{}, "block": 1}},
		{"auction_type": 1, "filled_auction_data": map[string]any{"bid": map[string]any{}, "lot": map[string]any{}, "block": 1}},
		{"auction_type": 1},
		{"auction_type": 0, "filled_auction_data": map[string]any{"bid": map[string]any{asset: "5"}, "lot": map[string]any{}, "block": 1}},
	} {
		if err := store.InsertBLNTEvent(ctx, timescale.BLNTEvent{
			ContractID: pool, Ledger: uint32(65_910_001 + i), LedgerCloseTime: ts, TxHash: pad64("b1", i), //nolint:gosec // i < 4
			ContractRole: string(blnt.RolePool), EventKind: "fill_auction", Pool: pool, UserAddress: user, Attributes: attrs,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.LendingBadDebtEvents(ctx, pool, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Kind != timescale.BadDebtKindFill || got[0].DTokens != "5" || got[0].Asset != asset ||
		got[1].Kind != timescale.BadDebtKindFillZeroBid || got[2].Kind != timescale.BadDebtKindFillBidUnrecorded {
		t.Fatalf("BLNT events = %+v", got)
	}
}
