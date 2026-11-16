//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestLendingEarningsFolds_ExactSumsAndNoLeak inserts supply, partial withdraw, borrow, repay and claim events plus backstop
// flows for one account and a second account's events in the same pools, then checks the folds behind the earnings endpoint
// give exact sums (above 2^63) for the first account only.
func TestLendingEarningsFolds_ExactSumsAndNoLeak(t *testing.T) {
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
		pool   = "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD"
		asset  = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
		user   = "GDEA2WMOWYQ3T56A2PZH5QYYEQBAORW2AUUJYX4FFD5GDPESAICTFTAL"
		other  = "GAVG3ODZ4SAVK2WJL3F3RT265RL7P6QNOMA6NL3XDAREKSX3OWWMXF4R"
		bpool  = blnt.MainnetFixedPool
		bstop  = blnt.MainnetBackstop
		blendB = blend.MainnetBackstop
	)
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	n := func(s string) *big.Int {
		v, ok := new(big.Int).SetString(s, 10)
		if !ok {
			t.Fatalf("bad int %q", s)
		}
		return v
	}
	pos := func(i int, kind, who, tok, bd string) domain.BlendPositionEvent {
		return domain.BlendPositionEvent{
			Pool: pool, Kind: kind, Asset: asset, User: who, TokenAmount: n(tok), BOrDAmount: n(bd),
			Ledger: 60_000_000 + uint32(i), TxHash: pad64("e4a", i), Timestamp: ts.Add(time.Duration(i) * time.Second),
		}
	}
	for _, e := range []domain.BlendPositionEvent{
		pos(1, domain.BlendEventSupplyCollateral, user, "18446744073709551626", "18446744073709551600"), // 2^64 + 10
		pos(2, domain.BlendEventWithdrawCollateral, user, "26", "20"),
		pos(3, domain.BlendEventSupply, user, "100", "90"),
		pos(4, domain.BlendEventBorrow, user, "1000", "900"),
		pos(5, domain.BlendEventRepay, user, "400", "350"),
		pos(6, domain.BlendEventSupply, other, "777", "700"),
		pos(7, domain.BlendEventBorrow, other, "555", "500"),
	} {
		if err := store.InsertBlendPositionEvent(ctx, e); err != nil {
			t.Fatalf("InsertBlendPositionEvent %s: %v", e.Kind, err)
		}
	}
	for i, e := range []domain.BlendEmissionEvent{
		{Pool: pool, Kind: domain.BlendEventClaim, User: user, Amount: n("9223372036854775808")}, // 2^63
		{Pool: pool, Kind: domain.BlendEventClaim, User: user, Amount: n("1")},
		{Pool: pool, Kind: domain.BlendEventClaim, User: other, Amount: n("5")},
	} {
		e.Ledger, e.TxHash, e.EventIndex, e.Timestamp = 60_000_100+uint32(i), pad64("e4b", i), 0, ts.Add(time.Duration(i)*time.Second)
		if err := store.InsertBlendEmissionEvent(ctx, e); err != nil {
			t.Fatalf("InsertBlendEmissionEvent: %v", err)
		}
	}
	for i, e := range []timescale.BlendBackstopEvent{
		{EventType: timescale.BackstopDeposit, UserAddress: user, Amount: "1000", Amount2: "800"},
		{EventType: timescale.BackstopWithdraw, UserAddress: user, Amount: "300", Amount2: "200"},
		{EventType: timescale.BackstopDeposit, UserAddress: other, Amount: "50", Amount2: "40"},
	} {
		e.ContractID, e.Pool, e.Ledger, e.TxHash, e.ObservedAt = blendB, pool, 60_000_200+uint32(i), pad64("e4c", i), ts.Add(time.Duration(i)*time.Second)
		if err := store.InsertBlendBackstopEvent(ctx, e); err != nil {
			t.Fatalf("InsertBlendBackstopEvent: %v", err)
		}
	}
	for i, e := range []timescale.BLNTEvent{
		{ContractID: bpool, ContractRole: string(blnt.RolePool), EventKind: "claim", Pool: bpool, UserAddress: user, Amount: "70"},
		{ContractID: bstop, ContractRole: string(blnt.RoleBackstop), EventKind: "deposit", Pool: bpool, Tier: "FirstLoss", UserAddress: user, Amount: "500", Amount2: "400"},
		{ContractID: bstop, ContractRole: string(blnt.RoleBackstop), EventKind: "withdraw", Pool: bpool, Tier: "FirstLoss", UserAddress: user, Amount: "120", Amount2: "100"},
		{ContractID: bstop, ContractRole: string(blnt.RoleBackstop), EventKind: "deposit", Pool: bpool, Tier: "FirstLoss", UserAddress: other, Amount: "9", Amount2: "9"},
	} {
		e.Ledger, e.LedgerCloseTime, e.TxHash = 65_500_000+uint32(i), ts.Add(time.Duration(i)*time.Second), pad64("e4d", i)
		if err := store.InsertBLNTEvent(ctx, e); err != nil {
			t.Fatalf("InsertBLNTEvent %s: %v", e.EventKind, err)
		}
	}

	folds, err := store.BlendPositionsByUser(ctx, user)
	if err != nil {
		t.Fatalf("BlendPositionsByUser: %v", err)
	}
	if len(folds) != 1 {
		t.Fatalf("folds = %+v, want one (pool, asset)", folds)
	}
	f := folds[0]
	if f.SupplyNet != "18446744073709551700" || f.SupplyTokens != "18446744073709551670" || f.SupplySuperseded ||
		f.BorrowNet != "600" || f.BorrowTokens != "550" || f.BorrowSuperseded {
		t.Fatalf("fold = %+v", f)
	}

	flows, err := store.LendingAccountFlows(ctx, user)
	if err != nil {
		t.Fatalf("LendingAccountFlows: %v", err)
	}
	want := timescale.LendingAccountFlows{
		BlendClaims:   []timescale.LendingClaimFold{{Pool: pool, Claimed: "9223372036854775809"}},
		BLNTClaims:    []timescale.LendingClaimFold{{Pool: bpool, Claimed: "70"}},
		BlendBackstop: []timescale.LendingBackstopTokenFold{{Pool: pool, NetDeposited: "700"}},
		BLNTBackstop:  []timescale.LendingBackstopTokenFold{{Pool: bpool, Tier: "FirstLoss", NetDeposited: "380"}},
	}
	if !reflect.DeepEqual(flows, want) {
		t.Fatalf("flows = %+v\nwant    %+v", flows, want)
	}
}
