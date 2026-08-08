package clickhouse

import (
	"context"
	"fmt"
)

// holdersRollupTopN is how deep each asset's precomputed board goes —
// the API clamps requests to 500, so 500 covers every servable page.
const holdersRollupTopN = 500

// holdersRollupStatements is the full recompute cycle: truncate staging,
// fill both arms (trustline assets + native XLM from account entries),
// fill counts, then atomically exchange live↔staging. The two FINAL
// scans here are the SAME reads AssetHolders used to run per-request
// (inventory #4) — now once per cycle.
var holdersRollupStatements = []string{
	`TRUNCATE TABLE stellar.asset_holders_rollup_staging`,
	`TRUNCATE TABLE stellar.asset_holders_counts_staging`,
	// Trustline assets: per-asset top-N by balance.
	`INSERT INTO stellar.asset_holders_rollup_staging (asset, rank, account_id, balance)
	 SELECT asset, rank, account_id, balance FROM (
	     SELECT asset, account_id, balance,
	            row_number() OVER (PARTITION BY asset ORDER BY balance DESC, account_id) AS rank
	     FROM stellar.ledger_entries_current FINAL
	     WHERE entry_type = 'trustline' AND change_type != 'removed' AND balance > 0
	 ) WHERE rank <= ` + fmt.Sprint(holdersRollupTopN) + `
	 SETTINGS max_threads = 4, max_memory_usage = 8589934592,
	          max_bytes_before_external_group_by = 4000000000, max_bytes_before_external_sort = 4000000000, max_execution_time = 600`,
	// Native XLM: every account holds it in its AccountEntry.
	`INSERT INTO stellar.asset_holders_rollup_staging (asset, rank, account_id, balance)
	 SELECT 'native', rank, account_id, balance FROM (
	     SELECT account_id, balance,
	            row_number() OVER (ORDER BY balance DESC, account_id) AS rank
	     FROM stellar.ledger_entries_current FINAL
	     WHERE entry_type = 'account' AND change_type != 'removed' AND balance > 0
	 ) WHERE rank <= ` + fmt.Sprint(holdersRollupTopN) + `
	 SETTINGS max_threads = 4, max_memory_usage = 8589934592,
	          max_bytes_before_external_sort = 4000000000, max_execution_time = 600`,
	`INSERT INTO stellar.asset_holders_counts_staging (asset, holders)
	 SELECT asset, toInt64(count())
	 FROM stellar.ledger_entries_current FINAL
	 WHERE entry_type = 'trustline' AND change_type != 'removed' AND balance > 0
	 GROUP BY asset
	 SETTINGS max_threads = 4, max_memory_usage = 8589934592,
	          max_bytes_before_external_group_by = 4000000000, max_execution_time = 600`,
	`INSERT INTO stellar.asset_holders_counts_staging (asset, holders)
	 SELECT 'native', toInt64(count())
	 FROM stellar.ledger_entries_current FINAL
	 WHERE entry_type = 'account' AND change_type != 'removed' AND balance > 0
	 SETTINGS max_threads = 4, max_memory_usage = 8589934592, max_execution_time = 600`,
	// ── accounts analytics (deploy/clickhouse/accounts_stats_rollup.sql)
	// — ride the same cycle; the holders statements above already paid
	// for the FINAL scans' page cache. top100 reads the STAGING board
	// (filled earlier in this cycle — statement order is load-bearing).
	`TRUNCATE TABLE stellar.accounts_stats_staging`,
	`TRUNCATE TABLE stellar.accounts_wealth_histogram_staging`,
	`TRUNCATE TABLE stellar.accounts_trustline_histogram_staging`,
	`INSERT INTO stellar.accounts_stats_staging (metric, value)
	 SELECT metric, value FROM (
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
	          max_bytes_before_external_group_by = 4000000000, max_execution_time = 600`,
	`INSERT INTO stellar.accounts_wealth_histogram_staging (bucket, accounts, xlm_stroops)
	 SELECT toInt8(least(greatest(floor(log10(balance / 10000000.0)), -1), 10)) AS bucket,
	        toUInt64(count()), toInt64(sum(balance))
	 FROM stellar.ledger_entries_current FINAL
	 WHERE entry_type = 'account' AND change_type != 'removed' AND balance > 0
	 GROUP BY bucket
	 SETTINGS max_threads = 4, max_memory_usage = 8589934592,
	          max_bytes_before_external_group_by = 4000000000, max_execution_time = 600`,
	`INSERT INTO stellar.accounts_trustline_histogram_staging (bucket, accounts)
	 SELECT multiIf(c = 1, '1', c <= 5, '2-5', c <= 10, '6-10', c <= 50, '11-50', '50+') AS bucket,
	        toUInt64(count())
	 FROM (
	     SELECT account_id, count() AS c
	     FROM stellar.ledger_entries_current FINAL
	     WHERE entry_type = 'trustline' AND change_type != 'removed'
	     GROUP BY account_id
	 )
	 GROUP BY bucket
	 SETTINGS max_threads = 4, max_memory_usage = 8589934592,
	          max_bytes_before_external_group_by = 4000000000, max_execution_time = 600`,
	`EXCHANGE TABLES stellar.asset_holders_rollup_staging AND stellar.asset_holders_rollup`,
	`EXCHANGE TABLES stellar.asset_holders_counts_staging AND stellar.asset_holders_counts`,
	`EXCHANGE TABLES stellar.accounts_stats_staging AND stellar.accounts_stats`,
	`EXCHANGE TABLES stellar.accounts_wealth_histogram_staging AND stellar.accounts_wealth_histogram`,
	`EXCHANGE TABLES stellar.accounts_trustline_histogram_staging AND stellar.accounts_trustline_histogram`,
}

