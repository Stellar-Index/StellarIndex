//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestLendingEmissionClaims_WindowAndExactSums pins the claim totals: NUMERIC sums above 2^63 come back exact, the 30-day
// window is (asOf - 30d, asOf], events after asOf are excluded, and another pool's claims never leak in.
func TestLendingEmissionClaims_WindowAndExactSums(t *testing.T) {
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
		blendPool = "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD"
		other     = "CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP"
		userA     = "GDEA2WMOWYQ3T56A2PZH5QYYEQBAORW2AUUJYX4FFD5GDPESAICTFTAL"
		userB     = "GBZXN7PIRZGNMHGA7MUUUF4GWPY5AYPV6LY4UV2GL6VJGIQRXFDNMADI"
		big9e18   = "9000000000000000000" // > 2^63 once two are summed
	)
	asOf := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	edge := asOf.Add(-30 * 24 * time.Hour)
	amt := func(s string) *big.Int { n, _ := new(big.Int).SetString(s, 10); return n }

	blendRows := []struct {
		pool, user string
		ts         time.Time
		amount     string
	}{
		{blendPool, userA, edge, big9e18},                  // on the boundary: lifetime only
		{blendPool, userA, edge.Add(time.Second), big9e18}, // first second inside the window
		{blendPool, userB, asOf, "5"},                      // at asOf: inside
		{blendPool, userB, asOf.Add(time.Second), "7"},     // after asOf: excluded
		{other, userA, asOf.Add(-time.Hour), "11"},         // another pool
	}
	for i, r := range blendRows {
		if err := store.InsertBlendEmissionEvent(ctx, domain.BlendEmissionEvent{
			Pool: r.pool, Kind: domain.BlendEventClaim, User: r.user, Amount: amt(r.amount),
			Ledger: uint32(60_000_000 + i), TxHash: fmt.Sprintf("%064x", i+1), Timestamp: r.ts,
		}); err != nil {
			t.Fatalf("InsertBlendEmissionEvent %d: %v", i, err)
		}
	}
	got, err := store.LendingEmissionClaims(ctx, blendPool, false, asOf)
	if err != nil {
		t.Fatal(err)
	}
	p := got.Pool
	if p.Claims != 3 || p.Claims30d != 2 || p.Claimers != 2 || p.Claimers30d != 2 ||
		p.Amount.String() != "18000000000000000005" || p.Amount30d.String() != "9000000000000000005" || got.Backstop != nil {
		t.Fatalf("blend totals = %+v", got)
	}

	blntRows := []timescale.BLNTEvent{
		{
			ContractID: blnt.MainnetFixedPool, ContractRole: string(blnt.RolePool), EventKind: "claim", UserAddress: userA,
			LedgerCloseTime: asOf.Add(-time.Hour), Amount: big9e18,
		},
		{
			ContractID: blnt.MainnetFixedPool, ContractRole: string(blnt.RolePool), EventKind: "claim", UserAddress: userA,
			LedgerCloseTime: edge.Add(-time.Hour), Amount: big9e18,
		},
		{
			ContractID: blnt.MainnetBackstop, ContractRole: string(blnt.RoleBackstop), EventKind: "claim", UserAddress: userB,
			Pool: blnt.MainnetFixedPool, Tier: "FirstLoss", LedgerCloseTime: asOf.Add(-time.Hour), Amount: big9e18, Amount2: "3",
			Attributes: map[string]any{"shares": big9e18},
		},
		{
			ContractID: blnt.MainnetBackstop, ContractRole: string(blnt.RoleBackstop), EventKind: "claim", UserAddress: userB,
			Pool: blnt.MainnetFixedPool, Tier: "FirstLoss", LedgerCloseTime: edge, Amount: big9e18, Amount2: "4",
			Attributes: map[string]any{"shares": big9e18},
		},
		{
			ContractID: blnt.MainnetBackstop, ContractRole: string(blnt.RoleBackstop), EventKind: "claim", UserAddress: userA,
			Pool: other, Tier: "FirstLoss", LedgerCloseTime: asOf.Add(-time.Hour), Amount: "1", Amount2: "1",
			Attributes: map[string]any{"shares": "1"},
		},
	}
	for i, r := range blntRows {
		r.Ledger, r.TxHash, r.EventIndex = uint32(61_000_000+i), fmt.Sprintf("%064x", 100+i), uint32(i)
		if err := store.InsertBLNTEvent(ctx, r); err != nil {
			t.Fatalf("InsertBLNTEvent %d: %v", i, err)
		}
	}
	got, err = store.LendingEmissionClaims(ctx, blnt.MainnetFixedPool, true, asOf)
	if err != nil {
		t.Fatal(err)
	}
	if p := got.Pool; p.Claims != 2 || p.Claims30d != 1 || p.Claimers != 1 ||
		p.Amount.String() != "18000000000000000000" || p.Amount30d.String() != big9e18 {
		t.Fatalf("blnt pool totals = %+v", p)
	}
	if len(got.Backstop) != 1 {
		t.Fatalf("blnt backstop tiers = %+v", got.Backstop)
	}
	b := got.Backstop[0]
	if b.Tier != "FirstLoss" || b.Claims != 2 || b.Claims30d != 1 || b.Amount.String() != "18000000000000000000" ||
		b.Amount30d.String() != big9e18 || b.LP.String() != "7" || b.LP30d.String() != "3" ||
		b.Shares.String() != "18000000000000000000" || b.Shares30d.String() != big9e18 {
		t.Fatalf("blnt backstop totals = %+v", b)
	}
}
