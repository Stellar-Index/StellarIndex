//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// TestClaimableSeed_RetractsServedClaim is the end-to-end proof: a claimable
// balance an earlier seed wrote as live, claimed while the live observer was
// not recording, must stop counting toward classic supply after a re-seed.
// A seed that could only add rows would leave the served reader
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

// TestClaimableSeedProvenanceRoundTrip executes migration 0184's table through
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

// TestSupplyCrossCheckConvergesAfterPoolBalanceRecovery is the
// acceptance test for the BLND/EURC/KALE/PHO supply_cross_check_divergence
// residual ("PHO/BLND VERDICT" incident). It
// can't run against live r1 data from here, so it reproduces the
// documented divergence SHAPE with synthetic rows through the REAL
// Algorithm-2 pipeline (StorageClassicSupplyReader.ClassicSupplyAt →
// ClassicComputer.Compute, exactly what the aggregator's per-asset
// Refresher runs) against a real TimescaleDB:
//
//  1. Insert only the classic-side components a classic-only system would
//     have observed (trustlines + a small "already-visible" SAC
//     balance) — Algorithm 2's total under-counts, exactly like the
//     documented incident.
//  2. Cross-check that under-count against a fixed Algorithm-3 total
//     (the SAC's verified-correct lifetime supply — for PHO/BLND these
//     are the incident's real on-chain-verified figures; EURC/KALE use
//     representative round numbers reproducing the same shape) via
//     supply.CrossCheckForClass(..., WrapClassPartial) — the exact
//     function CrossCheckRefresher uses. Assert it reports `over`
//     (WithinTolerance=false), reproducing the alert firing.
//  3. Insert the "recovered" pool-held SAC balance — the row
//     `supply seed-sac-balances -full-history` would have written via
//     clickhouse.StreamSACBalanceSeedsFullHistory + Store
//     .InsertSACBalanceObservation (test/integration/
//     clickhouse_entries_test.go covers that extraction step
//     against ClickHouse; this test covers what happens to Algorithm 2
//     once the row lands in Postgres, which is the same table either
//     seed source writes to — sac_balance_observations. No new
//     aggregation code was needed: SumSACBalancesAtOrBefore already
//     sums every holder regardless of type).
//  4. Re-run the cross-check. Assert it now converges
//     (WithinTolerance=true) — the acceptance criterion.
func TestSupplyCrossCheckConvergesAfterPoolBalanceRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	reader := supply.NewStorageClassicSupplyReader(store, supply.ClassicSupplyReaderOptions{})
	computer, err := supply.NewClassicComputer(supply.Policy{}, reader)
	if err != nil {
		t.Fatalf("NewClassicComputer: %v", err)
	}

	const asOfLedger = 70_000_000
	observedAt := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		// asset is the classic side (Algorithm 2).
		asset canonical.Asset
		// sacContract is the SAC wrapper's contract id (any structurally
		// distinct string — sac_balance_observations keys on it as an
		// opaque string, no strkey validation at this layer).
		sacContract string
		// classicHolder is an ordinary trustline holder representing the
		// classic-only "visible" classic supply.
		classicHolder string
		classicAmount int64
		// poolHolder is the dormant Phoenix/Blend pool contract's
		// address — the holder the -full-history seed recovers.
		poolHolder string
		poolAmount string // decimal string; some of these exceed int64
		// sacTotal is Algorithm 3's verified lifetime SAC supply — fixed,
		// not affected by anything this test inserts. PHO/BLND use the
		// incident's real on-chain-verified figures (docs/architecture/
		// supply-pipeline.md); EURC/KALE use representative round
		// numbers reproducing the same "sac_total > classic_total"
		// shape documented for the residual four.
		sacTotal string
	}{
		{
			name:          "PHO",
			asset:         canonical.Asset{Type: canonical.AssetClassic, Code: "PHO", Issuer: "GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"},
			sacContract:   "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32",
			classicHolder: "GHOLDER_PHO_1",
			classicAmount: 200_000_000_000_000, // representative classic-only Alg-2 reading — well under sacTotal
			poolHolder:    "CPOOL_PHO_PHOENIX_1",
			poolAmount:    "1900000000000000", // recovers the dormant pool balance; classic+pool > sacTotal
			sacTotal:      "1999999993050277", // PHO lifetime SAC supply (exact real figure)
		},
		{
			name:          "BLND",
			asset:         canonical.Asset{Type: canonical.AssetClassic, Code: "BLND", Issuer: "GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"},
			sacContract:   "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY",
			classicHolder: "GHOLDER_BLND_1",
			classicAmount: 1_100_000_000_000_000, // representative classic-only reading — under sacTotal by ~12%, matching the incident's ~12.4%-under BLND finding
			poolHolder:    "CPOOL_BLND_BACKSTOP_1",
			poolAmount:    "200000000000000",
			sacTotal:      "1236670485295609", // BLND lifetime SAC supply (exact real figure)
		},
		{
			name:          "EURC",
			asset:         canonical.Asset{Type: canonical.AssetClassic, Code: "EURC", Issuer: "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"},
			sacContract:   "CDTKPWPLOURQA2SGTKTUQOWRCBZEORB4BWBOMJ3D3ZTQQSGE5F6JBQLV",
			classicHolder: "GHOLDER_EURC_1",
			classicAmount: 3_000_000_000_000_000, // representative classic-only reading, same shape (exact live figure not captured in this investigation)
			poolHolder:    "CPOOL_EURC_PHOENIX_1",
			poolAmount:    "6000000000000000",
			sacTotal:      "7900000000000000", // representative, > classicAmount alone
		},
		{
			name:          "KALE",
			asset:         canonical.Asset{Type: canonical.AssetClassic, Code: "KALE", Issuer: "GBDVX4VELCDSQ54KQJYTNHXAHFLBCA77ZY2USQBM4CSHTTV7DME7KALE"},
			sacContract:   "CB23WRDQWGSP6YPMY4UV5C4OW5CBTXKYN3XEATG7KJEZCXMJBYEHOUOV",
			classicHolder: "GHOLDER_KALE_1",
			classicAmount: 1_000_000_000_000_000, // representative classic-only reading, same shape (exact live figure not captured in this investigation)
			poolHolder:    "CPOOL_KALE_DEFINDEX_1",
			poolAmount:    "2500000000000000",
			sacTotal:      "3010000000000000", // representative, matches the post-2×-fix KALE served-supply magnitude
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assetKey, err := supply.AssetKey(tc.asset)
			if err != nil {
				t.Fatalf("AssetKey: %v", err)
			}

			sacTotal, ok := new(big.Int).SetString(tc.sacTotal, 10)
			if !ok {
				t.Fatalf("bad fixture: sacTotal %q", tc.sacTotal)
			}
			sacSupply := supply.Supply{AssetKey: tc.sacContract, TotalSupply: sacTotal}

			// (1) Classic-side visibility only: an ordinary trustline
			// holder only — no pool-held balance yet.
			insertTrustline(t, ctx, store, tc.classicHolder, assetKey, 1000, tc.classicAmount, observedAt, false)

			before, err := computer.Compute(ctx, tc.asset, asOfLedger, observedAt)
			if err != nil {
				t.Fatalf("Compute (before): %v", err)
			}

			// (2) Cross-check BEFORE recovery: the over-mint direction
			// (sac_total > classic_total) is DIAGNOSTIC-ONLY since the
			// leg-1 demotion (c4184ddd — BLND retires supply classically
			// with no SAC burn and PHO minted its whole supply through
			// the SAC once, so this direction paged for a week on
			// correct accounting). The new contract this step pins:
			// WithinTolerance stays TRUE (no page) while OverMintStroops
			// still reports the excess so the diagnostic isn't lost.
			resultBefore, err := supply.CrossCheckForClass(before, sacSupply, supply.WrapClassPartial)
			if err != nil {
				t.Fatalf("CrossCheckForClass (before): %v", err)
			}
			if !resultBefore.WithinTolerance {
				t.Fatalf("%s: over-mint direction paged (WithinTolerance=false) — leg-1 demotion contract violated (classic=%s sac=%s)",
					tc.name, before.TotalSupply, sacTotal)
			}
			wantOverMint := new(big.Int).Sub(sacTotal, before.TotalSupply)
			if resultBefore.OverMintStroops == nil || resultBefore.OverMintStroops.Cmp(wantOverMint) != 0 {
				t.Fatalf("%s: OverMintStroops = %v, want %s (the demoted leg must still REPORT the excess)",
					tc.name, resultBefore.OverMintStroops, wantOverMint)
			}

			// (3) Recover the dormant pool-held SAC balance — the exact
			// row a `supply seed-sac-balances -full-history` pass would
			// write via Store.InsertSACBalanceObservation.
			poolAmount, ok := new(big.Int).SetString(tc.poolAmount, 10)
			if !ok {
				t.Fatalf("bad fixture: poolAmount %q", tc.poolAmount)
			}
			if err := store.InsertSACBalanceObservation(ctx, timescale.SACBalanceObservation{
				ContractID: tc.sacContract,
				AssetKey:   assetKey,
				Holder:     tc.poolHolder,
				Ledger:     41_500_000, // the pool's own dormant last-modified ledger, well below asOfLedger
				ObservedAt: observedAt.Add(-time.Hour),
				Balance:    poolAmount,
			}); err != nil {
				t.Fatalf("InsertSACBalanceObservation (recovered pool balance): %v", err)
			}

			after, err := computer.Compute(ctx, tc.asset, asOfLedger, observedAt)
			if err != nil {
				t.Fatalf("Compute (after): %v", err)
			}

			// (4) Acceptance criterion: recovery registers — the
			// cross-check stays within tolerance AND the over-mint
			// diagnostic clears (classic+pool now covers the SAC total,
			// so the reported excess drops to zero).
			resultAfter, err := supply.CrossCheckForClass(after, sacSupply, supply.WrapClassPartial)
			if err != nil {
				t.Fatalf("CrossCheckForClass (after): %v", err)
			}
			if !resultAfter.WithinTolerance {
				t.Errorf("%s: cross-check OVER TOLERANCE after pool-balance recovery — classic_total=%s sac_total=%s divergence=%s",
					tc.name, after.TotalSupply, sacTotal, resultAfter.DivergenceStroops)
			}
			if resultAfter.OverMintStroops != nil && resultAfter.OverMintStroops.Sign() > 0 {
				t.Errorf("%s: OverMintStroops = %s after recovery, want 0 (classic+pool must cover the SAC total)",
					tc.name, resultAfter.OverMintStroops)
			}
			if after.TotalSupply.Cmp(before.TotalSupply) <= 0 {
				t.Errorf("%s: classic total did not increase after recovering the pool balance (before=%s after=%s) — SumSACBalancesAtOrBefore did not pick up the new holder",
					tc.name, before.TotalSupply, after.TotalSupply)
			}
		})
	}
}
