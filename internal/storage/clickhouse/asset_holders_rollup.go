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

// holdersRollupTimeLayout is the ClickHouse DateTime literal format used to
// bake a single cycle stamp into every staging INSERT below.
const holdersRollupTimeLayout = "2006-01-02 15:04:05"

// holdersRollupExchangeStatement is the final, multi-pair EXCHANGE TABLES
// statement. Swaps all five live tables atomically as a group (RA-2).
// ClickHouse commits a multi-pair EXCHANGE TABLES as one metadata
// transaction, so a crash / ctx-cancel / CH restart cannot land midway and
// leave the board swapped-new while counts/stats/histograms hold the
// previous cycle's data. It is always the LAST statement holdersRollupStatements
// returns — every staging arm must be filled before the group swap fires.
const holdersRollupExchangeStatement = `EXCHANGE TABLES stellar.asset_holders_rollup_staging AND stellar.asset_holders_rollup,
                 stellar.asset_holders_counts_staging AND stellar.asset_holders_counts,
                 stellar.accounts_stats_staging AND stellar.accounts_stats,
                 stellar.accounts_wealth_histogram_staging AND stellar.accounts_wealth_histogram,
                 stellar.accounts_trustline_histogram_staging AND stellar.accounts_trustline_histogram`

// holdersRollupStatements builds the full recompute cycle for one run:
// truncate staging, fill both arms (trustline assets + native XLM from
// account entries), fill counts, fill the accounts-analytics tables that
// ride the same cycle, then atomically exchange live↔staging.
//
// Every staging INSERT stamps computed_at with cycleAt explicitly, rather
// than each table's own `DEFAULT now()` (which would give each of the six
// inserts below a slightly different timestamp, since they run one after
// another). All five live tables carry the SAME computed_at once this cycle
// swaps in — the cycle stamp holdersRollupBoard and AccountsStats compare
// across their separate read round trips to detect a swap landing mid-read
// (T346/T361): each read's own timestamps are internally consistent within
// a cycle, but only a SHARED stamp lets a reader detect that two of its
// queries landed in different cycles.
func holdersRollupStatements(cycleAt time.Time) []string {
	at := "'" + cycleAt.UTC().Format(holdersRollupTimeLayout) + "'"
	stmts := []string{
		`TRUNCATE TABLE stellar.asset_holders_rollup_staging`,
		`TRUNCATE TABLE stellar.asset_holders_counts_staging`,
	}
	stmts = append(stmts, holdersBoardSteps(at)...)
	stmts = append(stmts, holdersCountSteps(at)...)
	stmts = append(stmts,
		// ── accounts analytics (deploy/clickhouse/accounts_stats_rollup.sql)
		// — ride the same cycle; the holders statements above already paid
		// for the FINAL scans' page cache. top100 reads the STAGING board
		// (filled earlier in this cycle — statement order is load-bearing).
		`TRUNCATE TABLE stellar.accounts_stats_staging`,
		`TRUNCATE TABLE stellar.accounts_wealth_histogram_staging`,
		`TRUNCATE TABLE stellar.accounts_trustline_histogram_staging`,
	)
	stmts = append(stmts, accountsStatsStep(at))
	stmts = append(stmts, accountsHistogramSteps(at)...)
	return append(stmts, holdersRollupExchangeStatement)
}

// holdersBoardSteps is the two FINAL scans AssetHolders used to run
// per-request (inventory #4) — trustline assets' per-asset top-N by
// balance, and native XLM (every account holds it in its AccountEntry) —
// now run once per cycle instead.
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

// holdersRollupShrinkGuardMinRatio is the floor a staging arm's row count may
// fall to relative to what is CURRENTLY live before RunHoldersRollup refuses
// to publish it. EXCHANGE TABLES only guarantees the SWAP is atomic (RA-2);
// it has no opinion on what it is swapping in. ClickHouse can finish a FINAL
// scan that read far fewer rows than a healthy cycle without returning any
// error — a merge left mid-flight, a scan silently truncated by
// max_execution_time on a subset of parts — so a row-count check between the
// fills and the swap is the only thing standing between a broken cycle and a
// board published as authoritative. Holder counts move gradually cycle to
// cycle (30 min); halving would itself be page-one pubnet news, not a normal
// rollup, so this floor never fires on a healthy chain.
const holdersRollupShrinkGuardMinRatio = 0.5

// holdersRollupShrinkGuardTables pairs each staging arm the swap is about to
// publish with the live table it will replace.
var holdersRollupShrinkGuardTables = [][2]string{
	{"stellar.asset_holders_rollup_staging", "stellar.asset_holders_rollup"},
	{"stellar.asset_holders_counts_staging", "stellar.asset_holders_counts"},
}

