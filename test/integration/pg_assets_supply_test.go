//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/decimalsguard"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sac_balances"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// TestAssetSupplyHistoryCompressionRestored pins that the migration chain
// converges on a compressed asset_supply_history even when 0030 half-applied.
// 0030 commits its decompress + constraint swap inside an explicit
// BEGIN/COMMIT and re-enables compression in a SEPARATE implicit transaction
// after it, so a failure there leaves compression off with version 30 dirty;
// the only recovery, `force 30`, records it applied and nothing after it
// restores compression except the later repair migration.
func TestAssetSupplyHistoryCompressionRestored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 30)
	if !assetSupplyHistoryCompressionEnabled(t, ctx, db) {
		t.Fatal("after a clean 0030, asset_supply_history compression is disabled; want enabled")
	}

	// The state 0030 commits before its trailing ALTER: constraint swapped,
	// compression disabled.
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE asset_supply_history SET (timescaledb.compress = false)`); err != nil {
		t.Fatalf("simulate 0030's committed half: %v", err)
	}

	applyMigrations(t, dsn)

	if !assetSupplyHistoryCompressionEnabled(t, ctx, db) {
		t.Fatal("asset_supply_history compression still disabled at migrations head after a half-applied 0030")
	}
	var segmentby, orderby string
	if err := db.QueryRowContext(ctx, `
        SELECT coalesce(segmentby, ''), coalesce(orderby, '')
          FROM timescaledb_information.hypertable_compression_settings
         WHERE hypertable = 'asset_supply_history'::regclass`).Scan(&segmentby, &orderby); err != nil {
		t.Fatalf("read compression settings: %v", err)
	}
	if segmentby != "asset_key" || orderby != "\"time\" DESC" {
		t.Errorf("compression settings = (segmentby %q, orderby %q), want (asset_key, \"time\" DESC) as 0005 set them",
			segmentby, orderby)
	}
	assertPolicyAttached(t, db, ctx, "asset_supply_history", "policy_compression")

	// Down is forward-only (no-op); the healthy-path up above must be too.
	applyMigrationsUpTo(t, dsn, 166)
	if !assetSupplyHistoryCompressionEnabled(t, ctx, db) {
		t.Error("0168 down disabled asset_supply_history compression; it must be a no-op")
	}
}

func assetSupplyHistoryCompressionEnabled(t *testing.T, ctx context.Context, db *sql.DB) bool {
	t.Helper()
	var enabled bool
	if err := db.QueryRowContext(ctx, `
        SELECT compression_enabled FROM timescaledb_information.hypertables
         WHERE hypertable_name = 'asset_supply_history'`).Scan(&enabled); err != nil {
		t.Fatalf("read asset_supply_history compression_enabled: %v", err)
	}
	return enabled
}

// TestClassicAssetSlugDownsKeepForeignSlugs executes 0135 down and 0134 down
// over rows the migrations wrote and one they did not. 0134's up fills only
// NULL slugs and 0135's writer emits asset_id, so each down may clear only
// its own forms: slug is a UNIQUE public URL key, and an unconditional
// `SET slug = NULL` erases one no migration wrote.
func TestClassicAssetSlugDownsKeepForeignSlugs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 135)
	const (
		ownedID   = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		foreignID = "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO classic_assets
            (asset_id, code, issuer_g_strkey, slug,
             first_seen_at, first_seen_ledger, last_seen_at, last_seen_ledger)
        VALUES ($1, 'USDC', 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN', $1,
                now(), 1, now(), 1),
               ($2, 'AQUA', 'GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA', 'aqua',
                now(), 1, now(), 1)`, ownedID, foreignID); err != nil {
		t.Fatalf("seed classic_assets: %v", err)
	}

	applyMigrationsUpTo(t, dsn, 134)
	if got := classicAssetSlug(t, ctx, db, ownedID); got != "usdc-ga5zsejy" {
		t.Errorf("after 0135 down, 0135-written slug = %q, want 0134's tier-1 form %q", got, "usdc-ga5zsejy")
	}
	if got := classicAssetSlug(t, ctx, db, foreignID); got != "aqua" {
		t.Errorf("after 0135 down, foreign slug = %q, want it kept as %q", got, "aqua")
	}

	applyMigrationsUpTo(t, dsn, 133)
	if got := classicAssetSlug(t, ctx, db, ownedID); got != "" {
		t.Errorf("after 0134 down, 0134-written slug = %q, want NULL", got)
	}
	if got := classicAssetSlug(t, ctx, db, foreignID); got != "aqua" {
		t.Errorf("after 0134 down, foreign slug = %q, want it kept as %q", got, "aqua")
	}
}

