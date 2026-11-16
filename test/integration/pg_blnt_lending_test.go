//go:build integration

package integration_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestBLNTLendingReads_PoolListAssetsAndFoldedPosition runs the BLNT lending readers on real blnt_events rows: the
// pool list withholds a net flow across two assets, the asset list is the pool's, a supply leg above 2^63 folds
// exactly, and a user-liquidation fill supersedes both legs it moved for the user and the filler.
func TestBLNTLendingReads_PoolListAssetsAndFoldedPosition(t *testing.T) {
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
		pool   = blnt.MainnetFixedPool
		user   = "GDEA2WMOWYQ3T56A2PZH5QYYEQBAORW2AUUJYX4FFD5GDPESAICTFTAL"
		filler = "GAVG3ODZ4SAVK2WJL3F3RT265RL7P6QNOMA6NL3XDAREKSX3OWWMXF4R"
		assetA = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
		assetB = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	)
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	ev := func(i uint32, kind, who, asset, amount, amount2 string, attrs map[string]any) timescale.BLNTEvent {
		return timescale.BLNTEvent{
			ContractID: pool, Ledger: 65_500_000 + i, LedgerCloseTime: ts.Add(time.Duration(i) * time.Second),
			TxHash: pad64("b17", int(i)), ContractRole: string(blnt.RolePool), EventKind: kind, Pool: pool,
			UserAddress: who, Asset: asset, Amount: amount, Amount2: amount2, Attributes: attrs,
		}
	}
	for _, e := range []timescale.BLNTEvent{
		ev(1, "supply_collateral", user, assetA, "18446744073709551621", "18446744073709551000", nil), // 2^64 + 5
		ev(2, "withdraw_collateral", user, assetA, "5", "4", nil),
		ev(3, "borrow", user, assetB, "40", "40", nil),
		ev(4, "supply_collateral", user, assetB, "50", "50", nil),
		ev(5, "fill_auction", user, "", "", "", map[string]any{
			"auction_type": 0, "filler": filler, "fill_percent": "100",
			"filled_auction_data": map[string]any{"bid": map[string]any{assetB: "40"}, "lot": map[string]any{assetB: "50"}, "block": 1},
		}),
	} {
		if err := store.InsertBLNTEvent(ctx, e); err != nil {
			t.Fatalf("InsertBLNTEvent %s: %v", e.EventKind, err)
		}
	}

	pools, err := store.ListBLNTPools(ctx)
	if err != nil {
		t.Fatalf("ListBLNTPools: %v", err)
	}
	if len(pools) != 1 || pools[0].Pool != pool || pools[0].AuctionsTotal != 1 || pools[0].UniqueUsers30d != 1 ||
		pools[0].NetSupplied30d != nil || pools[0].NetBorrowed30d != nil {
		t.Fatalf("pools = %+v, want one pool with no cross-asset net flow", pools)
	}
	assets, err := store.BLNTPoolAssets(ctx, pool)
	if err != nil {
		t.Fatalf("BLNTPoolAssets: %v", err)
	}
	if want := []string{assetB, assetA}; !reflect.DeepEqual(assets, want) {
		t.Errorf("assets = %v, want %v", assets, want)
	}
	if none, err := store.BLNTPoolAssets(ctx, blnt.MainnetBackstop); err != nil || len(none) != 0 {
		t.Errorf("non-pool contract assets = %v (%v), want none", none, err)
	}

	folds, err := store.BLNTPositionsByUser(ctx, user)
	if err != nil {
		t.Fatalf("BLNTPositionsByUser: %v", err)
	}
	byAsset := map[string]timescale.BlendPositionFold{}
	for _, f := range folds {
		byAsset[f.Asset] = f
	}
	if a := byAsset[assetA]; !a.HasSupplyLeg || a.SupplyNet != "18446744073709551616" || a.SupplySuperseded || a.HasBorrowLeg {
		t.Errorf("asset A fold = %+v, want supply net 2^64 exactly", a)
	}
	if b := byAsset[assetB]; !b.SupplySuperseded || b.SupplyTokens != "0" || !b.BorrowSuperseded || b.BorrowTokens != "0" {
		t.Errorf("asset B fold = %+v, want both legs superseded with zero tokens", b)
	}
	got, err := store.BLNTPositionsByUser(ctx, filler)
	if err != nil {
		t.Fatalf("BLNTPositionsByUser filler: %v", err)
	}
	if len(got) != 1 || got[0].Asset != assetB || got[0].SupplyTokens != "50" || got[0].BorrowTokens != "40" ||
		!got[0].SupplySuperseded || !got[0].BorrowSuperseded {
		t.Errorf("filler folds = %+v, want the moved lot and bid", got)
	}
}
