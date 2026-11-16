//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func openLendingStore(t *testing.T, ctx context.Context) *timescale.Store {
	t.Helper()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestLendingAuctions_PairsFillsAndDeletesToTheirOwnAuction — BLNT: new → partial fill → full fill, then a second auction
// for the same user and type that is deleted (its delete must not land on the first, nor the first's fills on it), and a
// bad-debt auction of the same user kept apart by type. Pagination by cursor resumes after the last served auction.
func TestLendingAuctions_PairsFillsAndDeletesToTheirOwnAuction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store := openLendingStore(t, ctx)

	const pool = blnt.MainnetFixedPool
	user, filler := mustAccountStrkey(t, 31), mustAccountStrkey(t, 32)
	asset := mustContractStrkey(t, 33)
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	ev := func(i uint32, kind string, attrs map[string]any) timescale.BLNTEvent {
		return timescale.BLNTEvent{
			ContractID: pool, Ledger: 65_800_000 + i, LedgerCloseTime: ts.Add(time.Duration(i) * time.Second),
			TxHash: pad64("a1", int(i)), ContractRole: string(blnt.RolePool), EventKind: kind, Pool: pool, UserAddress: user, Attributes: attrs,
		}
	}
	newAuction := func(typ int, lot string) map[string]any {
		return map[string]any{"auction_type": typ, "percent": 60, "auction_data": map[string]any{
			"bid": map[string]any{asset: "18446744073709551621"}, "lot": map[string]any{asset: lot}, "block": 65_800_001,
		}}
	}
	fill := func(pct string) map[string]any {
		return map[string]any{"auction_type": 0, "filler": filler, "fill_percent": pct}
	}
	for _, e := range []timescale.BLNTEvent{
		ev(1, "new_auction", newAuction(0, "7")),
		ev(2, "fill_auction", fill("40")),
		ev(3, "new_auction", newAuction(1, "9")),
		ev(4, "fill_auction", fill("100")),
		ev(5, "new_auction", newAuction(0, "11")),
		ev(6, "delete_auction", map[string]any{"auction_type": 0}),
	} {
		if err := store.InsertBLNTEvent(ctx, e); err != nil {
			t.Fatalf("InsertBLNTEvent %s: %v", e.EventKind, err)
		}
	}

	got, truncated, err := store.LendingAuctions(ctx, pool, 10, timescale.LendingConfigCursor{})
	if err != nil || truncated {
		t.Fatalf("LendingAuctions: %v truncated=%v", err, truncated)
	}
	if len(got) != 3 {
		t.Fatalf("auctions = %+v, want 3", got)
	}
	second, badDebt, first := got[0], got[1], got[2]
	if second.Ledger != 65_800_005 || second.Deleted == nil || second.Deleted.Ledger != 65_800_006 || len(second.Fills) != 0 || second.Lot[asset] != "11" {
		t.Errorf("second auction = %+v", second)
	}
	if badDebt.AuctionType != 1 || len(badDebt.Fills) != 0 || badDebt.Deleted != nil {
		t.Errorf("bad-debt auction = %+v, want no fills (they are type 0)", badDebt)
	}
	if first.Deleted != nil || len(first.Fills) != 2 || first.Fills[0].FillPercent != "40" || first.Fills[1].FillPercent != "100" ||
		first.Fills[1].Filler != filler || first.Bid[asset] != "18446744073709551621" || first.Percent == nil || *first.Percent != 60 ||
		first.Block == nil || *first.Block != 65_800_001 {
		t.Errorf("first auction = %+v", first)
	}

	page, _, err := store.LendingAuctions(ctx, pool, 1, timescale.LendingConfigCursor{
		Ts: badDebt.Ts, Ledger: badDebt.Ledger, TxHash: badDebt.TxHash, OpIndex: badDebt.OpIndex, EventIndex: badDebt.EventIndex,
	})
	if err != nil || len(page) != 1 || page[0].Ledger != first.Ledger || len(page[0].Fills) != 2 {
		t.Fatalf("page after cursor = %+v (%v)", page, err)
	}
}

