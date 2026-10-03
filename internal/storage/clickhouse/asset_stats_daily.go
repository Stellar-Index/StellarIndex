package clickhouse

import (
	"context"
	"fmt"
	"time"
)

// assetStatsDailyLatestDay is the FROM clause the snapshot shrink guard counts
// as live: the newest stored day, which this cycle replaces or succeeds.
const assetStatsDailyLatestDay = `stellar.asset_stats_daily WHERE day = (SELECT max(day) FROM stellar.asset_stats_daily)`

// assetStatsSnapshotFill computes one arm of the day's snapshot into staging.
// Ranks run over every non-removed entry, zero balances included: they sort
// after every positive balance, so they shift no holder's rank and add
// nothing to any sum. With x ranked descending (r = 1 is the largest) the
// Gini over the n positive holders is ((n+1)·S − 2·Σ r·x) / (n·S); the
// numerator is exact in Int128 for any n below 2^31, only the ratio is float.
func assetStatsSnapshotFill(at, day, rankedSource string) string {
	return `INSERT INTO stellar.asset_stats_daily_staging
	     (day, asset, holders, trustlines, balance_total, top10_balance, top100_balance, gini, computed_at)
	 SELECT toDate('` + day + `'), asset, holders, trustlines, total, top10, top100,
	        if(total = 0, NULL, toFloat64((toInt128(holders) + 1) * total - 2 * weighted) / toFloat64(toInt128(holders) * total)),
	        toDateTime(` + at + `)
	 FROM (
	     SELECT asset,
	            toInt64(countIf(balance > 0)) AS holders,
	            toInt64(count()) AS trustlines,
	            sum(toInt128(balance)) AS total,
	            sumIf(toInt128(balance), rank <= 10) AS top10,
	            sumIf(toInt128(balance), rank <= 100) AS top100,
	            sum(toInt128(rank) * toInt128(balance)) AS weighted
	     FROM (` + rankedSource + `)
	     GROUP BY asset
	 )
	 SETTINGS max_threads = 4, max_memory_usage = 8589934592,
	          max_bytes_before_external_group_by = 4000000000, max_bytes_before_external_sort = 4000000000, max_execution_time = 600`
}

// assetStatsSnapshotFills is the snapshot's fill: trustline assets, then
// native XLM from account entries — the two populations holdersBoardSteps
// ranks, without its positive-balance filter so trustlines counts zero ones.
func assetStatsSnapshotFills(cycleAt time.Time) []string {
	at := "'" + cycleAt.UTC().Format(holdersRollupTimeLayout) + "', 'UTC'"
	day := cycleAt.UTC().Format("2006-01-02")
	return []string{
		`TRUNCATE TABLE stellar.asset_stats_daily_staging`,
		assetStatsSnapshotFill(at, day, `
	         SELECT asset, balance,
	                row_number() OVER (PARTITION BY asset ORDER BY balance DESC, account_id) AS rank
	         FROM stellar.ledger_entries_current FINAL
	         WHERE entry_type = 'trustline' AND change_type != 'removed'`),
		assetStatsSnapshotFill(at, day, `
	         SELECT 'native' AS asset, balance,
	                row_number() OVER (ORDER BY balance DESC, account_id) AS rank
	         FROM stellar.ledger_entries_current FINAL
	         WHERE entry_type = 'account' AND change_type != 'removed'`),
	}
}

// assetStatsSnapshotReplace swaps the recomputed day in whole, so re-running
// a day is idempotent and a reader never sees half a day.
func assetStatsSnapshotReplace(cycleAt time.Time) string {
	return "ALTER TABLE stellar.asset_stats_daily REPLACE PARTITION '" +
		cycleAt.UTC().Format("2006-01-02") + "' FROM stellar.asset_stats_daily_staging"
}

// runAssetStatsSnapshot records the cycle's per-asset snapshot for its UTC
// day. It runs after the board swap so a failure here never holds back the
// boards, and behind the same shrink guard so a truncated scan cannot
// overwrite a good day.
func runAssetStatsSnapshot(ctx context.Context, conn holdersRollupConn, cycleAt time.Time, logf func(format string, args ...any)) error {
	fills := assetStatsSnapshotFills(cycleAt)
	total := len(fills) + 1
	for i, stmt := range fills {
		if err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("clickhouse: asset stats snapshot step %d/%d: %w", i+1, total, err)
		}
		logf("asset stats snapshot step %d/%d done", i+1, total)
	}
	if err := shrinkGuardCompare(ctx, conn, "stellar.asset_stats_daily_staging", assetStatsDailyLatestDay); err != nil {
		return err
	}
	if err := conn.Exec(ctx, assetStatsSnapshotReplace(cycleAt)); err != nil {
		return fmt.Errorf("clickhouse: asset stats snapshot step %d/%d: %w", total, total, err)
	}
	logf("asset stats snapshot step %d/%d done", total, total)
	return nil
}