func classicAssetSlug(t *testing.T, ctx context.Context, db *sql.DB, assetID string) string {
	t.Helper()
	var slug sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT slug FROM classic_assets WHERE asset_id = $1`, assetID).Scan(&slug); err != nil {
		t.Fatalf("read slug for %s: %v", assetID, err)
	}
	return slug.String
}

// TestClassicSupplyObservationsRoundTrip exercises the four
// classic-supply hypertables through real
// TimescaleDB. Each Sum*AtOrBefore method uses the same
// DISTINCT ON pattern + WHERE NOT is_removal filter; a SQL
// regression in the DISTINCT ON ordering or the is_removal
// handling silently mis-reports Algorithm 2 components.
//
// Companion to the SEP-41 coverage. The Insert + DISTINCT-ON
// + last-writer-wins semantics ship untested at the SQL level
// without this; Go-layer defensive guards catch
// invalid inputs but can't detect a SQL regression.
func TestClassicSupplyObservationsRoundTrip(t *testing.T) {
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
		assetUSDC  = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		assetOther = "AQUA:GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		holderA    = "GA1"
		holderB    = "GA2"
	)
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	t.Run("Trustline", func(t *testing.T) {
		// Insert two trustlines for USDC; sum should be the post-state total.
		insertTrustline(t, ctx, store, holderA, assetUSDC, 1000, 100, t0, false)
		insertTrustline(t, ctx, store, holderB, assetUSDC, 2000, 500, t0.Add(time.Hour), false)

		got, err := store.SumTrustlineBalancesAtOrBefore(ctx, assetUSDC, 5000)
		if err != nil {
			t.Fatalf("Sum: %v", err)
		}
		if got.Cmp(big.NewInt(600)) != 0 {
			t.Errorf("Sum = %s, want 600 (100 + 500)", got)
		}

		// Update holderA — last-writer-wins on the same (account, asset, ledger).
		insertTrustline(t, ctx, store, holderA, assetUSDC, 1000, 999, t0, false)
		got, _ = store.SumTrustlineBalancesAtOrBefore(ctx, assetUSDC, 5000)
		if got.Cmp(big.NewInt(1499)) != 0 {
			t.Errorf("Sum after upsert = %s, want 1499 (999 + 500)", got)
		}

		// Insert at a later ledger — DISTINCT ON should pick the latest.
		insertTrustline(t, ctx, store, holderA, assetUSDC, 3000, 250, t0.Add(2*time.Hour), false)
		got, _ = store.SumTrustlineBalancesAtOrBefore(ctx, assetUSDC, 5000)
		if got.Cmp(big.NewInt(750)) != 0 {
			t.Errorf("Sum at ledger 5000 = %s, want 750 (250 + 500)", got)
		}

		// At-or-before ledger 1500: only the ledger-1000 row counts for holderA.
		got, _ = store.SumTrustlineBalancesAtOrBefore(ctx, assetUSDC, 1500)
		if got.Cmp(big.NewInt(999)) != 0 {
			t.Errorf("Sum at ledger 1500 = %s, want 999 (only ledger-1000 holderA)", got)
		}

		// Removal: holderA's most-recent observation is is_removal=true.
		insertTrustline(t, ctx, store, holderA, assetUSDC, 4000, 0, t0.Add(3*time.Hour), true)
		got, _ = store.SumTrustlineBalancesAtOrBefore(ctx, assetUSDC, 5000)
		if got.Cmp(big.NewInt(500)) != 0 {
			t.Errorf("Sum after removal = %s, want 500 (holderA removed; only holderB at 500 remains)", got)
		}

		// Per-account lookup returns 0 for the removed row.
		balA, _ := store.TrustlineBalanceForAccountAtOrBefore(ctx, holderA, assetUSDC, 5000)
		if balA.Sign() != 0 {
			t.Errorf("removed account balance = %s, want 0", balA)
		}

		// Other-asset isolation — AQUA stays at 0.
		got, _ = store.SumTrustlineBalancesAtOrBefore(ctx, assetOther, 5000)
		if got.Sign() != 0 {
			t.Errorf("isolated asset sum = %s, want 0 (asset_key WHERE filter broken)", got)
		}
	})

	t.Run("Claimable", func(t *testing.T) {
		insertClaimable(t, ctx, store, "claimable-1", assetUSDC, 1000, 5000, t0, false)
		insertClaimable(t, ctx, store, "claimable-2", assetUSDC, 1000, 7000, t0, false)
		insertClaimable(t, ctx, store, "claimable-3", assetOther, 1000, 99, t0, false) // isolation

		got, err := store.SumClaimableBalancesAtOrBefore(ctx, assetUSDC, 2000)
		if err != nil {
			t.Fatalf("Sum: %v", err)
		}
		if got.Cmp(big.NewInt(12_000)) != 0 {
			t.Errorf("Sum = %s, want 12000", got)
		}

		// Claim of one balance — most-recent observation for claimable-1 is removal.
		insertClaimable(t, ctx, store, "claimable-1", assetUSDC, 1500, 0, t0.Add(time.Hour), true)
		got, _ = store.SumClaimableBalancesAtOrBefore(ctx, assetUSDC, 2000)
		if got.Cmp(big.NewInt(7_000)) != 0 {
			t.Errorf("Sum after claim = %s, want 7000", got)
		}

		// Other asset still isolated.
		got, _ = store.SumClaimableBalancesAtOrBefore(ctx, assetOther, 2000)
		if got.Cmp(big.NewInt(99)) != 0 {
			t.Errorf("isolated asset sum = %s, want 99", got)
		}
	})

	t.Run("LPReserve", func(t *testing.T) {
		const pool1 = "pool-1"
		const pool2 = "pool-2"
		// Pool 1 holds USDC + AQUA; pool 2 holds USDC + native (XLM/native).
		// We index per asset side, so each pool emits 2 rows on a delta.
		insertLPReserve(t, ctx, store, pool1, assetUSDC, 1000, 1_000_000, t0, false)
		insertLPReserve(t, ctx, store, pool1, assetOther, 1000, 2_000_000, t0, false)
		insertLPReserve(t, ctx, store, pool2, assetUSDC, 1000, 500_000, t0, false)

		got, err := store.SumLPReservesAtOrBefore(ctx, assetUSDC, 2000)
		if err != nil {
			t.Fatalf("Sum: %v", err)
		}
		if got.Cmp(big.NewInt(1_500_000)) != 0 {
			t.Errorf("Sum USDC = %s, want 1500000 (pool1 + pool2)", got)
		}

		// Pool 1 gets a swap — USDC reserve drops, AQUA reserve rises.
		insertLPReserve(t, ctx, store, pool1, assetUSDC, 2000, 800_000, t0.Add(time.Hour), false)
		insertLPReserve(t, ctx, store, pool1, assetOther, 2000, 2_500_000, t0.Add(time.Hour), false)
		got, _ = store.SumLPReservesAtOrBefore(ctx, assetUSDC, 5000)
		if got.Cmp(big.NewInt(1_300_000)) != 0 {
			t.Errorf("Sum USDC after swap = %s, want 1300000 (800K + 500K)", got)
		}

		// At-or-before ledger 1500: only the original observations count.
		got, _ = store.SumLPReservesAtOrBefore(ctx, assetUSDC, 1500)
		if got.Cmp(big.NewInt(1_500_000)) != 0 {
			t.Errorf("Sum at ledger 1500 = %s, want 1500000", got)
		}
	})

	t.Run("SACBalance", func(t *testing.T) {
		const sacContract = "sac-contract-USDC"
		const otherSAC = "sac-contract-AQUA"
		insertSAC(t, ctx, store, sacContract, holderA, assetUSDC, 1000, 100_000, t0, false)
		insertSAC(t, ctx, store, sacContract, holderB, assetUSDC, 1000, 200_000, t0, false)
		insertSAC(t, ctx, store, otherSAC, holderA, assetOther, 1000, 999, t0, false)

		got, err := store.SumSACBalancesAtOrBefore(ctx, assetUSDC, 2000)
		if err != nil {
			t.Fatalf("Sum: %v", err)
		}
		if got.Cmp(big.NewInt(300_000)) != 0 {
			t.Errorf("Sum USDC = %s, want 300000", got)
		}

		// holderA transfers — balance updates at later ledger.
		insertSAC(t, ctx, store, sacContract, holderA, assetUSDC, 2000, 50_000, t0.Add(time.Hour), false)
		got, _ = store.SumSACBalancesAtOrBefore(ctx, assetUSDC, 5000)
		if got.Cmp(big.NewInt(250_000)) != 0 {
			t.Errorf("Sum after transfer = %s, want 250000 (50K + 200K)", got)
		}

		// Per-contract lookup for holderA returns the latest balance.
		balA, _ := store.SACBalanceForContractAtOrBefore(ctx, holderA, assetUSDC, 5000)
		if balA.Cmp(big.NewInt(50_000)) != 0 {
			t.Errorf("per-contract holderA = %s, want 50000", balA)
		}

		// Removed entry → 0.
		insertSAC(t, ctx, store, sacContract, holderA, assetUSDC, 3000, 0, t0.Add(2*time.Hour), true)
		got, _ = store.SumSACBalancesAtOrBefore(ctx, assetUSDC, 5000)
		if got.Cmp(big.NewInt(200_000)) != 0 {
			t.Errorf("Sum after removal = %s, want 200000 (only holderB remains)", got)
		}
		balA, _ = store.SACBalanceForContractAtOrBefore(ctx, holderA, assetUSDC, 5000)
		if balA.Sign() != 0 {
			t.Errorf("removed balance lookup = %s, want 0", balA)
		}

		// Asset isolation.
		got, _ = store.SumSACBalancesAtOrBefore(ctx, assetOther, 5000)
		if got.Cmp(big.NewInt(999)) != 0 {
			t.Errorf("isolated asset sum = %s, want 999", got)
		}

		// SACBalanceObservationsExist must distinguish "no
		// observation at all" from "observations exist and (possibly)
		// sum to zero" — SumSACBalancesAtOrBefore's COALESCE(sum, 0)
		// alone can't tell those apart, and CrossCheckSubsetBound's
		// escrow gate relies on that distinction.
		const neverSeenAsset = "SHX:GDNEVERSEENSACBALANCEOBSERVATIONFORTHISASSETXXXXXXXXXXXX"
		exists, err := store.SACBalanceObservationsExist(ctx, neverSeenAsset, 5000)
		if err != nil {
			t.Fatalf("SACBalanceObservationsExist(never seen): %v", err)
		}
		if exists {
			t.Error("SACBalanceObservationsExist(never seen) = true, want false")
		}

		exists, err = store.SACBalanceObservationsExist(ctx, assetUSDC, 5000)
		if err != nil {
			t.Fatalf("SACBalanceObservationsExist(assetUSDC): %v", err)
		}
		if !exists {
			t.Error("SACBalanceObservationsExist(assetUSDC) = false, want true — real rows were inserted above")
		}

		// All holders removed: sum is genuinely zero, but the asset WAS
		// observed — that must still read true, not collapse to the
		// "never seen" case.
		insertSAC(t, ctx, store, sacContract, holderB, assetUSDC, 4000, 0, t0.Add(3*time.Hour), true)
		got, _ = store.SumSACBalancesAtOrBefore(ctx, assetUSDC, 5000)
		if got.Sign() != 0 {
			t.Errorf("Sum after all holders removed = %s, want 0", got)
		}
		exists, err = store.SACBalanceObservationsExist(ctx, assetUSDC, 5000)
		if err != nil {
			t.Fatalf("SACBalanceObservationsExist(all removed): %v", err)
		}
		if !exists {
			t.Error("SACBalanceObservationsExist(all removed) = false, want true — a genuine zero-balance reading must not read as unobserved")
		}
	})
}

// ─── Insert helpers ─────────────────────────────────────────────

func insertTrustline(t *testing.T, ctx context.Context, store *timescale.Store, account, assetKey string, ledger uint32, balance int64, observedAt time.Time, removal bool) {
	t.Helper()
	if err := store.InsertTrustlineObservation(ctx, timescale.TrustlineObservation{
		AccountID:  account,
		AssetKey:   assetKey,
		Ledger:     ledger,
		ObservedAt: observedAt,
		Balance:    big.NewInt(balance),
		IsRemoval:  removal,
	}); err != nil {
		t.Fatalf("InsertTrustline %s/%s@%d: %v", account, assetKey, ledger, err)
	}
}

func insertClaimable(t *testing.T, ctx context.Context, store *timescale.Store, claimableID, assetKey string, ledger uint32, balance int64, observedAt time.Time, removal bool) {
	t.Helper()
	if err := store.InsertClaimableObservation(ctx, timescale.ClaimableObservation{
		ClaimableID: claimableID,
		AssetKey:    assetKey,
		Ledger:      ledger,
		ObservedAt:  observedAt,
		Balance:     big.NewInt(balance),
		IsRemoval:   removal,
	}); err != nil {
		t.Fatalf("InsertClaimable %s@%d: %v", claimableID, ledger, err)
	}
}

func insertLPReserve(t *testing.T, ctx context.Context, store *timescale.Store, poolID, assetKey string, ledger uint32, balance int64, observedAt time.Time, removal bool) {
	t.Helper()
	if err := store.InsertLPReserveObservation(ctx, timescale.LPReserveObservation{
		PoolID:     poolID,
		AssetKey:   assetKey,
		Ledger:     ledger,
		ObservedAt: observedAt,
		Balance:    big.NewInt(balance),
		IsRemoval:  removal,
	}); err != nil {
		t.Fatalf("InsertLPReserve %s/%s@%d: %v", poolID, assetKey, ledger, err)
	}
}

func insertSAC(t *testing.T, ctx context.Context, store *timescale.Store, contractID, holder, assetKey string, ledger uint32, balance int64, observedAt time.Time, removal bool) {
	t.Helper()
	if err := store.InsertSACBalanceObservation(ctx, timescale.SACBalanceObservation{
		ContractID: contractID,
		AssetKey:   assetKey,
		Holder:     holder,
		Ledger:     ledger,
		ObservedAt: observedAt,
		Balance:    big.NewInt(balance),
		IsRemoval:  removal,
	}); err != nil {
		t.Fatalf("InsertSACBalance %s/%s@%d: %v", contractID, holder, ledger, err)
	}
}

// TestClaimableSameLedgerTieBreak pins the read-path tie-break.
// Two rows can share a (claimable_id, ledger): an ops seed
// stamps SeedIntraLedgerSeq (MaxUint32, "authoritative reconstructed final
// state") while the live observer writes the real per-ledger ordinal. They
// do NOT collide on the natural key when observed_at differs, so both rows
// persist and the DISTINCT ON must choose deterministically.
//
// Ordering the readers by `ledger DESC` alone leaves the pick to the
// planner — the same shape as a case where a tie between a
// `state` before-image and its `updated` after-image serves whichever the
// engine happens to keep. The seed row must win: it reconstructs the
// ledger's FINAL state, so it belongs at the end of the intra-ledger order.
func TestClaimableSameLedgerTieBreak(t *testing.T) {
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
		asset = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		cbID  = "cb-tie-break"
		ledg  = uint32(63_400_000)
	)
	base := time.Date(2026, 7, 28, 4, 0, 0, 0, time.UTC)

	// Live-observer row: a real, small within-ledger ordinal.
	if err := store.InsertClaimableObservation(ctx, timescale.ClaimableObservation{
		ClaimableID: cbID, AssetKey: asset, Ledger: ledg,
		ObservedAt: base, Balance: big.NewInt(111), IntraLedgerSeq: 5,
	}); err != nil {
		t.Fatalf("insert live-style row: %v", err)
	}

	// Seed row at the SAME ledger, different observed_at so it is a distinct
	// natural key rather than an upsert of the row above.
	if err := store.InsertClaimableObservation(ctx, timescale.ClaimableObservation{
		ClaimableID: cbID, AssetKey: asset, Ledger: ledg,
		ObservedAt: base.Add(time.Second), Balance: big.NewInt(999),
		IntraLedgerSeq: timescale.SeedIntraLedgerSeq,
	}); err != nil {
		t.Fatalf("insert seed-style row: %v", err)
	}

	got, err := store.SumClaimableBalancesAtOrBefore(ctx, asset, ledg)
	if err != nil {
		t.Fatalf("SumClaimableBalancesAtOrBefore: %v", err)
	}
	if got.Cmp(big.NewInt(999)) != 0 {
		t.Fatalf("same-ledger tie resolved to %s, want 999 (the SeedIntraLedgerSeq row) — the read path is ordering by ledger alone", got)
	}
}

// TestMinClassicComponentLedgerUsesObserverWatermark pins that the
// freshness anchor must be the slowest component OBSERVER's watermark, not
// the asset's last activity in that component.
//
// This reproduces the production shape. All four
// observers are current (they have written at ledger ~5000 for SOME asset),
// but the quiet asset's own last claimable event is ancient. That is the
// normal, healthy state for a write-once entry type — nobody created or
// claimed a claimable balance for that asset recently — and it must NOT read
// as staleness.
//
// The query must not take MIN over per-ASSET MAX(ledger): the quiet
// asset's anchor is then pinned to its ancient claimable row. The Refresher then
// sees a lag past the dormancy horizon and refuses every subsequent snapshot,
// freezing the asset's served supply permanently. In production this froze 37
// of 48 watched assets within hours of claimable_observations being seeded
// from lake history; the only assets still publishing were the three that
// happened to have live claimable activity.
func TestMinClassicComponentLedgerUsesObserverWatermark(t *testing.T) {
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
		quiet  = "VELO:GDM4RQUQQUVSKQA7S6EM7XBZP3FCGH4Q7CL6TABQ7B2BEJ5ERARM2M5M"
		lively = "AQUA:GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		absent = "NONE:GCNONEXISTENTASSETKEYFORUNINSTRUMENTEDCOVERAGECHECK000000"
	)
	t0 := time.Date(2026, 7, 28, 3, 0, 0, 0, time.UTC)

	// Every observer is current: each has written at ledger 5000 for the
	// lively asset. The quiet asset is fully observed too, EXCEPT its
	// claimable activity stopped long ago at ledger 1000.
	for _, a := range []string{quiet, lively} {
		insertTrustline(t, ctx, store, "GA-"+a, a, 5000, 100, t0, false)
		insertLPReserve(t, ctx, store, "pool-"+a, a, 5000, 100, t0, false)
		insertSAC(t, ctx, store, "C-"+a, "holder", a, 5000, 100, t0, false)
	}
	insertClaimable(t, ctx, store, "cb-live", lively, 5000, 100, t0, false)
	insertClaimable(t, ctx, store, "cb-old", quiet, 1000, 100, t0.Add(-time.Hour), false)

	got, err := store.MinClassicComponentLedger(ctx, quiet, 10_000)
	if err != nil {
		t.Fatalf("MinClassicComponentLedger(quiet): %v", err)
	}
	if got != 5000 {
		t.Errorf("quiet asset anchor = %d, want 5000 (the observer watermark). "+
			"Got 1000 => the query regressed to per-asset last-activity, which "+
			"freezes any asset without recent claimable activity.", got)
	}

	// A genuinely stalled observer must STILL be caught: roll the other three
	// observers forward and leave claimable behind across every asset.
	for _, a := range []string{quiet, lively} {
		insertTrustline(t, ctx, store, "GA-"+a, a, 9000, 100, t0.Add(time.Hour), false)
		insertLPReserve(t, ctx, store, "pool-"+a, a, 9000, 100, t0.Add(time.Hour), false)
		insertSAC(t, ctx, store, "C-"+a, "holder", a, 9000, 100, t0.Add(time.Hour), false)
	}
	got, err = store.MinClassicComponentLedger(ctx, lively, 10_000)
	if err != nil {
		t.Fatalf("MinClassicComponentLedger(stalled): %v", err)
	}
	if got != 5000 {
		t.Errorf("stalled-observer anchor = %d, want 5000 (claimable's watermark "+
			"holds the MIN down) — stall detection must survive the fix", got)
	}

	// An asset with no observations anywhere stays uninstrumented (0), so the
	// caller skips the gate rather than publishing a zero supply behind a
	// healthy-looking anchor.
	got, err = store.MinClassicComponentLedger(ctx, absent, 10_000)
	if err != nil {
		t.Fatalf("MinClassicComponentLedger(absent): %v", err)
	}
	if got != 0 {
		t.Errorf("uninstrumented asset anchor = %d, want 0", got)
	}
}

// TestNonstandardDecimalsAssets_UpsertAndLoad exercises the round trip
// backing the dex-nonstandard-decimals read-time serving guard (migration
// 0093): the aggregator's decimals-guard sweep upserts a confirmed
// offender via UpsertNonstandardDecimalsAsset; the API's
// NonstandardDecimalsCache loads the full set via
// LoadNonstandardDecimalsAssets on its refresh cadence.
//
// Proves: empty-safe (nothing inserted yet → empty slice, not an error),
// a fresh insert round-trips faithfully, and a re-confirmation of the same
// asset (ON CONFLICT DO UPDATE) refreshes decimals/source/confirmed_at
// rather than producing a duplicate row — the guard's dedup latch means
// this should be rare in practice, but the upsert must still be safe if a
// process restart re-confirms the same standing offender.
func TestNonstandardDecimalsAssets_UpsertAndLoad(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Empty-safe.
	if rows, err := store.LoadNonstandardDecimalsAssets(ctx); err != nil {
		t.Fatalf("LoadNonstandardDecimalsAssets (empty): %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("LoadNonstandardDecimalsAssets (empty) = %d rows, want 0", len(rows))
	}

	const asset = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"

	if err := store.UpsertNonstandardDecimalsAsset(ctx, asset, 9, "aquarius"); err != nil {
		t.Fatalf("UpsertNonstandardDecimalsAsset: %v", err)
	}

	rows, err := store.LoadNonstandardDecimalsAssets(ctx)
	if err != nil {
		t.Fatalf("LoadNonstandardDecimalsAssets: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("LoadNonstandardDecimalsAssets = %d rows, want 1", len(rows))
	}
	if rows[0].Asset != asset || rows[0].Decimals != 9 || rows[0].Source != "aquarius" {
		t.Fatalf("row = %+v, want {Asset:%s Decimals:9 Source:aquarius}", rows[0], asset)
	}
	if rows[0].ConfirmedAt.IsZero() {
		t.Fatal("ConfirmedAt is zero, want a real timestamp (DEFAULT now())")
	}
	firstConfirmedAt := rows[0].ConfirmedAt

	// Re-confirmation (e.g. a process restart re-observing the same
	// standing offender) must upsert in place, not duplicate.
	time.Sleep(10 * time.Millisecond) // ensure a distinguishable now() on refresh
	if err := store.UpsertNonstandardDecimalsAsset(ctx, asset, 9, "phoenix"); err != nil {
		t.Fatalf("UpsertNonstandardDecimalsAsset (re-confirm): %v", err)
	}
	rows, err = store.LoadNonstandardDecimalsAssets(ctx)
	if err != nil {
		t.Fatalf("LoadNonstandardDecimalsAssets (after re-confirm): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("LoadNonstandardDecimalsAssets (after re-confirm) = %d rows, want 1 (upsert, not insert)", len(rows))
	}
	if rows[0].Source != "phoenix" {
		t.Fatalf("Source = %s, want phoenix (re-confirm should refresh source)", rows[0].Source)
	}
	if !rows[0].ConfirmedAt.After(firstConfirmedAt) {
		t.Fatalf("ConfirmedAt did not advance on re-confirm: first=%v second=%v", firstConfirmedAt, rows[0].ConfirmedAt)
	}

	// Delete (the lockstep reconcile's repair when the lake confirms 7 dp)
	// removes the row; a second delete of the now-absent row is a no-op,
	// not an error.
	if err := store.DeleteNonstandardDecimalsAsset(ctx, asset); err != nil {
		t.Fatalf("DeleteNonstandardDecimalsAsset: %v", err)
	}
	rows, err = store.LoadNonstandardDecimalsAssets(ctx)
	if err != nil {
		t.Fatalf("LoadNonstandardDecimalsAssets (after delete): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("LoadNonstandardDecimalsAssets (after delete) = %d rows, want 0", len(rows))
	}
	if err := store.DeleteNonstandardDecimalsAsset(ctx, asset); err != nil {
		t.Fatalf("DeleteNonstandardDecimalsAsset (absent row): %v", err)
	}
}

// lockstepTradeReader is a decimalsguard.TradeReader that enumerates
// nothing — Reconcile does not consult trades, and this keeps the test
// about the projection table.
type lockstepTradeReader struct{}

func (lockstepTradeReader) RecentSorobanDEXTrades(_ context.Context, _ time.Time) ([]timescale.SorobanDEXTradeRef, error) {
	return nil, nil
}

// lockstepResolver stands in for the lake: present ⇒ found.
type lockstepResolver map[string]uint32

func (r lockstepResolver) TokenDecimals(_ context.Context, contractID string) (uint32, bool, error) {
	d, ok := r[contractID]
	return d, ok, nil
}

// TestNonstandardDecimalsAssets_LockstepReconcileThroughStore runs the
// aggregator's lockstep reconcile (decimalsguard.Guard.Reconcile) against
// the REAL store on a real Postgres: the production wiring passes
// *timescale.Store as the guard's Writer, and the compile-time assertion
// in decimalsguard says it satisfies the reconcile seam — this proves the
// three statements behind that seam (load, upsert-repair, delete-repair)
// execute and converge the table on the lake.
func TestNonstandardDecimalsAssets_LockstepReconcileThroughStore(t *testing.T) {
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
		drifted      = "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J" // persisted 6, lake 8
		staleStd     = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO" // persisted 9, lake 7
		agreed       = "CBI7UCH5KGSVQRO5H4SUCZUTZABCITZLRHQQZTWL2TK4RZ72TAR6IHRV" // persisted 18, lake 18
		unresolvable = "CDPV3H7C3MR2R4Y4GAEJN4AXXY4LBITRRVE74VSMVCSBWISIU3Q4QTMW" // persisted 6, lake not derivable
	)
	seed := map[string]uint32{drifted: 6, staleStd: 9, agreed: 18, unresolvable: 6}
	for asset, d := range seed {
		if err := store.UpsertNonstandardDecimalsAsset(ctx, asset, d, "aquarius"); err != nil {
			t.Fatalf("seed %s: %v", asset, err)
		}
	}

	lake := lockstepResolver{drifted: 8, staleStd: 7, agreed: 18}
	guard := decimalsguard.New(lockstepTradeReader{}, lake, decimalsguard.Options{Writer: store})
	if err := guard.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rows, err := store.LoadNonstandardDecimalsAssets(ctx)
	if err != nil {
		t.Fatalf("LoadNonstandardDecimalsAssets (after reconcile): %v", err)
	}
	got := make(map[string]int, len(rows))
	for _, r := range rows {
		got[r.Asset] = r.Decimals
	}
	want := map[string]int{drifted: 8, agreed: 18, unresolvable: 6}
	if len(got) != len(want) {
		t.Fatalf("rows after reconcile = %v, want %v", got, want)
	}
	for asset, d := range want {
		if got[asset] != d {
			t.Errorf("row %s = %d, want %d", asset, got[asset], d)
		}
	}
	if _, still := got[staleStd]; still {
		t.Errorf("row %s remains although the lake confirms 7 dp", staleStd)
	}
	// The invariant the reconcile exists for, checked against the lake:
	// every remaining row the lake can read equals the lake.
	for asset, d := range got {
		if l, ok := lake[asset]; ok && int(l) != d {
			t.Errorf("row %s persisted %d but the lake says %d", asset, d, l)
		}
	}
}

// TestSACBalanceSeedSupersedeAndNumeric exercises the two properties the
// `supply seed-sac-balances` bootstrap relies on, through real
// TimescaleDB (the seed reuses Store.InsertSACBalanceObservation +
// SumSACBalancesAtOrBefore / SACBalanceForContractAtOrBefore):
//
//  1. SUPERSEDE / at-or-before-ledger ordering — a seed written at an
//     OLD ledger must NOT clobber a newer live observation. The readers
//     pick the most-recent row per (contract_id, holder) by ledger DESC,
//     so the higher-ledger row wins regardless of insertion order. This
//     is what makes seeding-then-live-observing (and re-seeding) safe.
//
//  2. NUMERIC round-trip — a dormant contract-held SAC balance larger
//     than 2^63 must survive the *big.Int → NUMERIC → *big.Int trip
//     intact (ADR-0003; the whole point of seeding dormant C-held
//     balances is that they can be huge — ~5.9988e14 for a single
//     Phoenix contract, and nothing caps the aggregate).
func TestSACBalanceSeedSupersedeAndNumeric(t *testing.T) {
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
		sac    = "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32"
		asset  = "PHO:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
		holder = "CCPTA5MVKZG7T3YQZ2X3M4E5EXAMPLEHOLDERZZZZZZZZZZZZZZZZ7"
	)
	t0 := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	insertSACBig := func(ledger uint32, bal *big.Int, at time.Time) {
		t.Helper()
		if err := store.InsertSACBalanceObservation(ctx, timescale.SACBalanceObservation{
			ContractID: sac, AssetKey: asset, Holder: holder,
			Ledger: ledger, ObservedAt: at, Balance: bal,
		}); err != nil {
			t.Fatalf("InsertSACBalanceObservation @%d: %v", ledger, err)
		}
	}

	// (2) NUMERIC round-trip: a dormant C-held balance > 2^63.
	dormant, _ := new(big.Int).SetString("599880000000000000000", 10) // ~6e20 ≫ 2^63
	// Seed the dormant balance at an OLD ledger (the entry's true
	// last-modified ledger, before the live observer's window).
	insertSACBig(62_400_000, dormant, t0)

	got, err := store.SACBalanceForContractAtOrBefore(ctx, holder, asset, 70_000_000)
	if err != nil {
		t.Fatalf("SACBalanceForContractAtOrBefore: %v", err)
	}
	if got.Cmp(dormant) != 0 {
		t.Fatalf("round-trip balance = %s, want %s (NUMERIC truncation?)", got, dormant)
	}
	sum, _ := store.SumSACBalancesAtOrBefore(ctx, asset, 70_000_000)
	if sum.Cmp(dormant) != 0 {
		t.Fatalf("sum after seed = %s, want %s", sum, dormant)
	}

	// (1) SUPERSEDE: a LATER live observation at a higher ledger wins.
	live := big.NewInt(1_000_000)
	insertSACBig(65_000_000, live, t0.Add(time.Hour))
	got, _ = store.SACBalanceForContractAtOrBefore(ctx, holder, asset, 70_000_000)
	if got.Cmp(live) != 0 {
		t.Errorf("after live obs = %s, want %s (higher-ledger live must supersede the seed)", got, live)
	}

	// A re-seed at the OLD ledger must NOT clobber the newer live row.
	insertSACBig(62_400_000, dormant, t0)
	got, _ = store.SACBalanceForContractAtOrBefore(ctx, holder, asset, 70_000_000)
	if got.Cmp(live) != 0 {
		t.Errorf("after re-seed = %s, want %s (old-ledger re-seed must not clobber newer live obs)", got, live)
	}

	// At-or-before the seed ledger only, the dormant seed is the answer.
	got, _ = store.SACBalanceForContractAtOrBefore(ctx, holder, asset, 62_400_500)
	if got.Cmp(dormant) != 0 {
		t.Errorf("at-or-before seed ledger = %s, want %s", got, dormant)
	}
}

// TestSACBalanceSeedProvenanceRoundTrip exercises the sac_balance_seed_
// provenance audit table (migration 0102): a fresh contract has no row,
// the first upsert creates one, and a second upsert with different
// stats OVERWRITES it (one row per contract, not an append log) — the
// same "most recent pass" shape as sep41_supply_rollup's genesis-baseline
// columns (migration 0088).
func TestSACBalanceSeedProvenanceRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const contractID = "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32"
	const assetKey = "PHO:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"

	// Never seeded: absent, not an error.
	_, ok, err := store.SACBalanceSeedProvenanceFor(ctx, contractID)
	if err != nil {
		t.Fatalf("SACBalanceSeedProvenanceFor (never seeded): %v", err)
	}
	if ok {
		t.Fatal("ok=true for a never-seeded contract, want false")
	}

	// First pass: the default current-state source, 16 holders found.
	minL1, maxL1 := uint32(65_000_000), uint32(70_000_000)
	if err := store.UpsertSACBalanceSeedProvenance(ctx, timescale.SACBalanceSeedProvenance{
		ContractID:    contractID,
		AssetKey:      assetKey,
		Source:        timescale.SACBalanceSeedSourceCurrentState,
		HoldersSeeded: 16,
		MinLedgerSeen: &minL1,
		MaxLedgerSeen: &maxL1,
	}); err != nil {
		t.Fatalf("UpsertSACBalanceSeedProvenance (current_state): %v", err)
	}
	got, ok, err := store.SACBalanceSeedProvenanceFor(ctx, contractID)
	if err != nil {
		t.Fatalf("SACBalanceSeedProvenanceFor (after current_state seed): %v", err)
	}
	if !ok {
		t.Fatal("ok=false after an upsert, want true")
	}
	if got.Source != timescale.SACBalanceSeedSourceCurrentState || got.HoldersSeeded != 16 {
		t.Errorf("got=%+v, want source=current_state holders=16", got)
	}
	if got.MinLedgerSeen == nil || *got.MinLedgerSeen != 65_000_000 {
		t.Errorf("MinLedgerSeen = %v, want 65000000", got.MinLedgerSeen)
	}
	if got.LakeVerifiedThrough != nil || got.HoldersRetracted != nil {
		t.Errorf("LakeVerifiedThrough=%v HoldersRetracted=%v, want both nil when the upsert set neither", got.LakeVerifiedThrough, got.HoldersRetracted)
	}

	// Second pass: -full-history, reaching well below the ~62M floor —
	// OVERWRITES the row (one row per contract), evidencing the floor
	// was actually reached via a min_ledger_seen far below 62,000,000.
	minL2, maxL2 := uint32(41_500_000), uint32(70_000_000)
	verified, retracted := uint32(70_000_123), 39
	unproven := timescale.SACBalanceSeedProvenance{
		ContractID:    contractID,
		AssetKey:      assetKey,
		Source:        timescale.SACBalanceSeedSourceFullHistory,
		HoldersSeeded: 40,
		MinLedgerSeen: &minL2,
		MaxLedgerSeen: &maxL2,
	}
	if err := store.UpsertSACBalanceSeedProvenance(ctx, unproven); err == nil {
		t.Fatal("a full_history row without LakeVerifiedThrough was stamped; the source label alone is not evidence")
	}
	proven := unproven
	proven.LakeVerifiedThrough, proven.HoldersRetracted = &verified, &retracted
	if err := store.UpsertSACBalanceSeedProvenance(ctx, proven); err != nil {
		t.Fatalf("UpsertSACBalanceSeedProvenance (full_history): %v", err)
	}
	got, ok, err = store.SACBalanceSeedProvenanceFor(ctx, contractID)
	if err != nil {
		t.Fatalf("SACBalanceSeedProvenanceFor (after full_history seed): %v", err)
	}
	if !ok {
		t.Fatal("ok=false after the second upsert, want true")
	}
	if got.Source != timescale.SACBalanceSeedSourceFullHistory {
		t.Errorf("Source = %q, want full_history (the second upsert should overwrite, not append)", got.Source)
	}
	if got.HoldersSeeded != 40 {
		t.Errorf("HoldersSeeded = %d, want 40", got.HoldersSeeded)
	}
	if got.LakeVerifiedThrough == nil || *got.LakeVerifiedThrough != verified {
		t.Errorf("LakeVerifiedThrough = %v, want %d", got.LakeVerifiedThrough, verified)
	}
	if got.HoldersRetracted == nil || *got.HoldersRetracted != retracted {
		t.Errorf("HoldersRetracted = %v, want %d", got.HoldersRetracted, retracted)
	}
	if got.MinLedgerSeen == nil || *got.MinLedgerSeen >= 62_000_000 {
		t.Errorf("MinLedgerSeen = %v, want < 62,000,000 (evidence the full-history pass reached below the current-state floor)", got.MinLedgerSeen)
	}

	// A wrapper with zero holders found this pass gets nil ledger bounds,
	// not a zero-valued (and misleading — ledger 0 is a real ledger)
	// min/max pair.
	const emptyContract = "CEMPTYWRAPPERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if err := store.UpsertSACBalanceSeedProvenance(ctx, timescale.SACBalanceSeedProvenance{
		ContractID:          emptyContract,
		AssetKey:            "NOPE:GISSUER",
		Source:              timescale.SACBalanceSeedSourceFullHistory,
		LakeVerifiedThrough: &verified,
	}); err != nil {
		t.Fatalf("UpsertSACBalanceSeedProvenance (zero holders): %v", err)
	}
	got, ok, err = store.SACBalanceSeedProvenanceFor(ctx, emptyContract)
	if err != nil {
		t.Fatalf("SACBalanceSeedProvenanceFor (zero holders): %v", err)
	}
	if !ok {
		t.Fatal("ok=false for the zero-holders row, want true (we DID seed, just found nothing)")
	}
	if got.HoldersSeeded != 0 {
		t.Errorf("HoldersSeeded = %d, want 0", got.HoldersSeeded)
	}
	if got.MinLedgerSeen != nil || got.MaxLedgerSeen != nil {
		t.Errorf("MinLedgerSeen=%v MaxLedgerSeen=%v, want both nil for a zero-holder pass", got.MinLedgerSeen, got.MaxLedgerSeen)
	}
}

// TestSACEvictionFallsOutOfServedSupply is the served-money proof, end to end over real TimescaleDB:
//
//	LedgerCloseMeta with an evicted key
//	  → internal/dispatcher (eviction phase)
//	  → sac_balances observer
//	  → timescale.InsertSACBalanceObservation
//	  → SumSACBalancesAtOrBefore (the served SAC supply component)
//
// Soroban state archival is the one way a ledger entry leaves the live
// state without a transaction touching it, so nothing in transaction meta
// can report it. Before the eviction phase the archived balance stayed in
// the served supply FOREVER — the published supply drifted permanently
// above the truth and never self-corrected.
//
// The starting balance is written the way the ops SAC seed writes it, at
// timescale.SeedIntraLedgerSeq (MaxUint32, "authoritative reconstructed
// final state for its ledger"). That is the sentinel the read path's
// tie-break prefers, so it is the row an eviction has to beat: it beats it
// on LEDGER, which is what makes the fix hold against a re-seed of the
// dormant holder the seed exists to recover.
func TestSACEvictionFallsOutOfServedSupply(t *testing.T) {
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
		// Zero-body C-strkey / G-strkey fixtures: a SAC wrapper contract
		// and the holder whose balance ages out of the live state.
		sorobanContract = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4"
		holderAccount   = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF"
		assetKey        = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

		seedLedger    = uint32(56_400_000)
		evictLedger   = uint32(63_500_000)
		restoreLedger = uint32(63_600_000)
	)
	t0 := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)

	observer, err := sac_balances.NewObserver(map[string]string{sorobanContract: assetKey})
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	disp := dispatcher.New()
	disp.AddEntryDecoder(observer)

	// ── The holder's last write, as the ops SAC seed lands it.
	if err := store.InsertSACBalanceObservation(ctx, timescale.SACBalanceObservation{
		ContractID:     sorobanContract,
		AssetKey:       assetKey,
		Holder:         holderAccount,
		Ledger:         seedLedger,
		ObservedAt:     t0,
		Balance:        big.NewInt(1_000_000),
		IntraLedgerSeq: timescale.SeedIntraLedgerSeq,
	}); err != nil {
		t.Fatalf("seed the dormant holder: %v", err)
	}
	if got := sumSAC(t, ctx, store, assetKey, restoreLedger); got.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("served SAC component before eviction = %s, want 1000000", got)
	}

	// ── The ledger that archives it. No transactions: eviction is applied
	// at ledger close and belongs to no transaction.
	lcm := evictionOnlyLedger(evictLedger, []xdr.LedgerKey{
		evictedSACBalanceKey(t, sorobanContract, holderAccount),
	})
	evs, err := disp.ProcessLedger(lcm, testPassphrase)
	if err != nil {
		t.Fatalf("ProcessLedger(eviction): %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("eviction ledger produced %d observations, want 1", len(evs))
	}
	persistSAC(t, ctx, store, evs[0].(sac_balances.Observation))

	got := sumSAC(t, ctx, store, assetKey, restoreLedger)
	if got.Sign() != 0 {
		t.Fatalf("served SAC component after the eviction = %s, want 0 — the archived balance is still "+
			"published as live supply and nothing will ever take it back out", got)
	}

	// ── Archival is reversible: a restore must put the balance back, or the
	// fix has traded a permanent over-count for a permanent under-count.
	// Routed through the dispatcher's entry-change seam (the eviction path
	// above is what needed the LCM walk; a restore arrives as an ordinary
	// apply-phase change).
	restoreEvs, err := disp.RouteEntryChange(dispatcher.LedgerEntryChangeContext{
		Ledger:         restoreLedger,
		ClosedAt:       t0.Add(2 * time.Hour),
		OpIndex:        0,
		IntraLedgerSeq: 3,
		Change:         restoredSACBalanceChange(t, sorobanContract, holderAccount, 1_000_000),
	})
	if err != nil {
		t.Fatalf("RouteEntryChange(restore): %v", err)
	}
	if len(restoreEvs) != 1 {
		t.Fatalf("restore produced %d observations, want 1", len(restoreEvs))
	}
	persistSAC(t, ctx, store, restoreEvs[0].(sac_balances.Observation))

	if got := sumSAC(t, ctx, store, assetKey, restoreLedger); got.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("served SAC component after the restore = %s, want 1000000 — an entry brought back out "+
			"of the archive is live state again", got)
	}
}

func sumSAC(t *testing.T, ctx context.Context, store *timescale.Store, assetKey string, asOf uint32) *big.Int {
	t.Helper()
	got, err := store.SumSACBalancesAtOrBefore(ctx, assetKey, asOf)
	if err != nil {
		t.Fatalf("SumSACBalancesAtOrBefore: %v", err)
	}
	return got
}

// persistSAC mirrors pipeline.persistSACBalanceObservation — the production
// sink for this observation type.
func persistSAC(t *testing.T, ctx context.Context, store *timescale.Store, o sac_balances.Observation) {
	t.Helper()
	if err := store.InsertSACBalanceObservation(ctx, timescale.SACBalanceObservation{
		ContractID:     o.ContractID,
		AssetKey:       o.AssetKey,
		Holder:         o.Holder,
		Ledger:         o.Ledger,
		ObservedAt:     o.ObservedAt,
		Balance:        o.Balance,
		IsRemoval:      o.IsRemoval,
		IntraLedgerSeq: o.IntraLedgerSeq,
	}); err != nil {
		t.Fatalf("InsertSACBalanceObservation %s/%s@%d: %v", o.ContractID, o.Holder, o.Ledger, err)
	}
}

// sacBalanceScKey builds the SEP-41 `Vec(Symbol("Balance"), Address)` key
// a SAC stores a holder's balance under.
func sacBalanceScKey(t *testing.T, holder string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, holder)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", holder, err)
	}
	var pk [32]byte
	copy(pk[:], raw)
	accountID := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: (*xdr.Uint256)(&pk)}
	address := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &accountID}
	sym := xdr.ScSymbol("Balance")
	vec := xdr.ScVec{
		{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
		{Type: xdr.ScValTypeScvAddress, Address: &address},
	}
	vp := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vp}
}

func sacContractAddress(t *testing.T, contract string) xdr.ScAddress {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, contract)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", contract, err)
	}
	var id [32]byte
	copy(id[:], raw)
	contractID := xdr.ContractId(id)
	return xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &contractID}
}

// evictedSACBalanceKey is the ledger key core reports when a SAC balance
// entry's TTL lapses and it is archived out of the live state.
func evictedSACBalanceKey(t *testing.T, contract, holder string) xdr.LedgerKey {
	t.Helper()
	return xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   sacContractAddress(t, contract),
			Key:        sacBalanceScKey(t, holder),
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
}

func restoredSACBalanceChange(t *testing.T, contract, holder string, amount int64) xdr.LedgerEntryChange {
	t.Helper()
	return xdr.LedgerEntryChange{
		Type: xdr.LedgerEntryChangeTypeLedgerEntryRestored,
		Restored: &xdr.LedgerEntry{
			Data: xdr.LedgerEntryData{
				Type: xdr.LedgerEntryTypeContractData,
				ContractData: &xdr.ContractDataEntry{
					Contract:   sacContractAddress(t, contract),
					Key:        sacBalanceScKey(t, holder),
					Durability: xdr.ContractDataDurabilityPersistent,
					Val: xdr.ScVal{
						Type: xdr.ScValTypeScvI128,
						I128: &xdr.Int128Parts{Hi: 0, Lo: xdr.Uint64(amount)},
					},
				},
			},
		},
	}
}

// evictionOnlyLedger is a transaction-free ledger that archives the given
// keys at close — the shape of a ledger in which a dormant balance ages out.
func evictionOnlyLedger(seq uint32, keys []xdr.LedgerKey) xdr.LedgerCloseMeta {
	return xdr.LedgerCloseMeta{
		V: 1,
		V1: &xdr.LedgerCloseMetaV1{
			LedgerHeader: xdr.LedgerHeaderHistoryEntry{
				Header: xdr.LedgerHeader{
					LedgerSeq: xdr.Uint32(seq),
					ScpValue:  xdr.StellarValue{CloseTime: xdr.TimePoint(1_758_270_000)},
				},
			},
			TxSet: xdr.GeneralizedTransactionSet{
				V:       1,
				V1TxSet: &xdr.TransactionSetV1{},
			},
			EvictedKeys: keys,
		},
	}
}

// TestSupplyCoverageStats_CountsEveryAssetWithNoWindow pins the diagnostic's
// answer to all history: an asset last snapshotted a year ago still counts,
// and the newest snapshot supplies last_snapshot_at and latest_ledger.
func TestSupplyCoverageStats_CountsEveryAssetWithNoWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if got, err := store.SupplyCoverageStats(ctx); err != nil || got != (timescale.SupplyCoverage{}) {
		t.Fatalf("empty table: got %+v, %v; want zero value", got, err)
	}

	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	sep41 := "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	classicOld := "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	rows := []struct {
		key    string
		ledger uint32
		at     time.Time
	}{
		{"XLM", 59_000_000, now.Add(-24 * time.Hour)},
		{"XLM", 59_100_000, now},
		{sep41, 58_000_000, now.Add(-40 * 24 * time.Hour)},
		{classicOld, 53_000_000, now.Add(-365 * 24 * time.Hour)},
	}
	for _, r := range rows {
		if err := store.InsertSupply(ctx, supply.Supply{
			AssetKey:          r.key,
			TotalSupply:       big.NewInt(1),
			CirculatingSupply: big.NewInt(1),
			Basis:             supply.BasisXLMSDFReserveExclusion,
			LedgerSequence:    r.ledger,
			ObservedAt:        r.at,
		}); err != nil {
			t.Fatalf("InsertSupply %s@%d: %v", r.key, r.ledger, err)
		}
	}

	got, err := store.SupplyCoverageStats(ctx)
	if err != nil {
		t.Fatalf("SupplyCoverageStats: %v", err)
	}
	want := timescale.SupplyCoverage{
		ClassicAssets:  2, // XLM and the year-old USDC
		SEP41Assets:    1,
		LastSnapshotAt: now,
		LatestLedger:   59_100_000,
	}
	if got.ClassicAssets != want.ClassicAssets || got.SEP41Assets != want.SEP41Assets ||
		!got.LastSnapshotAt.Equal(want.LastSnapshotAt) || got.LatestLedger != want.LatestLedger {
		t.Fatalf("SupplyCoverageStats = %+v, want %+v", got, want)
	}
}

// TestLatestSupplyObservations_BoundsVintage is the storage-side proof of the
// stale-vintage defect in the listing's supply arm.
//
// The listing's authoritative supply arm read supply_1d — a DAILY roll-up of
// asset_supply_history — with `bucket = max(bucket)` and no vintage bound at
// all. supply_1d's refresh policy carries an end_offset, so the current day's
// bucket is never fully covered by a refresh window and never materialises;
// the newest bucket is therefore always a COMPLETED PREVIOUS day, so the value
// in it is the last observation of the PREVIOUS UTC day — on r1's 6-hourly
// refresh, between about 2.9 and about 26.9 hours old, and 17 h 47 m old at
// the moment of measurement. On r1 that served USDC at 354,858,863.57 against
// 375,766,247.91 outstanding — 5.57% low, roughly $21M of market cap — while
// the response envelope reported the figure fresh.
//
// Two properties are asserted together because either alone is satisfied by
// code that still has the bug: the read returns the LATEST observation (not a
// day-old roll-up of it), and it returns NOTHING for an asset whose newest
// observation is older than the bound.
func TestLatestSupplyObservations_BoundsVintage(t *testing.T) {
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
		usdcKey = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		usdcID  = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		phoKey  = "PHO:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
		phoID   = "PHO-GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
		bound   = 6 * time.Hour
	)
	now := time.Now().UTC()

	// USDC: the day-old reading that a max(bucket) read serves, then the live one.
	// Both are inside the bound, so the newest must win — this is the half
	// of the contract a max(bucket) read satisfies for the WRONG day.
	mustInsert(t, ctx, store, usdcKey, "3548588635712599", now.Add(-26*time.Hour), 64_400_000)
	mustInsert(t, ctx, store, usdcKey, "3763021295452262", now.Add(-4*time.Minute), 64_443_400)

	// XLM exercises the `XLM` -> `native` key translation the listing joins on.
	mustInsert(t, ctx, store, "XLM", "348508791955885292", now.Add(-5*time.Minute), 64_443_401)

	// PHO's observer has stopped: its newest reading is outside the bound and
	// must not be offered at all, so the caller falls through to its next arm
	// instead of publishing a figure nobody is computing.
	mustInsert(t, ctx, store, phoKey, "778827871496573", now.Add(-30*time.Hour), 64_390_000)

	got, err := store.LatestSupplyObservations(ctx, bound)
	if err != nil {
		t.Fatalf("LatestSupplyObservations: %v", err)
	}

	obs, ok := got[usdcID]
	if !ok {
		t.Fatalf("USDC missing from %v — a fresh observation must be served", keysOf(got))
	}
	if obs.CirculatingSupply != "3763021295452262" {
		t.Errorf("USDC circulating = %q, want the newest observation 3763021295452262 (a stale "+
			"reading outranking a live one is the defect this test exists for)", obs.CirculatingSupply)
	}
	if obs.Basis != string(supply.BasisIssuerExclusion) {
		t.Errorf("USDC basis = %q, want %q — the arm must publish the basis the observer recorded",
			obs.Basis, supply.BasisIssuerExclusion)
	}
	if age := time.Since(obs.ObservedAt); age <= 0 || age > bound {
		t.Errorf("USDC ObservedAt = %v (age %v), want a vintage inside the bound", obs.ObservedAt, age)
	}

	if _, ok := got["native"]; !ok {
		t.Errorf("native missing from %v — XLM must translate to the listing's asset_id", keysOf(got))
	}

	if stale, ok := got[phoID]; ok {
		t.Errorf("PHO served at %q from an observation %v old, past the %v bound — "+
			"an out-of-bound reading must not be offered",
			stale.CirculatingSupply, time.Since(stale.ObservedAt).Round(time.Hour), bound)
	}

	// A non-positive bound admits nothing. That is the safe direction: the
	// caller falls back to its next arm rather than publishing an observation
	// of unknown vintage.
	if none, err := store.LatestSupplyObservations(ctx, 0); err != nil {
		t.Fatalf("LatestSupplyObservations(0): %v", err)
	} else if len(none) != 0 {
		t.Errorf("LatestSupplyObservations(0) returned %d rows, want none", len(none))
	}
}

func mustInsert(
	t *testing.T, ctx context.Context, store *timescale.Store,
	assetKey, circ string, at time.Time, ledger uint32,
) {
	t.Helper()
	n, ok := new(big.Int).SetString(circ, 10)
	if !ok {
		t.Fatalf("bad fixture supply %q", circ)
	}
	basis := supply.BasisIssuerExclusion
	if assetKey == "XLM" {
		basis = supply.BasisXLMSDFReserveExclusion
	}
	if err := store.InsertSupply(ctx, supply.Supply{
		AssetKey:          assetKey,
		TotalSupply:       n,
		CirculatingSupply: n,
		Basis:             basis,
		LedgerSequence:    ledger,
		ObservedAt:        at,
	}); err != nil {
		t.Fatalf("InsertSupply %s at %v: %v", assetKey, at, err)
	}
}

func keysOf(m map[string]timescale.SupplyObservation) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestSupplyStorageRoundTrip exercises the InsertSupply →
// LatestSupply → SupplyHistory paths through real TimescaleDB with
// the asset_supply_history hypertable migration applied. Together
// with the unit tests for assembleSupply this is the end-to-end
// proof of the storage layer for ADR-0011 supply data.
func TestSupplyStorageRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// ─── LatestSupply on empty table → ErrNotFound ──────────────
	if _, err := store.LatestSupply(ctx, "XLM"); !errors.Is(err, timescale.ErrNotFound) {
		t.Fatalf("LatestSupply on empty table: err = %v, want ErrNotFound", err)
	}

	// ─── Insert XLM snapshot at ledger 50_000_000 ───────────────
	xlmTotal, _ := new(big.Int).SetString("500018068120000000", 10) // 50.0018... B XLM in stroops
	xlmCirc, _ := new(big.Int).SetString("499000000000000000", 10)
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	snap := supply.Supply{
		AssetKey:          "XLM",
		TotalSupply:       xlmTotal,
		CirculatingSupply: xlmCirc,
		MaxSupply:         xlmTotal, // XLM is hard-capped at total
		Basis:             supply.BasisXLMSDFReserveExclusion,
		LedgerSequence:    50_000_000,
		ObservedAt:        t0,
	}
	if err := store.InsertSupply(ctx, snap); err != nil {
		t.Fatalf("InsertSupply: %v", err)
	}

	// Idempotent re-insert at the same ledger is a no-op.
	if err := store.InsertSupply(ctx, snap); err != nil {
		t.Fatalf("InsertSupply (duplicate): %v", err)
	}

	// ─── LatestSupply round-trips ───────────────────────────────
	got, err := store.LatestSupply(ctx, "XLM")
	if err != nil {
		t.Fatalf("LatestSupply: %v", err)
	}
	if got.TotalSupply.Cmp(xlmTotal) != 0 {
		t.Errorf("TotalSupply = %s, want %s", got.TotalSupply, xlmTotal)
	}
	if got.CirculatingSupply.Cmp(xlmCirc) != 0 {
		t.Errorf("CirculatingSupply = %s, want %s", got.CirculatingSupply, xlmCirc)
	}
	if got.MaxSupply == nil || got.MaxSupply.Cmp(xlmTotal) != 0 {
		t.Errorf("MaxSupply = %v, want %s", got.MaxSupply, xlmTotal)
	}
	if got.Basis != supply.BasisXLMSDFReserveExclusion {
		t.Errorf("Basis = %q", got.Basis)
	}
	if got.LedgerSequence != 50_000_000 {
		t.Errorf("LedgerSequence = %d", got.LedgerSequence)
	}

	// ─── Insert a later snapshot — Latest must advance ───────────
	t1 := t0.Add(1 * time.Hour)
	xlmCirc2, _ := new(big.Int).SetString("499010000000000000", 10) // 1M XLM less reserved
	snap2 := snap
	snap2.CirculatingSupply = xlmCirc2
	snap2.LedgerSequence = 50_001_000
	snap2.ObservedAt = t1
	if err := store.InsertSupply(ctx, snap2); err != nil {
		t.Fatalf("InsertSupply (advance): %v", err)
	}

	got, err = store.LatestSupply(ctx, "XLM")
	if err != nil {
		t.Fatalf("LatestSupply (after advance): %v", err)
	}
	if got.LedgerSequence != 50_001_000 {
		t.Errorf("Latest didn't advance; LedgerSequence = %d, want 50_001_000", got.LedgerSequence)
	}
	if got.CirculatingSupply.Cmp(xlmCirc2) != 0 {
		t.Errorf("Latest circulating = %s, want %s", got.CirculatingSupply, xlmCirc2)
	}

	// ─── Classic asset with NULL max_supply ─────────────────────
	usdcKey := "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdcSnap := supply.Supply{
		AssetKey:          usdcKey,
		TotalSupply:       big.NewInt(1_000_000_000),
		CirculatingSupply: big.NewInt(990_000_000),
		MaxSupply:         nil, // uncapped issuer + no override
		Basis:             supply.BasisIssuerExclusion,
		LedgerSequence:    50_000_000,
		ObservedAt:        t0,
	}
	if err := store.InsertSupply(ctx, usdcSnap); err != nil {
		t.Fatalf("InsertSupply USDC: %v", err)
	}
	gotUSDC, err := store.LatestSupply(ctx, usdcKey)
	if err != nil {
		t.Fatalf("LatestSupply USDC: %v", err)
	}
	if gotUSDC.MaxSupply != nil {
		t.Errorf("USDC MaxSupply = %v, want nil", gotUSDC.MaxSupply)
	}

	// ─── SupplyHistory returns both XLM rows in time order ──────
	hist, err := store.SupplyHistory(ctx, "XLM", t0.Add(-1*time.Hour), t1.Add(1*time.Hour), 0)
	if err != nil {
		t.Fatalf("SupplyHistory: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("SupplyHistory returned %d rows, want 2", len(hist))
	}
	if hist[0].LedgerSequence != 50_000_000 {
		t.Errorf("hist[0].LedgerSequence = %d, want 50_000_000 (ascending order)", hist[0].LedgerSequence)
	}
	if hist[1].LedgerSequence != 50_001_000 {
		t.Errorf("hist[1].LedgerSequence = %d, want 50_001_000", hist[1].LedgerSequence)
	}

	// SupplyHistory with limit caps results.
	histLimited, err := store.SupplyHistory(ctx, "XLM", t0.Add(-1*time.Hour), t1.Add(1*time.Hour), 1)
	if err != nil {
		t.Fatalf("SupplyHistory (limit=1): %v", err)
	}
	if len(histLimited) != 1 {
		t.Errorf("SupplyHistory(limit=1) returned %d rows", len(histLimited))
	}

	// SupplyHistory across an empty window returns []
	histEmpty, err := store.SupplyHistory(ctx, "XLM", t0.Add(2*time.Hour), t0.Add(3*time.Hour), 0)
	if err != nil {
		t.Fatalf("SupplyHistory (empty window): %v", err)
	}
	if len(histEmpty) != 0 {
		t.Errorf("SupplyHistory across empty window returned %d rows", len(histEmpty))
	}
}