// TestLendingAuctions_BlendTable — a Blend pool's auctions come from blend_auctions with the stored bid/lot arrays.
func TestLendingAuctions_BlendTable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store := openLendingStore(t, ctx)

	pool, user, filler := mustContractStrkey(t, 41), mustAccountStrkey(t, 42), mustAccountStrkey(t, 43)
	asset, err := canonical.NewSorobanAsset(mustContractStrkey(t, 44))
	if err != nil {
		t.Fatal(err)
	}
	big64, _ := new(big.Int).SetString("18446744073709551621", 10)
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	data := blend.AuctionData{Bid: []blend.AssetAmount{{Asset: asset, Amount: big64}}, Lot: []blend.AssetAmount{{Asset: asset, Amount: big.NewInt(3)}}, Block: 100}
	if err := store.InsertBlendNewAuction(ctx, blend.NewAuctionEvent{
		Pool: pool, AuctionType: 0, User: user, Percent: 50, Data: data,
		Ledger: 100, TxHash: pad64("b1", 1), Timestamp: ts,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertBlendFillAuction(ctx, blend.FillAuctionEvent{
		Pool: pool, AuctionType: 0, User: user, Filler: filler,
		FillPercent: big.NewInt(100), Data: &data, Ledger: 101, TxHash: pad64("b1", 2), Timestamp: ts.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.LendingAuctions(ctx, pool, 10, timescale.LendingConfigCursor{})
	if err != nil || len(got) != 1 {
		t.Fatalf("auctions = %+v (%v)", got, err)
	}
	a := got[0]
	if a.Bid[asset.String()] != "18446744073709551621" || a.Lot[asset.String()] != "3" || len(a.Fills) != 1 ||
		a.Fills[0].Filler != filler || a.Fills[0].FillPercent != "100" || *a.Percent != 50 {
		t.Fatalf("auction = %+v", a)
	}
}

// TestBLNTBackstopByUser_ReplaysTheWithdrawalQueue — deposit, queue, dequeue (newest first), queue, withdraw (oldest first)
// across two tiers with asymmetric amounts, so a flipped sign or queue end changes the result.
func TestBLNTBackstopByUser_ReplaysTheWithdrawalQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store := openLendingStore(t, ctx)

	const pool = blnt.MainnetFixedPool
	user := mustAccountStrkey(t, 51)
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	n := 0
	ev := func(tier, kind, amount, amount2 string, attrs map[string]any) timescale.BLNTEvent {
		n++
		return timescale.BLNTEvent{
			ContractID: blnt.MainnetBackstop, Ledger: 65_900_000 + uint32(n), LedgerCloseTime: ts.Add(time.Duration(n) * time.Second),
			TxHash: pad64("c1", n), ContractRole: string(blnt.RoleBackstop), EventKind: kind, Pool: pool, UserAddress: user,
			Tier: tier, Amount: amount, Amount2: amount2, Attributes: attrs,
		}
	}
	exp := func(sec int) map[string]any { return map[string]any{"expiration": time.Unix(int64(sec), 0).Unix()} }
	for _, e := range []timescale.BLNTEvent{
		ev("SecondLoss", "deposit", "500", "18446744073709551621", nil), // 2^64 + 5 shares
		ev("SecondLoss", "queue_withdrawal", "", "100", exp(1_000)),
		ev("SecondLoss", "queue_withdrawal", "", "30", exp(2_000)),
		ev("SecondLoss", "dequeue_withdrawal", "", "20", nil), // takes from the newest: 30 → 10
		ev("SecondLoss", "queue_withdrawal", "", "7", exp(3_000)),
		ev("SecondLoss", "withdraw", "60", "104", nil), // burns the oldest 100, then 4 of the 10
		ev("FirstLoss", "deposit", "9", "13", nil),
		ev("FirstLoss", "claim", "", "", map[string]any{"shares": "2"}),
		ev("FirstLoss", "draw", "5", "", nil), // tokens only; never read
	} {
		if err := store.InsertBLNTEvent(ctx, e); err != nil {
			t.Fatalf("InsertBLNTEvent %s: %v", e.EventKind, err)
		}
	}
	folds, err := store.BLNTBackstopByUser(ctx, user)
	if err != nil {
		t.Fatalf("BLNTBackstopByUser: %v", err)
	}
	if len(folds) != 2 || folds[0].Tier != "FirstLoss" || folds[1].Tier != "SecondLoss" {
		t.Fatalf("folds = %+v", folds)
	}
	if f := folds[0]; f.Shares != "15" || len(f.Queued) != 0 {
		t.Errorf("FirstLoss = %+v, want 15 shares, no queue", f)
	}
	f := folds[1]
	if f.Shares != "18446744073709551517" || len(f.Queued) != 2 || f.Queued[0].Shares != "6" || f.Queued[0].Expiration.Unix() != 2_000 ||
		f.Queued[1].Shares != "7" || f.Queued[1].Expiration.Unix() != 3_000 {
		t.Errorf("SecondLoss = %+v, want 2^64+5-104 shares with queue [6@2000, 7@3000]", f)
	}
}

// TestLendingAuctions_SameLedgerDeleteAfterAClosedAuction — an auction filled to 100%, then a new auction and its delete in one
// ledger: the delete is the new auction's (the earlier one is already closed), never a false deletion of the earlier one.
func TestLendingAuctions_SameLedgerDeleteAfterAClosedAuction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store := openLendingStore(t, ctx)

	const pool = blnt.MainnetFixedPool
	user := mustAccountStrkey(t, 61)
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	ev := func(ledger uint32, tx, kind string, attrs map[string]any) timescale.BLNTEvent {
		return timescale.BLNTEvent{
			ContractID: pool, Ledger: ledger, LedgerCloseTime: ts.Add(time.Duration(ledger-66_000_000) * time.Second),
			TxHash: strings.Repeat(tx, 64), ContractRole: string(blnt.RolePool), EventKind: kind, Pool: pool, UserAddress: user, Attributes: attrs,
		}
	}
	newAuction := map[string]any{"auction_type": 0, "percent": 50, "auction_data": map[string]any{"bid": map[string]any{}, "lot": map[string]any{}, "block": 1}}
	for _, e := range []timescale.BLNTEvent{
		ev(66_000_001, "1", "new_auction", newAuction),
		ev(66_000_002, "2", "fill_auction", map[string]any{"auction_type": 0, "filler": user, "fill_percent": "100"}),
		ev(66_000_003, "f", "new_auction", newAuction),
		ev(66_000_003, "0", "delete_auction", map[string]any{"auction_type": 0}),
	} {
		if err := store.InsertBLNTEvent(ctx, e); err != nil {
			t.Fatalf("InsertBLNTEvent %s: %v", e.EventKind, err)
		}
	}
	got, _, err := store.LendingAuctions(ctx, pool, 10, timescale.LendingConfigCursor{})
	if err != nil || len(got) != 2 {
		t.Fatalf("auctions = %+v (%v)", got, err)
	}
	if got[0].Deleted == nil || got[0].Deleted.Ledger != 66_000_003 || got[1].Deleted != nil || len(got[1].Fills) != 1 {
		t.Fatalf("new = %+v earlier = %+v, want the delete on the new auction", got[0], got[1])
	}
}

// TestLendingAuctions_DeepPageReadStopsAtTheNextAuction — a BLNT interest auction's user is the backstop, so every later auction
// shares its key; an old page reads only up to that key's next auction and is not degraded by thousands of later events.
func TestLendingAuctions_DeepPageReadStopsAtTheNextAuction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	store := openLendingStore(t, ctx)

	const pool, later = blnt.MainnetFixedPool, 5_000
	ts := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	ev := func(i int, kind string, attrs map[string]any) timescale.BLNTEvent {
		return timescale.BLNTEvent{
			ContractID: pool, Ledger: 66_100_000 + uint32(i), LedgerCloseTime: ts.Add(time.Duration(i) * time.Second), //nolint:gosec // small test index
			TxHash: pad64("d1", i), ContractRole: string(blnt.RolePool), EventKind: kind, Pool: pool, UserAddress: blnt.MainnetBackstop, Attributes: attrs,
		}
	}
	newAuction := map[string]any{"auction_type": 2, "auction_data": map[string]any{"bid": map[string]any{}, "lot": map[string]any{}, "block": 1}}
	events := []timescale.BLNTEvent{ev(1, "new_auction", newAuction), ev(2, "fill_auction", map[string]any{"auction_type": 2, "filler": blnt.MainnetBackstop, "fill_percent": "100"})}
	for i := range later {
		events = append(events, ev(3+i, "new_auction", newAuction))
	}
	for _, e := range events {
		if err := store.InsertBLNTEvent(ctx, e); err != nil {
			t.Fatalf("InsertBLNTEvent: %v", err)
		}
	}
	next := events[2]
	got, truncated, err := store.LendingAuctions(ctx, pool, 1, timescale.LendingConfigCursor{
		Ts: next.LedgerCloseTime, Ledger: next.Ledger, TxHash: next.TxHash,
	})
	if err != nil || truncated || len(got) != 1 || got[0].Ledger != events[0].Ledger || len(got[0].Fills) != 1 {
		t.Fatalf("deep page = %+v truncated=%v (%v)", got, truncated, err)
	}
}

