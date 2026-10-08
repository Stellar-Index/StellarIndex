package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// holdersRollupTopN is how deep each asset's precomputed board goes —
// the API clamps requests to 500, so 500 covers every servable page.
const holdersRollupTopN = 500

// holdersRollupTimeLayout is the ClickHouse DateTime literal format for the cycle stamp.
const holdersRollupTimeLayout = "2006-01-02 15:04:05"

// holdersRollupExchangeStatement is the multi-pair EXCHANGE TABLES swapping all five live
// tables as one metadata transaction, so a crash cannot leave the board new and counts old.
// Always the last statement; every staging arm must be filled first.
const holdersRollupExchangeStatement = `EXCHANGE TABLES stellar.asset_holders_rollup_staging AND stellar.asset_holders_rollup,
                 stellar.asset_holders_counts_staging AND stellar.asset_holders_counts,
                 stellar.accounts_stats_staging AND stellar.accounts_stats,
                 stellar.accounts_wealth_histogram_staging AND stellar.accounts_wealth_histogram,
                 stellar.accounts_trustline_histogram_staging AND stellar.accounts_trustline_histogram`

// holdersRollupStatements builds one recompute cycle: truncate staging, fill both arms
// (trustline assets + native XLM), counts and accounts-analytics, then exchange.
//
// Every INSERT stamps computed_at with cycleAt, not each table's `DEFAULT now()`, so all
// five tables share one stamp; readers compare it across round trips to detect a swap
// landing mid-read.
func holdersRollupStatements(cycleAt time.Time) []string {
	// Explicit 'UTC': a bare toDateTime(literal) uses the server timezone and would skew the
	// age holdersRollupFresh gates on.
	at := "'" + cycleAt.UTC().Format(holdersRollupTimeLayout) + "', 'UTC'"
	stmts := []string{
		`TRUNCATE TABLE stellar.asset_holders_rollup_staging`,
		`TRUNCATE TABLE stellar.asset_holders_counts_staging`,
	}
	stmts = append(stmts, holdersBoardSteps(at)...)
	stmts = append(stmts, holdersCountSteps(at)...)
	stmts = append(stmts,
		// ── accounts analytics (deploy/clickhouse/accounts_stats_rollup.sql): same cycle.
		// top100 reads the STAGING board filled earlier, so statement order is load-bearing.
		`TRUNCATE TABLE stellar.accounts_stats_staging`,
		`TRUNCATE TABLE stellar.accounts_wealth_histogram_staging`,
		`TRUNCATE TABLE stellar.accounts_trustline_histogram_staging`,
	)
	stmts = append(stmts, accountsStatsStep(at))
	stmts = append(stmts, accountsHistogramSteps(at)...)
	return append(stmts, holdersRollupExchangeStatement)
}

// holdersBoardSteps is the two FINAL scans AssetHolders would run per request (trustline
// assets' per-asset top-N, and native XLM), run once per cycle instead.
func holdersBoardSteps(at string) []string {
	return []string{
		`INSERT INTO stellar.asset_holders_rollup_staging (asset, rank, account_id, balance, computed_at)
		 SELECT asset, rank, account_id, balance, toDateTime(` + at + `) FROM (
		     SELECT asset, account_id, balance,
		            row_number() OVER (PARTITION BY asset ORDER BY balance DESC, account_id) AS rank
		     FROM stellar.ledger_entries_current FINAL
		     WHERE entry_type = 'trustline' AND change_type != 'removed' AND balance > 0
		 ) WHERE rank <= ` + fmt.Sprint(holdersRollupTopN) + `
		 SETTINGS max_threads = 4, max_memory_usage = 8589934592,
		          max_bytes_before_external_group_by = 4000000000, max_bytes_before_external_sort = 4000000000, max_execution_time = 600`,
		`INSERT INTO stellar.asset_holders_rollup_staging (asset, rank, account_id, balance, computed_at)
		 SELECT 'native', rank, account_id, balance, toDateTime(` + at + `) FROM (
		     SELECT account_id, balance,
		            row_number() OVER (ORDER BY balance DESC, account_id) AS rank
		     FROM stellar.ledger_entries_current FINAL
		     WHERE entry_type = 'account' AND change_type != 'removed' AND balance > 0
		 ) WHERE rank <= ` + fmt.Sprint(holdersRollupTopN) + `
		 SETTINGS max_threads = 4, max_memory_usage = 8589934592,
		          max_bytes_before_external_sort = 4000000000, max_execution_time = 600`,
	}
}

