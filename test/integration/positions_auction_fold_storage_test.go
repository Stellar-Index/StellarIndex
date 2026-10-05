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
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	auctionPool       = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	auctionCollateral = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"
	auctionDebt       = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	auctionBackstopLP = "CAQF5KNOFIGRI24NQRRGUPD46Q45MGMXZMRTQFXS25Y4NZVNPT34GM6S"
	liquidatedAcct    = "GLIQUIDATEDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	fillerAcct        = "GFILLERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	badDebtAcct       = "GBADDEBTAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	bystanderAcct     = "GBYSTANDERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	backstopAcct      = "CBACKSTOPAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	badDebtFillerAcct = "GBADDEBTFILLERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

// auctionLeg is one expected Blend leg: underlying net, superseded flag,
// b/d-token balance, last ledger.
type auctionLeg struct {
	net        string
	superseded bool
	tokens     string
	ledger     uint32
}

// TestPositionsFold_BlendAuctionSupersedesLegs pins, through real SQL,
// that BlendPositionsByUser flags every Blend leg a liquidation fill, a
// bad-debt auction fill or a bad_debt write-off moved — for the auctioned
// account AND the filler, after a PARTIAL fill, and still after later
// activity on the leg — with the exact b/d-token balance counting those
// moves, while an Interest auction touches nothing; and that
// DeFiPositionHolders leaves every moved leg out.
func TestPositionsFold_BlendAuctionSupersedesLegs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	seedBlendAuctionFixture(ctx, t, store, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))

	type fold struct{ supply, borrow *auctionLeg }
	cases := map[string]map[string]fold{
		// b 9000 + 600 in, 4500 + 5100 seized -> 0 bTokens; d 4800 in,
		// 2400 + 2000 seized -> 400 dTokens still owed.
		liquidatedAcct: {
			auctionCollateral: {supply: &auctionLeg{"10700", true, "0", 106}},
			auctionDebt:       {borrow: &auctionLeg{"5000", true, "400", 106}},
		},
		// Own supply (b 45) plus both lots; took on both bids with no
		// blend_positions borrow row of its own.
		fillerAcct: {
			auctionCollateral: {supply: &auctionLeg{"50", true, "9645", 106}},
			auctionDebt:       {borrow: &auctionLeg{"0", true, "4400", 106}},
		},
		badDebtAcct:       {auctionDebt: {borrow: &auctionLeg{"30", true, "0", 104}}},
		backstopAcct:      {auctionDebt: {borrow: &auctionLeg{"0", true, "-28", 108}}},
		badDebtFillerAcct: {auctionDebt: {borrow: &auctionLeg{"0", true, "28", 108}}},
		bystanderAcct:     {auctionCollateral: {supply: &auctionLeg{"10", false, "10", 100}}},
	}
	for acct, want := range cases {
		folds, err := store.BlendPositionsByUser(ctx, acct)
		if err != nil {
			t.Fatalf("BlendPositionsByUser(%s): %v", acct, err)
		}
		if len(folds) != len(want) {
			t.Fatalf("%s folds = %+v, want %d (pool, asset) rows", acct, folds, len(want))
		}
		for _, f := range folds {
			w := want[f.Asset]
			var got fold
			if f.HasSupplyLeg {
				got.supply = &auctionLeg{f.SupplyNet, f.SupplySuperseded, f.SupplyTokens, f.SupplyLastLedger}
			}
			if f.HasBorrowLeg {
				got.borrow = &auctionLeg{f.BorrowNet, f.BorrowSuperseded, f.BorrowTokens, f.BorrowLastLedger}
			}
			if f.Pool != auctionPool || !sameAuctionLeg(got.supply, w.supply) || !sameAuctionLeg(got.borrow, w.borrow) {
				t.Errorf("%s %s fold = %+v, want supply %+v borrow %+v", acct, f.Asset, f, w.supply, w.borrow)
			}
		}
	}

	holders, err := store.DeFiPositionHolders(ctx)
	if err != nil {
		t.Fatalf("DeFiPositionHolders: %v", err)
	}
	var blendLegs []timescale.DeFiPositionHolder
	for _, h := range holders {
		if h.Protocol == "blend" && (h.PositionKind == "lending_supply" || h.PositionKind == "lending_borrow") {
			blendLegs = append(blendLegs, h)
		}
	}
	if len(blendLegs) != 1 || blendLegs[0].User != bystanderAcct || blendLegs[0].Amount != "10" {
		t.Errorf("blend holder legs = %+v, want only the untouched bystander supply of 10", blendLegs)
	}
}

