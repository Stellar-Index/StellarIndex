//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestBLNTBespoke_KPIsTablesAndExactAmounts builds /v1/protocols/blnt's bespoke block on real blnt_events rows: position, auction,
// backstop and emission KPIs from BLNT's own kinds, an emission sum above 2^63 kept exact, and backstop flows per (pool, tier).
func TestBLNTBespoke_KPIsTablesAndExactAmounts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if blk, err := store.BuildProtocolBespoke(ctx, "blnt", "lending", 90); err != nil || blk == nil {
		t.Fatalf("empty BuildProtocolBespoke blnt = %v, %v; want a count-only block", blk, err)
	}

	const (
		pool     = blnt.MainnetFixedPool
		backstop = "CBSTOPBLNTFIXTUREXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
		user     = "GDEA2WMOWYQ3T56A2PZH5QYYEQBAORW2AUUJYX4FFD5GDPESAICTFTAL"
		asset    = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	)
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	ev := func(i uint32, contract, role, kind, tier, assetID, amount, amount2 string, attrs map[string]any) timescale.BLNTEvent {
		return timescale.BLNTEvent{
			ContractID: contract, Ledger: 65_600_000 + i, LedgerCloseTime: ts.Add(time.Duration(i) * time.Second),
			TxHash: pad64("b1e", int(i)), ContractRole: role, EventKind: kind, Pool: pool, UserAddress: user,
			Asset: assetID, Tier: tier, Amount: amount, Amount2: amount2, Attributes: attrs,
		}
	}
	pr, br := string(blnt.RolePool), string(blnt.RoleBackstop)
	for _, e := range []timescale.BLNTEvent{
		ev(1, pool, pr, "supply", "", asset, "18446744073709551621", "18446744073709551000", nil), // 2^64 + 5
		ev(2, pool, pr, "borrow", "", asset, "40", "40", nil),
		ev(3, pool, pr, "flash_loan", "", asset, "7", "7", nil),
		ev(4, pool, pr, "new_auction", "", "", "", "", map[string]any{"auction_type": 0}),
		ev(5, pool, pr, "fill_auction", "", "", "", "", map[string]any{"auction_type": 0, "fill_percent": "100"}),
		ev(6, pool, pr, "claim", "", "", "9223372036854775808", "", nil), // 2^63
		ev(7, backstop, br, "claim", "FirstLoss", "", "2", "1", map[string]any{"shares": "1"}),
		ev(8, backstop, br, "deposit", "FirstLoss", "", "1000", "900", nil),
		ev(9, backstop, br, "withdraw", "FirstLoss", "", "300", "250", nil),
		ev(10, backstop, br, "deposit", "SecondLoss", "", "55", "50", nil),
		ev(11, pool, pr, "bad_debt", "", asset, "3", "", nil),
	} {
		if err := store.InsertBLNTEvent(ctx, e); err != nil {
			t.Fatalf("InsertBLNTEvent %s: %v", e.EventKind, err)
		}
	}

	blk, err := store.BuildProtocolBespoke(ctx, "blnt", "lending", 90)
	if err != nil || blk == nil {
		t.Fatalf("BuildProtocolBespoke blnt = %v, %v", blk, err)
	}
	kpis := kpiMap(blk)
	assertKPI(t, kpis, "Active users (90d)", "1")
	assertKPI(t, kpis, "Flash loans (90d)", "1")
	assertKPI(t, kpis, "Borrow-side events (90d)", "2")
	assertKPI(t, kpis, "All-time position events", "3")
	assertKPI(t, kpis, "Auctions started (90d)", "1")
	assertKPI(t, kpis, "Auctions filled (90d)", "1")
	assertKPI(t, kpis, "Backstop events (90d)", "4")
	assertKPI(t, kpis, "Emissions claimed (90d)", "9223372036854775810") // pool 2^63 + backstop 2, exact base units
	assertKPI(t, kpis, "Claim events (90d)", "2")
	assertKPI(t, kpis, "Credit-risk events (90d)", "1")
	if _, ok := kpis["Backstop volume (90d)"]; ok {
		t.Error("BLNT must not serve a cross-tier backstop volume: each tier holds a different asset")
	}

	tables := map[string]timescale.BespokeTable{}
	for _, tb := range blk.Tables {
		tables[tb.Title] = tb
	}
	nf := tables["Net flow by asset (90d)"]
	if len(nf.Rows) != 1 || nf.Rows[0][1] != "18446744073709551621" || nf.Rows[0][2] != "47" {
		t.Errorf("net flow rows = %v, want [asset 18446744073709551621 47 3]", nf.Rows)
	}
	bf := tables["Backstop flow by pool and tier (window)"]
	want := [][]string{{pool, "FirstLoss", "1000", "300", "1", "1"}, {pool, "SecondLoss", "55", "0", "1", "0"}}
	if len(bf.Rows) != 2 || bf.Rows[0][1] != want[0][1] || bf.Rows[0][2] != want[0][2] || bf.Rows[0][3] != want[0][3] ||
		bf.Rows[1][2] != want[1][2] || bf.Rows[1][3] != want[1][3] {
		t.Errorf("backstop flow rows = %v, want %v", bf.Rows, want)
	}
	ra := tables["Recent auctions"].Rows
	if len(ra) != 2 || ra[0][1] != "user-liquidation" || ra[0][2] != "fill" || ra[0][4] != "100" || ra[1][2] != "new" {
		t.Errorf("recent auctions rows = %v, want fill (100%%) then new, both user-liquidation", ra)
	}
}
