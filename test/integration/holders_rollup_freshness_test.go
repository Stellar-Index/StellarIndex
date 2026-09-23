//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestHoldersRollupFreshness_ExecutesAgainstServer runs the real
// ch-holders-rollup cycle, then ages its stamp, against a real ClickHouse
// server (T390). It proves: the writer's UTC-pinned cycle stamp round-trips
// as the true instant; a fresh cycle is served from the rollup, including
// the live-table stamp read for an asset absent from it; and once the cycle
// is older than the reader's max age, AssetHolders stops serving the rollup
// and answers from the live per-request scans.
func TestHoldersRollupFreshness_ExecutesAgainstServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	// Other tests in this package read AssetHolders expecting the legacy
	// path; leave the rollup as unpopulated as this test found it.
	t.Cleanup(func() {
		for _, table := range []string{
			"asset_holders_rollup", "asset_holders_counts", "accounts_stats",
			"accounts_wealth_histogram", "accounts_trustline_histogram",
		} {
			_ = conn.Exec(context.Background(), "TRUNCATE TABLE stellar."+table)
		}
	})

	const listedAsset, lateAsset = "T390L-GISSUERT390", "T390N-GISSUERT390"
	seedEntry := func(entryType, asset, holder string, ledger uint32) {
		t.Helper()
		row := chstore.LedgerEntryChangeRow{
			LedgerSeq: ledger, CloseTime: time.Date(2024, 3, 3, 0, 0, 0, 0, time.UTC),
			TxHash: "t390", IntraLedgerSeq: 1, ChangeType: "created", EntryType: entryType,
			KeyXDR: "t390-" + entryType + "-" + holder + asset, EntryXDR: "t390", AccountID: holder,
			Asset: asset, Balance: 42,
		}
		if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{row}, 0); err != nil {
			t.Fatalf("InsertEntryChanges(%s %s): %v", entryType, asset, err)
		}
	}

	// The cycle's accounts_stats arm needs at least one funded account
	// (avg() over none is NaN, which toInt64 rejects).
	seedEntry("account", "", "GHOLDERT390L", 72_000_001)
	seedEntry("trustline", listedAsset, "GHOLDERT390L", 72_000_001)
	if err := chstore.RunHoldersRollup(ctx, addr, t.Logf); err != nil {
		t.Fatalf("RunHoldersRollup: %v", err)
	}
	// Issued after the cycle: absent from the rollup, present in the lake.
	seedEntry("trustline", lateAsset, "GHOLDERT390N", 72_000_002)

	var stamp time.Time
	if err := conn.QueryRow(ctx, `SELECT max(computed_at) FROM stellar.asset_holders_rollup`).Scan(&stamp); err != nil {
		t.Fatalf("read cycle stamp: %v", err)
	}
	if age := time.Since(stamp); age < -time.Minute || age > 5*time.Minute {
		t.Fatalf("cycle stamp %s is %s from now — the writer's stamp did not round-trip as the true instant", stamp, age)
	}

	holders := func(asset string) int64 {
		t.Helper()
		r, err := chstore.NewExplorerReader(ctx, addr)
		if err != nil {
			t.Fatalf("NewExplorerReader: %v", err)
		}
		defer func() { _ = r.Close() }()
		_, total, err := r.AssetHolders(ctx, asset, 5)
		if err != nil {
			t.Fatalf("AssetHolders(%s): %v", asset, err)
		}
		return total
	}

	if got := holders(listedAsset); got != 1 {
		t.Errorf("fresh cycle, listed asset: total = %d, want 1", got)
	}
	// Fresh cycle: the rollup is authoritative, so the late asset reads as
	// zero holders — the live scan would have found one.
	if got := holders(lateAsset); got != 0 {
		t.Errorf("fresh cycle, late asset: total = %d, want 0 served from the rollup", got)
	}

	for _, table := range []string{"asset_holders_rollup", "asset_holders_counts"} {
		if err := conn.Exec(ctx, "ALTER TABLE stellar."+table+
			" UPDATE computed_at = computed_at - INTERVAL 3 HOUR WHERE 1 SETTINGS mutations_sync = 1"); err != nil {
			t.Fatalf("age %s: %v", table, err)
		}
	}
	// Stale cycle: the rollup must no longer answer; the live scan finds the
	// late asset's holder.
	if got := holders(lateAsset); got != 1 {
		t.Errorf("stale cycle, late asset: total = %d, want 1 from the live scan — a 3h-old rollup was served as current", got)
	}
}
