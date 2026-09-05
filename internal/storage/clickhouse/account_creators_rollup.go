package clickhouse

import (
	"context"
	"fmt"
	"math/big"
	"time"
)

// AccountCreatorRow is one row of the account-creator league table: a
// funder and the accounts it brought into existence (#351).
//
// Two kinds of figure live here and they answer different questions.
// AccountsCreated / FundedStroops / the ledger bounds are IMMUTABLE
// history — a creation never un-happens, so those only grow.
// LiveAccounts / LiveStroops are POINT-IN-TIME: created accounts merge
// away and balances move, so they describe the created set as of the
// cycle that computed them, not as of creation.
type AccountCreatorRow struct {
	Rank            uint32
	Creator         string
	AccountsCreated uint64
	// FundedStroops is the sum of starting balances. Zero is a real,
	// common value: CAP-33 sponsored reserves let an account be created
	// with no XLM of its own.
	FundedStroops  *big.Int
	LiveAccounts   uint64
	LiveStroops    *big.Int
	FirstLedger    uint32
	LastLedger     uint32
	FirstCreatedAt time.Time
	LastCreatedAt  time.Time
}

// AccountCreators is one cycle's snapshot: the requested slice of the
// board plus the totals and the ledger span the cycle actually
// aggregated.
//
// FromLedger/ThruLedger are DATA-DERIVED (ADR-0031) — min/max over the
// creation rows the cycle read, never a constant and never genesis by
// assumption. The API serves them so a caller can see the span the
// numbers cover instead of inferring the whole chain.
type AccountCreators struct {
	Board             []AccountCreatorRow
	CreatorsTotal     int64
	CreationsTotal    int64
	LiveAccountsTotal int64
	FromLedger        uint32
	ThruLedger        uint32
	FromTime          time.Time
	ThruTime          time.Time
	ComputedAt        time.Time
}

// creatorsRollupStatements is the full recompute cycle: truncate both
// staging arms, aggregate the board, derive the stats FROM that board,
// then swap the pair atomically.
//
// Deriving the stats from the staging board rather than from a second
// scan is what makes the served coverage span honest by construction:
// the totals and the span describe exactly the rows the board was built
// from, so the two cannot drift apart.
//
// The dedupe arm reproduces ReplacingMergeTree semantics explicitly
// (argMax over ingested_at, grouped by the table's full ORDER BY key)
// rather than using FINAL, which on a 10-billion-row archive would pay
// merge-on-read for the whole table instead of for the ~32M rows that
// survive the movement_kind filter.
var creatorsRollupStatements = []string{
	`TRUNCATE TABLE stellar.account_creators_rollup_staging`,
	`TRUNCATE TABLE stellar.account_creators_stats_staging`,
	`INSERT INTO stellar.account_creators_rollup_staging
	     (rank, creator, accounts_created, funded_stroops, live_accounts, live_stroops,
	      first_ledger, last_ledger, first_created_at, last_created_at)
	 SELECT row_number() OVER (ORDER BY accounts_created DESC, creator) AS rank,
	        creator, accounts_created, funded_stroops, live_accounts, live_stroops,
	        first_ledger, last_ledger, first_created_at, last_created_at
	 FROM (
	     SELECT c.creator AS creator,
	            toUInt64(count()) AS accounts_created,
	            toInt128(sum(c.amount)) AS funded_stroops,
	            toUInt64(countIf(e.account_id != '')) AS live_accounts,
	            toInt128(sum(e.balance)) AS live_stroops,
	            min(c.ledger) AS first_ledger,
	            max(c.ledger) AS last_ledger,
	            toDateTime(min(c.closed_at), 'UTC') AS first_created_at,
	            toDateTime(max(c.closed_at), 'UTC') AS last_created_at
	     FROM (
	         SELECT address AS creator,
	                argMax(counterparty, ingested_at) AS created,
	                argMax(amount, ingested_at) AS amount,
	                argMax(ledger_close_time, ingested_at) AS closed_at,
	                ledger
	         FROM stellar.account_movements
	         WHERE movement_kind = 'create_account' AND direction = 'sent'
	         GROUP BY address, ledger, tx_hash, op_index, leg_index, direction
	     ) AS c
	     LEFT JOIN (
	         SELECT account_id, balance
	         FROM stellar.ledger_entries_current FINAL
	         WHERE entry_type = 'account' AND change_type != 'removed'
	     ) AS e ON c.created = e.account_id
	     GROUP BY creator
	 )
	 SETTINGS max_threads = 4, max_memory_usage = 8589934592,
	          max_bytes_before_external_group_by = 4000000000,
	          max_bytes_before_external_sort = 4000000000, max_execution_time = 3600`,
	`INSERT INTO stellar.account_creators_stats_staging (metric, value)
	 SELECT metric, value FROM (
	     SELECT 'creators_total' AS metric, toInt64(count()) AS value
	     FROM stellar.account_creators_rollup_staging
	     UNION ALL
	     SELECT 'creations_total', toInt64(sum(accounts_created))
	     FROM stellar.account_creators_rollup_staging
	     UNION ALL
	     SELECT 'live_accounts_total', toInt64(sum(live_accounts))
	     FROM stellar.account_creators_rollup_staging
	     UNION ALL
	     SELECT 'from_ledger', toInt64(min(first_ledger))
	     FROM stellar.account_creators_rollup_staging
	     UNION ALL
	     SELECT 'thru_ledger', toInt64(max(last_ledger))
	     FROM stellar.account_creators_rollup_staging
	     UNION ALL
	     SELECT 'from_time', toInt64(toUnixTimestamp(min(first_created_at)))
	     FROM stellar.account_creators_rollup_staging
	     UNION ALL
	     SELECT 'thru_time', toInt64(toUnixTimestamp(max(last_created_at)))
	     FROM stellar.account_creators_rollup_staging
	 )
	 SETTINGS max_threads = 4, max_execution_time = 600`,
	// Swap both live tables in one metadata transaction: a board swapped
	// new beside last cycle's span would be the exact overstatement this
	// surface exists to avoid.
	`EXCHANGE TABLES stellar.account_creators_rollup_staging AND stellar.account_creators_rollup,
	                 stellar.account_creators_stats_staging AND stellar.account_creators_stats`,
}

