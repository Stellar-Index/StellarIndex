//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/sources/blend"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

const (
	blendEmPool  = "CALI2BYU2JE6WVRUFYTS6MSBNEHGJ35P4AVCZYF3B6QOE3QKOB2PLE6M"
	blendEmAsset = "CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG"
	blendEmUser  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

// TestBlendEmissionWindowStatsAndBespoke exercises the Blend emission /
// credit-risk READ side (BlendEmissionWindowStats) + its surfacing on the
// blend lending bespoke block. Proves empty-safe, i128/NUMERIC preservation
// of claimed-emission volume (never int64), the bad_debt+defaulted_debt
// credit-risk tally, and that the emission KPIs appear alongside (not
// replacing) the existing Blend position/auction/backstop KPIs.
func TestBlendEmissionWindowStatsAndBespoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if em, err := store.BlendEmissionWindowStats(ctx, 90); err != nil {
		t.Fatalf("BlendEmissionWindowStats (empty): %v", err)
	} else if em != nil {
		t.Fatalf("BlendEmissionWindowStats (empty) = %+v, want nil", em)
	}

	t0 := time.Now().UTC().Add(-10 * time.Hour)
	claimHuge, _ := new(big.Int).SetString("44444444444444444444", 10) // > 2^63

	rows := []blend.EmissionEvent{
		{Pool: blendEmPool, Kind: blend.EventClaim, User: blendEmUser, Amount: claimHuge, ReserveTokenIDs: []uint32{0, 3}, Ledger: 61_000_000, TxHash: pad64("h", 0), OpIndex: 0, EventIndex: 0, Timestamp: t0},
		{Pool: blendEmPool, Kind: blend.EventGulp, Asset: blendEmAsset, Amount: big.NewInt(1_000), Ledger: 61_000_001, TxHash: pad64("h", 1), OpIndex: 0, EventIndex: 0, Timestamp: t0.Add(time.Minute)},
		{Pool: blendEmPool, Kind: blend.EventBadDebt, User: blendEmUser, Asset: blendEmAsset, Amount: big.NewInt(500), Ledger: 61_000_002, TxHash: pad64("h", 2), OpIndex: 0, EventIndex: 0, Timestamp: t0.Add(2 * time.Minute)},
		{Pool: blendEmPool, Kind: blend.EventDefaultedDebt, Asset: blendEmAsset, Amount: big.NewInt(250), Ledger: 61_000_003, TxHash: pad64("h", 3), OpIndex: 0, EventIndex: 0, Timestamp: t0.Add(3 * time.Minute)},
	}
	for _, e := range rows {
		if err := store.InsertBlendEmissionEvent(ctx, e); err != nil {
			t.Fatalf("InsertBlendEmissionEvent %s: %v", e.Kind, err)
		}
	}

	em, err := store.BlendEmissionWindowStats(ctx, 90)
	if err != nil {
		t.Fatalf("BlendEmissionWindowStats: %v", err)
	}
	if em == nil {
		t.Fatal("BlendEmissionWindowStats = nil, want summary")
	}
	if em.Claims != 1 {
		t.Errorf("Claims = %d, want 1", em.Claims)
	}
	if em.ClaimVolume.BigInt().Cmp(claimHuge) != 0 {
		t.Errorf("ClaimVolume = %s, want %s — i128/NUMERIC lost precision", em.ClaimVolume, claimHuge)
	}
	if em.Gulps != 1 {
		t.Errorf("Gulps = %d, want 1", em.Gulps)
	}
	if em.CreditRisk != 2 {
		t.Errorf("CreditRisk = %d, want 2 (bad_debt + defaulted_debt)", em.CreditRisk)
	}
	if em.TotalEvents != 4 {
		t.Errorf("TotalEvents = %d, want 4", em.TotalEvents)
	}

	blk, err := store.BuildProtocolBespoke(ctx, "blend", "lending", 90)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke blend: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke blend = nil, want a block")
	}
	kpis := kpiMap(blk)
	assertKPI(t, kpis, "Emissions claimed (90d)", claimHuge.String())
	assertKPI(t, kpis, "Credit-risk events (90d)", "2")
	// The Blend position KPIs must still be present (emissions ADD, never replace).
	if _, ok := kpis["Net supplied (90d)"]; !ok {
		t.Errorf("blend block lost its existing 'Net supplied' KPI after adding emissions; KPIs=%+v", blk.KPIs)
	}
}