func sameAuctionLeg(a, b *auctionLeg) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// seedBlendAuctionFixture writes the money-market events, two partial
// UserLiquidation fills with user activity between them, a BadDebt fill,
// an Interest fill (which must move no pool position) and a bad_debt
// write-off.
func seedBlendAuctionFixture(ctx context.Context, t *testing.T, store *timescale.Store, t0 time.Time) {
	t.Helper()
	at := func(ledger uint32) time.Time { return t0.Add(time.Duration(ledger) * time.Second) }
	positions := []struct {
		kind, asset, acct string
		amount, tokens    int64
		ledger            uint32
	}{
		{blend.EventSupplyCollateral, auctionCollateral, liquidatedAcct, 10000, 9000, 100},
		{blend.EventBorrow, auctionDebt, liquidatedAcct, 5000, 4800, 101},
		{blend.EventSupplyCollateral, auctionCollateral, liquidatedAcct, 700, 600, 105},
		{blend.EventSupply, auctionCollateral, fillerAcct, 50, 45, 99},
		{blend.EventSupply, auctionCollateral, bystanderAcct, 10, 10, 100},
		{blend.EventBorrow, auctionDebt, badDebtAcct, 30, 28, 101},
	}
	for i, r := range positions {
		if err := store.InsertBlendPositionEvent(ctx, domain.BlendPositionEvent{
			Pool: auctionPool, Kind: r.kind, Asset: r.asset, User: r.acct,
			TokenAmount: big.NewInt(r.amount), BOrDAmount: big.NewInt(r.tokens),
			Ledger: r.ledger, TxHash: pad64("a", i), Timestamp: at(r.ledger),
		}); err != nil {
			t.Fatalf("InsertBlendPositionEvent %d: %v", i, err)
		}
	}
	amounts := func(asset string, n int64) []blend.AssetAmount {
		return []blend.AssetAmount{{Asset: canonical.Asset{Type: canonical.AssetSoroban, ContractID: asset}, Amount: big.NewInt(n)}}
	}
	fills := []blend.FillAuctionEvent{
		{
			AuctionType: 0, User: liquidatedAcct, Filler: fillerAcct, FillPercent: big.NewInt(50), Ledger: 103,
			Data: &blend.AuctionData{Bid: amounts(auctionDebt, 2400), Lot: amounts(auctionCollateral, 4500)},
		},
		{
			AuctionType: 0, User: liquidatedAcct, Filler: fillerAcct, FillPercent: big.NewInt(100), Ledger: 106,
			Data: &blend.AuctionData{Bid: amounts(auctionDebt, 2000), Lot: amounts(auctionCollateral, 5100)},
		},
		{
			AuctionType: 2, User: backstopAcct, Filler: bystanderAcct, FillPercent: big.NewInt(100), Ledger: 107,
			Data: &blend.AuctionData{Bid: amounts(auctionBackstopLP, 70), Lot: amounts(auctionCollateral, 90)},
		},
		{
			AuctionType: 1, User: backstopAcct, Filler: badDebtFillerAcct, FillPercent: big.NewInt(100), Ledger: 108,
			Data: &blend.AuctionData{Bid: amounts(auctionDebt, 28), Lot: amounts(auctionBackstopLP, 60)},
		},
	}
	for i, f := range fills {
		f.Pool, f.TxHash, f.Timestamp = auctionPool, pad64("b", i), at(f.Ledger)
		if err := store.InsertBlendFillAuction(ctx, f); err != nil {
			t.Fatalf("InsertBlendFillAuction %d: %v", i, err)
		}
	}
	if err := store.InsertBlendEmissionEvent(ctx, domain.BlendEmissionEvent{
		Pool: auctionPool, Kind: blend.EventBadDebt, Asset: auctionDebt, User: badDebtAcct,
		Amount: big.NewInt(28), Ledger: 104, TxHash: pad64("c", 0), Timestamp: at(104),
	}); err != nil {
		t.Fatalf("InsertBlendEmissionEvent bad_debt: %v", err)
	}
}