// RunCreatorsRollup executes one full recompute + atomic exchange.
func RunCreatorsRollup(ctx context.Context, addr string, logf func(format string, args ...any)) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	for i, stmt := range creatorsRollupStatements {
		if err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("clickhouse: creators rollup step %d/%d: %w", i+1, len(creatorsRollupStatements), err)
		}
		logf("step %d/%d done", i+1, len(creatorsRollupStatements))
	}
	return nil
}

// AccountCreators reads the rollup snapshot: the top `limit` rows of the
// board plus the cycle's totals and covered span. ok=false (not an
// error) when the rollup isn't provisioned or hasn't completed a cycle —
// the handler 503s rather than serving zeros, and rather than serving a
// board with no span to qualify it.
func (r *ExplorerReader) AccountCreators(ctx context.Context, limit int) (AccountCreators, bool, error) {
	if !r.probeSchema(ctx, &r.accountCreatorsProbe,
		`SELECT rank FROM stellar.account_creators_rollup LIMIT 1`, true) {
		return AccountCreators{}, false, nil
	}
	var out AccountCreators
	if err := r.readCreatorsBoard(ctx, &out, limit); err != nil {
		return AccountCreators{}, false, err
	}
	if err := r.readCreatorsStats(ctx, &out); err != nil {
		return AccountCreators{}, false, err
	}
	// A board with no span behind it cannot be qualified honestly, so it
	// is not served. This is the guard against the board arm of the
	// exchange landing while the stats arm is empty.
	if out.ThruLedger == 0 {
		return AccountCreators{}, false, nil
	}
	return out, true, nil
}

func (r *ExplorerReader) readCreatorsBoard(ctx context.Context, out *AccountCreators, limit int) error {
	rows, err := r.conn.Query(ctx, `
		SELECT rank, creator, accounts_created, funded_stroops, live_accounts, live_stroops,
		       first_ledger, last_ledger, first_created_at, last_created_at, computed_at
		FROM stellar.account_creators_rollup ORDER BY rank LIMIT ?`, limit)
	if err != nil {
		return fmt.Errorf("clickhouse: account creators board: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			row        AccountCreatorRow
			computedAt time.Time
		)
		if err := rows.Scan(&row.Rank, &row.Creator, &row.AccountsCreated, &row.FundedStroops,
			&row.LiveAccounts, &row.LiveStroops, &row.FirstLedger, &row.LastLedger,
			&row.FirstCreatedAt, &row.LastCreatedAt, &computedAt); err != nil {
			return fmt.Errorf("clickhouse: scan account creator: %w", err)
		}
		out.ComputedAt = computedAt
		out.Board = append(out.Board, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

// creatorsStatsMetrics maps the metric-keyed stats table onto the
// snapshot. A metric absent from the table leaves its field zero, which
// AccountCreators' ThruLedger guard turns into "warming" rather than a
// span claim nothing backs.
func (r *ExplorerReader) readCreatorsStats(ctx context.Context, out *AccountCreators) error {
	rows, err := r.conn.Query(ctx, `SELECT metric, value FROM stellar.account_creators_stats`)
	if err != nil {
		return fmt.Errorf("clickhouse: account creators stats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			metric string
			value  int64
		)
		if err := rows.Scan(&metric, &value); err != nil {
			return fmt.Errorf("clickhouse: scan account creators stat: %w", err)
		}
		switch metric {
		case "creators_total":
			out.CreatorsTotal = value
		case "creations_total":
			out.CreationsTotal = value
		case "live_accounts_total":
			out.LiveAccountsTotal = value
		case "from_ledger":
			out.FromLedger = clampLedger(value)
		case "thru_ledger":
			out.ThruLedger = clampLedger(value)
		case "from_time":
			out.FromTime = time.Unix(value, 0).UTC()
		case "thru_time":
			out.ThruTime = time.Unix(value, 0).UTC()
		}
	}
	return rows.Err()
}

// clampLedger narrows a stats Int64 to the uint32 a ledger sequence is,
// refusing the negative/overflowing values the column type permits but
// the data never holds.
func clampLedger(v int64) uint32 {
	if v <= 0 || v > int64(^uint32(0)) {
		return 0
	}
	return uint32(v)
}
