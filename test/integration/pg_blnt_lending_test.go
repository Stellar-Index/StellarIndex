//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
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

// TestBLNTLendingReads_EventArms pins each fold arm with distinct, asymmetric values so a flipped sign or a dropped
// arm changes the result: clawback, bad debt after a fill, the three non-user fill types, flash loans, and the
// reserve-config lifecycle including a same-ledger tie.
func TestBLNTLendingReads_EventArms(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const pool = blnt.MainnetFixedPool
	clawed, debtor, liquidator := mustAccountStrkey(t, 1), mustAccountStrkey(t, 2), mustAccountStrkey(t, 3)
	bdFiller, t2User, t2Filler := mustAccountStrkey(t, 4), mustAccountStrkey(t, 5), mustAccountStrkey(t, 6)
	flasher := mustAccountStrkey(t, 7)
	assetX, assetY, cfgCancel, cfgTie := mustContractStrkey(t, 11), mustContractStrkey(t, 12), mustContractStrkey(t, 13), mustContractStrkey(t, 14)
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	n := uint32(0)
	ev := func(kind, who, asset, amount, amount2 string, attrs map[string]any) timescale.BLNTEvent {
		n++
		return timescale.BLNTEvent{
			ContractID: pool, Ledger: 65_600_000 + n, LedgerCloseTime: ts.Add(time.Duration(n) * time.Second),
			TxHash: pad64("b18", int(n)), ContractRole: string(blnt.RolePool), EventKind: kind, Pool: pool,
			UserAddress: who, Asset: asset, Amount: amount, Amount2: amount2, Attributes: attrs,
		}
	}
	fill := func(auctionType int, who, filler string, bid, lot map[string]any) timescale.BLNTEvent {
		return ev("fill_auction", who, "", "", "", map[string]any{
			"auction_type": auctionType, "filler": filler, "fill_percent": "100",
			"filled_auction_data": map[string]any{"bid": bid, "lot": lot, "block": 1},
		})
	}
	meta := func(cFactor int) map[string]any {
		return map[string]any{"metadata": map[string]any{"c_factor": cFactor}}
	}
	events := []timescale.BLNTEvent{
		ev("supply_collateral", clawed, assetX, "1000", "990", nil),
		ev("clawback", clawed, assetX, "300", "", map[string]any{"supply_burned": "7", "collateral_burned": "113"}),

		ev("borrow", debtor, assetX, "80", "75", nil),
		fill(0, debtor, liquidator, map[string]any{assetX: "30"}, map[string]any{}),
		ev("bad_debt", debtor, assetX, "17", "", nil),

		fill(1, blnt.MainnetBackstop, bdFiller, map[string]any{assetX: "23"}, map[string]any{assetY: "31"}),

		fill(2, t2User, t2Filler, map[string]any{assetX: "41"}, map[string]any{assetY: "43"}),
		fill(3, t2User, t2Filler, map[string]any{assetY: "47"}, map[string]any{assetX: "53"}),

		ev("flash_loan", flasher, assetY, "61", "59", nil),

		ev("queue_set_reserve", "", cfgCancel, "", "", meta(101)),
		ev("cancel_set_reserve", "", cfgCancel, "", "", nil),
		ev("queue_set_reserve", "", cfgCancel, "", "", meta(102)),
		ev("set_reserve", "", cfgCancel, "", "", map[string]any{"index": 3}),
	}
	// Two queue/set pairs in one ledger and close time, ordered only by op_index.
	tie := ev("queue_set_reserve", "", cfgTie, "", "", nil)
	at := func(kind string, op, idx uint32, attrs map[string]any) timescale.BLNTEvent {
		e := tie
		e.EventKind, e.OpIndex, e.EventIndex, e.Attributes = kind, op, idx, attrs
		return e
	}
	events = append(events,
		at("queue_set_reserve", 1, 0, meta(201)), at("set_reserve", 1, 1, map[string]any{"index": 4}),
		at("queue_set_reserve", 3, 0, meta(203)), at("set_reserve", 3, 1, map[string]any{"index": 4}))
	for _, e := range events {
		if err := store.InsertBLNTEvent(ctx, e); err != nil {
			t.Fatalf("InsertBLNTEvent %s: %v", e.EventKind, err)
		}
	}

	folds := func(who string) map[string]timescale.BlendPositionFold {
		t.Helper()
		got, err := store.BLNTPositionsByUser(ctx, who)
		if err != nil {
			t.Fatalf("BLNTPositionsByUser %s: %v", who, err)
		}
		out := map[string]timescale.BlendPositionFold{}
		for _, f := range got {
			out[f.Asset] = f
		}
		return out
	}

	if f := folds(clawed)[assetX]; f.SupplyNet != "700" || f.SupplyTokens != "870" || f.SupplySuperseded {
		t.Errorf("clawback fold = %+v, want supply net 700, tokens 990-7-113=870, not superseded", f)
	}
	// 75 d-tokens, 30 moved to the liquidator, 17 written off: 28 (45 without bad_debt, 62 with its sign flipped).
	if f := folds(debtor)[assetX]; f.BorrowTokens != "28" || !f.BorrowSuperseded {
		t.Errorf("bad-debt fold = %+v, want borrow tokens 28, superseded", f)
	}
	if got := folds(bdFiller); len(got) != 1 || got[assetX].BorrowTokens != "23" || !got[assetX].BorrowSuperseded || got[assetX].HasSupplyLeg {
		t.Errorf("type-1 filler folds = %+v, want only the 23 bid d-tokens and no lot supply", got)
	}
	if f := folds(blnt.MainnetBackstop)[assetX]; f.BorrowTokens != "-23" {
		t.Errorf("type-1 backstop fold = %+v, want borrow tokens -23", f)
	}
	for _, who := range []string{t2User, t2Filler} {
		if got := folds(who); len(got) != 0 {
			t.Errorf("type-2/3 fill folds for %s = %+v, want none", who, got)
		}
	}
	if f := folds(flasher)[assetY]; f.BorrowNet != "61" || f.BorrowTokens != "59" || f.BorrowSuperseded {
		t.Errorf("flash_loan fold = %+v, want borrow net 61, tokens 59", f)
	}

	cfgs, err := store.BLNTReserveConfigMetadata(ctx, pool)
	if err != nil {
		t.Fatalf("BLNTReserveConfigMetadata: %v", err)
	}
	for asset, want := range map[string]float64{cfgCancel: 102, cfgTie: 203} {
		var m map[string]float64
		if err := json.Unmarshal(cfgs[asset], &m); err != nil || m["c_factor"] != want {
			t.Errorf("config %s = %s (%v), want c_factor %v", asset, cfgs[asset], err, want)
		}
	}
	if len(cfgs) != 2 {
		t.Errorf("configs = %d assets, want 2", len(cfgs))
	}
}
