//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestClaimableSeed_RetractsServedClaim is GH #712 end to end: a claimable
// balance an earlier seed wrote as live, claimed while the live observer was
// not recording, must stop counting toward classic supply after a re-seed.
// Before the fix the seed could only add rows, so the served reader kept
// summing the claimed balance forever.
func TestClaimableSeed_RetractsServedClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	claimed, kept := cbsID(t, 0x61), cbsID(t, 0x62)
	created := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	claimedAt := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)

	// The served tier as an earlier (add-only) seed left it: both live.
	for _, id := range [][32]byte{claimed, kept} {
		if err := store.InsertClaimableObservation(ctx, timescale.ClaimableObservation{
			ClaimableID: cbsHex(id), AssetKey: cbsAssetKey, Ledger: 34_000_000, ObservedAt: created,
			Balance: big.NewInt(999), IntraLedgerSeq: timescale.SeedIntraLedgerSeq,
		}); err != nil {
			t.Fatalf("InsertClaimableObservation: %v", err)
		}
	}
	// A removed row for an id nobody serves as live must not be listed.
	if err := store.InsertClaimableObservation(ctx, timescale.ClaimableObservation{
		ClaimableID: cbsHex(cbsID(t, 0x63)), AssetKey: cbsAssetKey, Ledger: 35_000_000, ObservedAt: created,
		Balance: big.NewInt(0), IsRemoval: true,
	}); err != nil {
		t.Fatalf("InsertClaimableObservation (removal): %v", err)
	}

	served, err := store.LiveClaimableObservations(ctx)
	if err != nil {
		t.Fatalf("LiveClaimableObservations: %v", err)
	}
	if len(served) != 2 || served[cbsHex(claimed)] != (timescale.LiveClaimable{AssetKey: cbsAssetKey, Ledger: 34_000_000}) {
		t.Fatalf("served live set = %+v, want exactly the two live balances at 34000000", served)
	}

	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: 34_000_000, CloseTime: created, TxHash: "cbr61a", IntraLedgerSeq: 1,
			ChangeType: "created", EntryType: "claimable_balance", KeyXDR: cbsKeyXDR(t, claimed),
			EntryXDR: cbsEntryXDR(t, claimed, cbsAsset(t, "AQUA"), 999, 34_000_000),
		},
		{
			LedgerSeq: 48_000_000, CloseTime: claimedAt, TxHash: "cbr61b", IntraLedgerSeq: 1,
			ChangeType: "removed", EntryType: "claimable_balance", KeyXDR: cbsKeyXDR(t, claimed),
		},
		{
			LedgerSeq: 34_000_000, CloseTime: created, TxHash: "cbr62a", IntraLedgerSeq: 1,
			ChangeType: "created", EntryType: "claimable_balance", KeyXDR: cbsKeyXDR(t, kept),
			EntryXDR: cbsEntryXDR(t, kept, cbsAsset(t, "AQUA"), 999, 34_000_000),
		},
	}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	servedKeys := map[string]string{}
	for id, row := range served {
		servedKeys[id] = row.AssetKey
	}
	var rows []timescale.ClaimableObservation
	if _, err := chstore.StreamClaimableBalanceSeeds(ctx, addr, nil, servedKeys, chstore.SeedWalk{}, func(s chstore.ClaimableBalanceSeed) error {
		if _, ours := served[s.ClaimableID]; ours {
			rows = append(rows, timescale.ClaimableObservation{
				ClaimableID: s.ClaimableID, AssetKey: s.AssetKey, Ledger: s.LedgerSeq, ObservedAt: s.CloseTime,
				Balance: s.Balance, IsRemoval: s.IsRemoval, IntraLedgerSeq: timescale.SeedIntraLedgerSeq,
			})
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamClaimableBalanceSeeds: %v", err)
	}
	if err := store.InsertClaimableObservationBatch(ctx, rows); err != nil {
		t.Fatalf("InsertClaimableObservationBatch: %v", err)
	}

	sum, err := store.SumClaimableBalancesAtOrBefore(ctx, cbsAssetKey, 50_000_000)
	if err != nil {
		t.Fatalf("SumClaimableBalancesAtOrBefore: %v", err)
	}
	if sum.Cmp(big.NewInt(999)) != 0 {
		t.Errorf("served claimable sum after re-seed = %s, want 999 (the claimed balance retracted, the live one kept)", sum)
	}
	after, err := store.LiveClaimableObservations(ctx)
	if err != nil {
		t.Fatalf("LiveClaimableObservations (after): %v", err)
	}
	if _, still := after[cbsHex(claimed)]; still || len(after) != 1 {
		t.Errorf("served live set after re-seed = %+v, want only the unclaimed balance", after)
	}
}

// TestClaimableSeedProvenanceRoundTrip executes migration 0183's table through
// the store: an absent asset reads ok=false, an upsert round-trips every
// column, a second upsert overwrites, and an unverified pass is refused.
func TestClaimableSeedProvenanceRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, ok, err := store.ClaimableSeedProvenanceFor(ctx, cbsAssetKey); err != nil || ok {
		t.Fatalf("never stamped: ok=%v err=%v, want ok=false", ok, err)
	}
	if err := store.UpsertClaimableSeedProvenance(ctx, timescale.ClaimableSeedProvenance{AssetKey: cbsAssetKey, ClaimablesSeeded: 1}); err == nil {
		t.Fatal("a row without LakeVerifiedThrough was stamped")
	}

	minL, maxL := uint32(33_000_000), uint32(63_000_000)
	first := timescale.ClaimableSeedProvenance{
		AssetKey: cbsAssetKey, ClaimablesSeeded: 1200, ClaimablesRetracted: 7,
		MinLedgerSeen: &minL, MaxLedgerSeen: &maxL, LakeVerifiedThrough: 63_100_000,
	}
	if err := store.UpsertClaimableSeedProvenance(ctx, first); err != nil {
		t.Fatalf("UpsertClaimableSeedProvenance: %v", err)
	}
	got, ok, err := store.ClaimableSeedProvenanceFor(ctx, cbsAssetKey)
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if got.ClaimablesSeeded != 1200 || got.ClaimablesRetracted != 7 || got.LakeVerifiedThrough != 63_100_000 ||
		got.MinLedgerSeen == nil || *got.MinLedgerSeen != minL || got.MaxLedgerSeen == nil || *got.MaxLedgerSeen != maxL || got.SeededAt.IsZero() {
		t.Errorf("read back %+v, want %+v", got, first)
	}

	second := timescale.ClaimableSeedProvenance{AssetKey: cbsAssetKey, ClaimablesRetracted: 3, LakeVerifiedThrough: 64_000_000}
	if err := store.UpsertClaimableSeedProvenance(ctx, second); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, _, err = store.ClaimableSeedProvenanceFor(ctx, cbsAssetKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClaimablesSeeded != 0 || got.ClaimablesRetracted != 3 || got.LakeVerifiedThrough != 64_000_000 || got.MinLedgerSeen != nil || got.MaxLedgerSeen != nil {
		t.Errorf("after overwrite %+v, want %+v with nil ledger bounds", got, second)
	}
}