// TestBLNTBackstopByUser_SameLedgerQueueThenDequeue — a queue and its dequeue execute in that order in two transactions of one
// ledger whose hashes sort the dequeue first: the position is not poisoned, the totals are exact and the queue's expirations are
// marked unknown.
func TestBLNTBackstopByUser_SameLedgerQueueThenDequeue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store := openLendingStore(t, ctx)

	const pool = blnt.MainnetFixedPool
	user := mustAccountStrkey(t, 62)
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	ev := func(ledger uint32, tx, kind, amount2 string, attrs map[string]any) timescale.BLNTEvent {
		return timescale.BLNTEvent{
			ContractID: blnt.MainnetBackstop, Ledger: ledger, LedgerCloseTime: ts.Add(time.Duration(ledger-66_200_000) * time.Second),
			TxHash: strings.Repeat(tx, 64), ContractRole: string(blnt.RoleBackstop), EventKind: kind, Pool: pool, UserAddress: user,
			Tier: "FirstLoss", Amount2: amount2, Attributes: attrs,
		}
	}
	for _, e := range []timescale.BLNTEvent{
		ev(66_200_001, "1", "deposit", "18446744073709551621", nil),
		ev(66_200_002, "f", "queue_withdrawal", "100", map[string]any{"expiration": 5_000}), // executed first
		ev(66_200_002, "0", "dequeue_withdrawal", "60", nil),
	} {
		if err := store.InsertBLNTEvent(ctx, e); err != nil {
			t.Fatalf("InsertBLNTEvent %s: %v", e.EventKind, err)
		}
	}
	folds, err := store.BLNTBackstopByUser(ctx, user)
	if err != nil || len(folds) != 1 {
		t.Fatalf("folds = %+v (%v)", folds, err)
	}
	if f := folds[0]; f.Shares != "18446744073709551621" || !f.ExpirationsUnknown || f.QueuedShares != "40" || f.Queued != nil {
		t.Fatalf("fold = %+v, want 2^64+5 shares, 40 queued, expirations unknown", f)
	}
}