// RunHoldersRollup executes one full recompute + atomic exchange.
func RunHoldersRollup(ctx context.Context, addr string, logf func(format string, args ...any)) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	for i, stmt := range holdersRollupStatements {
		if err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("clickhouse: holders rollup step %d/%d: %w", i+1, len(holdersRollupStatements), err)
		}
		logf("step %d/%d done", i+1, len(holdersRollupStatements))
	}
	return nil
}

// holdersRollupBoard is AssetHolders' precomputed fast path: keyed
// sub-millisecond reads off the rollup tables. Returns ok=false when the
// board can't answer (probe says the rollup is unavailable) — caller
// falls back to the legacy per-request scans.
func (r *ExplorerReader) holdersRollupBoard(ctx context.Context, asset string, limit int) ([]AssetHolder, int64, bool, error) {
	if !r.probeSchema(ctx, &r.holdersRollupProbe,
		`SELECT rank FROM stellar.asset_holders_rollup LIMIT 1`, true) {
		return nil, 0, false, nil
	}
	rows, err := r.conn.Query(ctx, `
		SELECT account_id, balance FROM stellar.asset_holders_rollup
		WHERE asset = ? ORDER BY rank LIMIT ?`, asset, limit)
	if err != nil {
		return nil, 0, false, fmt.Errorf("clickhouse: holders rollup read: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AssetHolder
	for rows.Next() {
		var h AssetHolder
		if err := rows.Scan(&h.AccountID, &h.Balance); err != nil {
			return nil, 0, false, fmt.Errorf("clickhouse: scan rollup holder: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, err
	}
	var total int64
	// max() collapses the (impossible-by-design, but cheap to be safe)
	// multi-row case; a missing row scans to 0 — authoritative under the
	// exchange contract: a completed cycle materializes EVERY asset with
	// a positive-balance holder.
	if err := r.conn.QueryRow(ctx, `
		SELECT toInt64(max(holders)) FROM stellar.asset_holders_counts WHERE asset = ?`, asset).Scan(&total); err != nil {
		return nil, 0, false, fmt.Errorf("clickhouse: holders rollup count: %w", err)
	}
	return out, total, true, nil
}