// holdersRollupConn is what one holders-rollup cycle needs from a
// connection: Exec for every statement, QueryRow for
// holdersRollupShrinkGuard. Named so the cycle is drivable without a
// ClickHouse connection — the same split runRollupCycle/runRollupSteps use.
type holdersRollupConn interface {
	Exec(ctx context.Context, query string, args ...any) error
	QueryRow(ctx context.Context, query string, args ...any) driver.Row
}

// holdersRollupShrinkGuard aborts the cycle — leaving the previous (good)
// cycle live — when a staging arm has shrunk by more than
// holdersRollupShrinkGuardMinRatio relative to its live counterpart. A live
// count of zero (first cycle ever, or a not-yet-populated deployment) has
// nothing to compare against and is skipped rather than treated as a shrink.
func holdersRollupShrinkGuard(ctx context.Context, conn holdersRollupConn) error {
	for _, pair := range holdersRollupShrinkGuardTables {
		staging, live := pair[0], pair[1]
		var stagingCount, liveCount uint64
		if err := conn.QueryRow(ctx, "SELECT count() FROM "+staging).Scan(&stagingCount); err != nil {
			return fmt.Errorf("clickhouse: holders rollup shrink guard: count %s: %w", staging, err)
		}
		if err := conn.QueryRow(ctx, "SELECT count() FROM "+live).Scan(&liveCount); err != nil {
			return fmt.Errorf("clickhouse: holders rollup shrink guard: count %s: %w", live, err)
		}
		if liveCount == 0 {
			continue
		}
		if float64(stagingCount) < float64(liveCount)*holdersRollupShrinkGuardMinRatio {
			return fmt.Errorf("clickhouse: holders rollup shrink guard: %s has %d row(s), down from %d live in %s (floor %.0f%% of live) — refusing to publish, previous cycle stays live",
				staging, stagingCount, liveCount, live, holdersRollupShrinkGuardMinRatio*100)
		}
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

// runHoldersRollupSteps runs the fills, then holdersRollupShrinkGuard, then
// the swap, against an already-open connection — split out from
// RunHoldersRollup so the cycle is drivable in a test without dialing
// ClickHouse. holdersRollupShrinkGuard runs after every staging arm is
// filled and before the swap, so a broken cycle errors out with the
// previous cycle left live rather than publishing a degraded board.
func runHoldersRollupSteps(ctx context.Context, conn holdersRollupConn, logf func(format string, args ...any)) error {
	stmts := holdersRollupStatements(time.Now())
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

// holdersRollupBoard is AssetHolders' precomputed fast path: keyed
// sub-millisecond reads off the rollup tables. Returns ok=false when the
// board can't answer (probe says the rollup is unavailable) — caller
// falls back to the legacy per-request scans.
//
// asset_holders_rollup and asset_holders_counts are exchanged together as
// part of RA-2's five-table atomic group, but read here as two independent
// round trips — a swap landing between them serves a board from one cycle
// paired with a count from another (T361). Both tables carry the same
// computed_at cycle stamp once a swap lands (holdersRollupStatements), so
// comparing the two reads' stamps detects that; a mismatch retries the pair
// once, matching AccountsStats' consistency check (T346).
func (r *ExplorerReader) holdersRollupBoard(ctx context.Context, asset string, limit int) ([]AssetHolder, int64, bool, error) {
	if !r.probeSchema(ctx, &r.holdersRollupProbe,
		`SELECT rank FROM stellar.asset_holders_rollup LIMIT 1`, true) {
		return nil, 0, false, nil
	}
	out, total, consistent, err := r.readHoldersRollupCycle(ctx, asset, limit)
	if err != nil {
		return nil, 0, false, err
	}
	if !consistent {
		out, total, _, err = r.readHoldersRollupCycle(ctx, asset, limit)
		if err != nil {
			return nil, 0, false, err
		}
	}
	return out, total, true, nil
}

// readHoldersRollupCycle reads the board and its count, plus each side's
// computed_at cycle stamp, and reports whether the two round trips landed in
// the same swap cycle (see holdersRollupBoard).
func (r *ExplorerReader) readHoldersRollupCycle(ctx context.Context, asset string, limit int) ([]AssetHolder, int64, bool, error) {
	out, boardAt, err := r.readHoldersRollupRows(ctx, asset, limit)
	if err != nil {
		return nil, 0, false, err
	}
	total, countAt, err := r.readHoldersRollupCount(ctx, asset)
	if err != nil {
		return nil, 0, false, err
	}
	return out, total, boardAt.Equal(countAt), nil
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
	// max() collapses the (impossible-by-design, but cheap to be safe)
	// multi-row case; a missing row scans to 0 — authoritative under the
	// exchange contract: a completed cycle materializes EVERY asset with
	// a positive-balance holder.
	if err := r.conn.QueryRow(ctx, `
		SELECT toInt64(max(holders)), max(computed_at) FROM stellar.asset_holders_counts WHERE asset = ?`, asset).Scan(&total, &at); err != nil {
		return 0, time.Time{}, fmt.Errorf("clickhouse: holders rollup count: %w", err)
	}
	return total, at, nil
}
