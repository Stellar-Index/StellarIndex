//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// TestSupplyCrossCheckConvergesAfterPoolBalanceRecovery is the
// acceptance test for the BLND/EURC/KALE/PHO supply_cross_check_divergence
// residual (incident 2026-07-06 "PHO/BLND VERDICT", ROADMAP #14). It
// can't run against live r1 data from here, so it reproduces the
// documented divergence SHAPE with synthetic rows through the REAL
// Algorithm-2 pipeline (StorageClassicSupplyReader.ClassicSupplyAt →
// ClassicComputer.Compute, exactly what the aggregator's per-asset
// Refresher runs) against a real TimescaleDB:
//
//  1. Insert only the classic-side components a pre-fix system would
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
//     sac_full_history_seed_test.go covers that extraction step
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

	reader := supply.NewStorageClassicSupplyReader(store)
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
		// pre-fix "visible" classic supply.
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
			classicAmount: 200_000_000_000_000, // representative pre-fix Alg-2 reading — well under sacTotal
			poolHolder:    "CPOOL_PHO_PHOENIX_1",
			poolAmount:    "1900000000000000", // recovers the dormant pool balance; classic+pool > sacTotal
			sacTotal:      "1999999993050277", // PHO lifetime SAC supply, verified 2026-07-06 (exact real figure)
		},
		{
			name:          "BLND",
			asset:         canonical.Asset{Type: canonical.AssetClassic, Code: "BLND", Issuer: "GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"},
			sacContract:   "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY",
			classicHolder: "GHOLDER_BLND_1",
			classicAmount: 1_100_000_000_000_000, // representative pre-fix reading — under sacTotal by ~12%, matching the incident's ~12.4%-under BLND finding
			poolHolder:    "CPOOL_BLND_BACKSTOP_1",
			poolAmount:    "200000000000000",
			sacTotal:      "1236670485295609", // BLND lifetime SAC supply, verified 2026-07-06 (exact real figure)
		},
		{
			name:          "EURC",
			asset:         canonical.Asset{Type: canonical.AssetClassic, Code: "EURC", Issuer: "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"},
			sacContract:   "CDTKPWPLOURQA2SGTKTUQOWRCBZEORB4BWBOMJ3D3ZTQQSGE5F6JBQLV",
			classicHolder: "GHOLDER_EURC_1",
			classicAmount: 3_000_000_000_000_000, // representative pre-fix reading, same shape (exact live figure not captured in this investigation)
			poolHolder:    "CPOOL_EURC_PHOENIX_1",
			poolAmount:    "6000000000000000",
			sacTotal:      "7900000000000000", // representative, > classicAmount alone
		},
		{
			name:          "KALE",
			asset:         canonical.Asset{Type: canonical.AssetClassic, Code: "KALE", Issuer: "GBDVX4VELCDSQ54KQJYTNHXAHFLBCA77ZY2USQBM4CSHTTV7DME7KALE"},
			sacContract:   "CB23WRDQWGSP6YPMY4UV5C4OW5CBTXKYN3XEATG7KJEZCXMJBYEHOUOV",
			classicHolder: "GHOLDER_KALE_1",
			classicAmount: 1_000_000_000_000_000, // representative pre-fix reading, same shape (exact live figure not captured in this investigation)
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

			// (1) Pre-fix classic-side visibility: an ordinary trustline
			// holder only — no pool-held balance yet.
			insertTrustline(t, ctx, store, tc.classicHolder, assetKey, 1000, tc.classicAmount, observedAt, false)

			before, err := computer.Compute(ctx, tc.asset, asOfLedger, observedAt)
			if err != nil {
				t.Fatalf("Compute (before): %v", err)
			}

			// (2) Cross-check BEFORE recovery reproduces the documented
			// divergence: sac_total > classic_total → `over`.
			resultBefore, err := supply.CrossCheckForClass(before, sacSupply, supply.WrapClassPartial)
			if err != nil {
				t.Fatalf("CrossCheckForClass (before): %v", err)
			}
			if resultBefore.WithinTolerance {
				t.Fatalf("%s: cross-check WithinTolerance=true BEFORE pool-balance recovery (classic=%s sac=%s) — fixture doesn't reproduce the documented divergence; adjust classicAmount/poolAmount/sacTotal so classic < sac",
					tc.name, before.TotalSupply, sacTotal)
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

			// (4) Acceptance criterion: the cross-check now converges.
			resultAfter, err := supply.CrossCheckForClass(after, sacSupply, supply.WrapClassPartial)
			if err != nil {
				t.Fatalf("CrossCheckForClass (after): %v", err)
			}
			if !resultAfter.WithinTolerance {
				t.Errorf("%s: cross-check still OVER TOLERANCE after pool-balance recovery — classic_total=%s sac_total=%s divergence=%s (want classic_total >= sac_total)",
					tc.name, after.TotalSupply, sacTotal, resultAfter.DivergenceStroops)
			}
			if after.TotalSupply.Cmp(before.TotalSupply) <= 0 {
				t.Errorf("%s: classic total did not increase after recovering the pool balance (before=%s after=%s) — SumSACBalancesAtOrBefore did not pick up the new holder",
					tc.name, before.TotalSupply, after.TotalSupply)
			}
		})
	}
}