// holdersCountSteps fills the per-asset holder counts alongside the board.
func holdersCountSteps(at string) []string {
	return []string{
		`INSERT INTO stellar.asset_holders_counts_staging (asset, holders, computed_at)
		 SELECT asset, toInt64(count()), toDateTime(` + at + `)
		 FROM stellar.ledger_entries_current FINAL
		 WHERE entry_type = 'trustline' AND change_type != 'removed' AND balance > 0
		 GROUP BY asset
		 SETTINGS max_threads = 4, max_memory_usage = 8589934592,
		          max_bytes_before_external_group_by = 4000000000, max_execution_time = 600`,
		`INSERT INTO stellar.asset_holders_counts_staging (asset, holders, computed_at)
		 SELECT 'native', toInt64(count()), toDateTime(` + at + `)
		 FROM stellar.ledger_entries_current FINAL
		 WHERE entry_type = 'account' AND change_type != 'removed' AND balance > 0
		 SETTINGS max_threads = 4, max_memory_usage = 8589934592, max_execution_time = 600`,
	}
}

// accountsStatsStep fills the metric-keyed accounts_stats board (one row
// per metric — adding a metric is an INSERT, not a schema migration).
func accountsStatsStep(at string) string {
	return `INSERT INTO stellar.accounts_stats_staging (metric, value, computed_at)
		 SELECT metric, value, toDateTime(` + at + `) FROM (
		     SELECT 'total_accounts' AS metric, toInt64(count()) AS value
		     FROM stellar.ledger_entries_current FINAL
		     WHERE entry_type = 'account' AND change_type != 'removed' AND balance > 0
		     UNION ALL
		     SELECT 'xlm_total_stroops', toInt64(sum(balance))
		     FROM stellar.ledger_entries_current FINAL
		     WHERE entry_type = 'account' AND change_type != 'removed' AND balance > 0
		     UNION ALL
		     SELECT 'avg_stroops', toInt64(avg(balance))
		     FROM stellar.ledger_entries_current FINAL
		     WHERE entry_type = 'account' AND change_type != 'removed' AND balance > 0
		     UNION ALL
		     SELECT 'median_stroops', toInt64(quantile(0.5)(balance))
		     FROM stellar.ledger_entries_current FINAL
		     WHERE entry_type = 'account' AND change_type != 'removed' AND balance > 0
		     UNION ALL
		     SELECT 'p90_stroops', toInt64(quantile(0.9)(balance))
		     FROM stellar.ledger_entries_current FINAL
		     WHERE entry_type = 'account' AND change_type != 'removed' AND balance > 0
		     UNION ALL
		     SELECT 'p99_stroops', toInt64(quantile(0.99)(balance))
		     FROM stellar.ledger_entries_current FINAL
		     WHERE entry_type = 'account' AND change_type != 'removed' AND balance > 0
		     UNION ALL
		     SELECT 'total_trustlines', toInt64(count())
		     FROM stellar.ledger_entries_current FINAL
		     WHERE entry_type = 'trustline' AND change_type != 'removed'
		     UNION ALL
		     SELECT 'trustline_holding_accounts', toInt64(uniqExact(account_id))
		     FROM stellar.ledger_entries_current FINAL
		     WHERE entry_type = 'trustline' AND change_type != 'removed'
		     UNION ALL
		     SELECT 'top100_xlm_stroops', toInt64(sum(balance))
		     FROM stellar.asset_holders_rollup_staging
		     WHERE asset = 'native' AND rank <= 100
		 )
		 SETTINGS max_threads = 4, max_memory_usage = 8589934592,
		          max_bytes_before_external_group_by = 4000000000, max_execution_time = 600`
}

// accountsHistogramSteps fills the wealth and trustline-count histograms.
func accountsHistogramSteps(at string) []string {
	return []string{
		`INSERT INTO stellar.accounts_wealth_histogram_staging (bucket, accounts, xlm_stroops, computed_at)
		 SELECT toInt8(least(greatest(floor(log10(balance / 10000000.0)), -1), 10)) AS bucket,
		        toUInt64(count()), toInt64(sum(balance)), toDateTime(` + at + `)
		 FROM stellar.ledger_entries_current FINAL
		 WHERE entry_type = 'account' AND change_type != 'removed' AND balance > 0
		 GROUP BY bucket
		 SETTINGS max_threads = 4, max_memory_usage = 8589934592,
		          max_bytes_before_external_group_by = 4000000000, max_execution_time = 600`,
		`INSERT INTO stellar.accounts_trustline_histogram_staging (bucket, accounts, computed_at)
		 SELECT multiIf(c = 1, '1', c <= 5, '2-5', c <= 10, '6-10', c <= 50, '11-50', '50+') AS bucket,
		        toUInt64(count()), toDateTime(` + at + `)
		 FROM (
		     SELECT account_id, count() AS c
		     FROM stellar.ledger_entries_current FINAL
		     WHERE entry_type = 'trustline' AND change_type != 'removed'
		     GROUP BY account_id
		 )
		 GROUP BY bucket
		 SETTINGS max_threads = 4, max_memory_usage = 8589934592,
		          max_bytes_before_external_group_by = 4000000000, max_execution_time = 600`,
	}
}

// holdersRollupShrinkGuardMinRatio is the floor a staging arm's row count may fall to
// relative to the live table before RunHoldersRollup refuses to publish. EXCHANGE only makes
// the swap atomic; a FINAL scan can finish with far fewer rows and no error (mid-flight merge,
// max_execution_time truncation). Holder counts move gradually, so a healthy cycle never trips it.
const holdersRollupShrinkGuardMinRatio = 0.5

// holdersRollupShrinkGuardTables pairs each staging arm the swap is about to
// publish with the live table it will replace.
var holdersRollupShrinkGuardTables = [][2]string{
	{"stellar.asset_holders_rollup_staging", "stellar.asset_holders_rollup"},
	{"stellar.asset_holders_counts_staging", "stellar.asset_holders_counts"},
}

// holdersRollupConn is what one cycle needs from a connection (Exec, and QueryRow for the
// shrink guard), so it is drivable without ClickHouse.
type holdersRollupConn interface {
	Exec(ctx context.Context, query string, args ...any) error
	QueryRow(ctx context.Context, query string, args ...any) driver.Row
}

// holdersRollupShrinkGuard aborts the cycle, leaving the previous one live, when a staging
// arm shrank below holdersRollupShrinkGuardMinRatio of its live counterpart. A live count of
// zero (first cycle) has nothing to compare and is skipped.
func holdersRollupShrinkGuard(ctx context.Context, conn holdersRollupConn) error {
	for _, pair := range holdersRollupShrinkGuardTables {
		if err := shrinkGuardCompare(ctx, conn, pair[0], pair[1]); err != nil {
			return err
		}
	}
	return nil
}

// shrinkGuardCompare applies holdersRollupShrinkGuardMinRatio to one staging
// arm against the row set it would replace; live is counted as a FROM clause.
func shrinkGuardCompare(ctx context.Context, conn holdersRollupConn, staging, live string) error {
	var stagingCount, liveCount uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM "+staging).Scan(&stagingCount); err != nil {
		return fmt.Errorf("clickhouse: holders rollup shrink guard: count %s: %w", staging, err)
	}
	if err := conn.QueryRow(ctx, "SELECT count() FROM "+live).Scan(&liveCount); err != nil {
		return fmt.Errorf("clickhouse: holders rollup shrink guard: count %s: %w", live, err)
	}
	if liveCount == 0 {
		return nil
	}
	if float64(stagingCount) < float64(liveCount)*holdersRollupShrinkGuardMinRatio {
		return fmt.Errorf("clickhouse: holders rollup shrink guard: %s has %d row(s), down from %d live in %s (floor %.0f%% of live) — refusing to publish, previous cycle stays live",
			staging, stagingCount, liveCount, live, holdersRollupShrinkGuardMinRatio*100)
	}
	return nil
}

// RunHoldersRollup executes one full recompute + atomic exchange.
func RunHoldersRollup(ctx context.Context, addr string, logf func(format string, args ...any)) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return runHoldersRollupSteps(ctx, conn, logf)
}

// runHoldersRollupSteps runs the fills, the shrink guard (after every arm is filled, before
// the swap, so a broken cycle leaves the previous one live), the swap, then the daily
// snapshot, on an open connection; split out so tests need no ClickHouse.
func runHoldersRollupSteps(ctx context.Context, conn holdersRollupConn, logf func(format string, args ...any)) error {
	cycleAt := time.Now()
	if err := runHoldersRollupSwap(ctx, conn, cycleAt, logf); err != nil {
		return err
	}
	return runAssetStatsSnapshot(ctx, conn, cycleAt, logf)
}

func runHoldersRollupSwap(ctx context.Context, conn holdersRollupConn, cycleAt time.Time, logf func(format string, args ...any)) error {
	stmts := holdersRollupStatements(cycleAt)
	total := len(stmts)
	fill, exchange := stmts[:total-1], stmts[total-1]
	for i, stmt := range fill {
		if err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("clickhouse: holders rollup step %d/%d: %w", i+1, total, err)
		}
		logf("step %d/%d done", i+1, total)
	}
	if err := holdersRollupShrinkGuard(ctx, conn); err != nil {
		return err
	}
	if err := conn.Exec(ctx, exchange); err != nil {
		return fmt.Errorf("clickhouse: holders rollup step %d/%d: %w", total, total, err)
	}
	logf("step %d/%d done", total, total)
	return nil
}

// holdersRollupMaxAge is the oldest cycle stamp holdersRollupBoard will serve: run + timer
// OnUnitInactiveSec=30min + RandomizedDelaySec=2min + next run. Runs under ~44 min stay
// inside it; slower ones (unit allows 80min) age the board out and readers fall back to scans.
const holdersRollupMaxAge = 2 * time.Hour

// holdersRollupBoard is AssetHolders' precomputed fast path (keyed sub-millisecond reads).
// ok=false when the rollup is unavailable or its stamp is older than holdersRollupMaxAge;
// the caller falls back to per-request scans, an honest slow answer over a fast stale one.
//
// The board and count tables are exchanged together but read in two round trips, so a swap
// between them could pair different cycles. Both carry the same computed_at stamp; a
// mismatch retries the pair once, as AccountsStats does.
func (r *ExplorerReader) holdersRollupBoard(ctx context.Context, asset string, limit int) ([]AssetHolder, int64, bool, error) {
	if !r.probeSchema(ctx, &r.holdersRollupProbe,
		`SELECT rank FROM stellar.asset_holders_rollup LIMIT 1`, true) {
		return nil, 0, false, nil
	}
	out, total, at, consistent, err := r.readHoldersRollupCycle(ctx, asset, limit)
	if err != nil {
		return nil, 0, false, err
	}
	if !consistent {
		out, total, at, _, err = r.readHoldersRollupCycle(ctx, asset, limit)
		if err != nil {
			return nil, 0, false, err
		}
	}
	fresh, err := r.holdersRollupFresh(ctx, at)
	if err != nil || !fresh {
		return nil, 0, false, err
	}
	return out, total, true, nil
}

// readHoldersRollupCycle reads the board, its count and each side's computed_at stamp, and
// reports whether both landed in the same cycle (see holdersRollupBoard). The returned stamp
// is the older one carrying a stamp; unstamped when the asset has no row in either table.
func (r *ExplorerReader) readHoldersRollupCycle(ctx context.Context, asset string, limit int) ([]AssetHolder, int64, time.Time, bool, error) {
	out, boardAt, err := r.readHoldersRollupRows(ctx, asset, limit)
	if err != nil {
		return nil, 0, time.Time{}, false, err
	}
	total, countAt, err := r.readHoldersRollupCount(ctx, asset)
	if err != nil {
		return nil, 0, time.Time{}, false, err
	}
	at := boardAt
	if !holdersRollupStamped(at) || (holdersRollupStamped(countAt) && countAt.Before(at)) {
		at = countAt
	}
	return out, total, at, boardAt.Equal(countAt), nil
}

// holdersRollupStamped reports whether a computed_at read carried a real
// cycle stamp: an empty board scans to the zero time, an empty max() to the
// Unix epoch.
func holdersRollupStamped(at time.Time) bool {
	return at.Unix() > 0
}

// holdersRollupFresh reports whether a board's cycle is young enough to serve. An asset in
// neither table has no stamp, yet "no row" is only authoritative for a current cycle, so read
// the live table's stamp, which every row of one cycle shares.
func (r *ExplorerReader) holdersRollupFresh(ctx context.Context, at time.Time) (bool, error) {
	if !holdersRollupStamped(at) {
		if err := r.conn.QueryRow(ctx, `
			SELECT max(computed_at) FROM (SELECT computed_at FROM stellar.asset_holders_rollup LIMIT 1)`).Scan(&at); err != nil {
			return false, fmt.Errorf("clickhouse: holders rollup cycle stamp: %w", err)
		}
	}
	return holdersRollupStamped(at) && time.Since(at) <= holdersRollupMaxAge, nil
}

func (r *ExplorerReader) readHoldersRollupRows(ctx context.Context, asset string, limit int) ([]AssetHolder, time.Time, error) {
	rows, err := r.conn.Query(ctx, `
		SELECT account_id, balance, computed_at FROM stellar.asset_holders_rollup
		WHERE asset = ? ORDER BY rank LIMIT ?`, asset, limit)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("clickhouse: holders rollup read: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AssetHolder
	var at time.Time
	for rows.Next() {
		var h AssetHolder
		var rowAt time.Time
		if err := rows.Scan(&h.AccountID, &h.Balance, &rowAt); err != nil {
			return nil, time.Time{}, fmt.Errorf("clickhouse: scan rollup holder: %w", err)
		}
		out = append(out, h)
		if rowAt.After(at) {
			at = rowAt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, err
	}
	return out, at, nil
}

func (r *ExplorerReader) readHoldersRollupCount(ctx context.Context, asset string) (int64, time.Time, error) {
	var total int64
	var at time.Time
	// max() collapses duplicate rows (impossible by design); a missing row scans to 0, which is
	// authoritative: a completed cycle materializes every asset with a positive-balance holder.
	if err := r.conn.QueryRow(ctx, `
		SELECT toInt64(max(holders)), max(computed_at) FROM stellar.asset_holders_counts WHERE asset = ?`, asset).Scan(&total, &at); err != nil {
		return 0, time.Time{}, fmt.Errorf("clickhouse: holders rollup count: %w", err)
	}
	return total, at, nil
}
