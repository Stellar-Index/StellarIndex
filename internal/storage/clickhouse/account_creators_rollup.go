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

// creatorsBoardSettings is the settings clause for the single JOIN in
// this cycle. The memory shape is the house full-history scan class;
// query_plan_join_swap_table = 0 additionally PINS which side of the
// join is built into the hash table.
//
// Pinning matters because the two sides are sized by different
// populations. The right side is one row per account that currently
// exists — 10,928,611 rows at 3.18 GiB measured on r1 2026-09-06 at
// max_threads=2 — and it grows with the account population. The left
// side is one row per account creation ever, and it grows with chain
// history, which is far faster. Left to its own estimate the planner
// picks whichever side it believes smaller, so the side that gets built
// — and therefore the cycle's peak — would silently switch populations
// as the archive grows. Pinned, the peak is a stated function of one
// thing: about 313 bytes per live account, so the 8 GiB budget is
// reached near 27 million accounts against today's 10.9 million.
const creatorsBoardSettings = boundedScanSettings +
	", query_plan_join_swap_table = 0, max_execution_time = 1800"

// creatorsRollupStatements is the full recompute cycle: truncate the
// working table, WALK the movement archive one lake partition at a time
// landing that partition's deduplicated creations, truncate both staging
// arms, aggregate the board from the working table, derive the stats
// FROM that board, then swap the pair atomically.
//
// WHY THE WALK, AND WHY THE JOIN SITS OUTSIDE IT. movement_kind is not
// in account_movements' ORDER BY, so finding creations is scan-shaped
// over the whole archive. Doing that in ONE statement holds two growing
// things at once: the dedupe hash table, one state per creation in all
// of history, and the join's build side, one row per live account.
// Measured on r1 2026-09-06 the pair summed past the 8 GiB budget —
// 8.12 GiB in FillingRightJoinSide, with the dedupe already spilled to
// 32 external parts and all 10,309,146,441 movement rows read. Raising
// the ceiling would only move which cycle fails, because neither
// population stops growing.
//
// So the scan is walked and the join is not inside the walk. A window
// holds only its own partition's creations — the widest measured window
// is 1,027,707 rows at 701.17 MiB — and the join runs ONCE against the
// working table, where its cost is the account population alone
// (3.31 GiB measured; see creatorsBoardSettings). Putting the join
// inside the walk would instead have rebuilt that same 10.9 M-row hash
// table on every one of the 65 windows and re-read the 19.47 M-row
// account entry range each time, while leaving the cycle's largest
// single memory consumer un-walked.
//
// Nothing is written to a staging arm until the walk has finished, so an
// interrupted cycle leaves the live board as the previous cycle left it,
// and the single EXCHANGE stays the only moment anything becomes
// visible.
//
// Deriving the stats from the staging board rather than from a second
// scan is what makes the served coverage span honest by construction:
// the totals and the span describe exactly the rows the board was built
// from, so the two cannot drift apart.
//
// The dedupe arm reproduces ReplacingMergeTree semantics explicitly
// (argMax over ingested_at, grouped by the table's full ORDER BY key)
// rather than using FINAL, which on a 10-billion-row archive would pay
// merge-on-read for the whole table instead of for the rows that survive
// the movement_kind filter. Grouping per window is exact, not an
// approximation of grouping globally: account_movements is PARTITION BY
// intDiv(ledger, 1000000) and `ledger` is part of its ORDER BY, so all
// rows sharing an ORDER BY key share a partition and no duplicate group
// can straddle a window boundary.
var creatorsRollupStatements = []rollupStep{
	{sql: `TRUNCATE TABLE stellar.account_creators_ops`},
	// The walked step. Reads one lake partition, writes that partition's
	// deduplicated creations, and reads no other table — in particular it
	// does not join, which is what keeps its peak a function of the
	// window instead of the account population.
	{walk: true, sql: `INSERT INTO stellar.account_creators_ops
	     (creator, created, amount, ledger, closed_at)
	 SELECT address AS creator,
	        argMax(counterparty, ingested_at) AS created,
	        argMax(amount, ingested_at) AS amount,
	        ledger,
	        toDateTime(argMax(ledger_close_time, ingested_at), 'UTC') AS closed_at
	 FROM stellar.account_movements
	 WHERE ledger BETWEEN ? AND ?
	   AND movement_kind = 'create_account' AND direction = 'sent'
	 GROUP BY address, ledger, tx_hash, op_index, leg_index, direction
	 ` + rollupWalkSettings},
	{sql: `TRUNCATE TABLE stellar.account_creators_rollup_staging`},
	{sql: `TRUNCATE TABLE stellar.account_creators_stats_staging`},
	{sql: `INSERT INTO stellar.account_creators_rollup_staging
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
	            min(c.closed_at) AS first_created_at,
	            max(c.closed_at) AS last_created_at
	     FROM stellar.account_creators_ops AS c
	     LEFT JOIN (
	         SELECT account_id, balance
	         FROM stellar.ledger_entries_current FINAL
	         WHERE entry_type = 'account' AND change_type != 'removed'
	     ) AS e ON c.created = e.account_id
	     GROUP BY creator
	 )
	 ` + creatorsBoardSettings},
	{sql: `INSERT INTO stellar.account_creators_stats_staging (metric, value)
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
	 SETTINGS max_threads = 2, max_execution_time = 600`},
	// Swap both live tables in one metadata transaction: a board swapped
	// new beside last cycle's span would be the exact overstatement this
	// surface exists to avoid. The working table is not served and is
	// never swapped.
	{sql: `EXCHANGE TABLES stellar.account_creators_rollup_staging AND stellar.account_creators_rollup,
	                 stellar.account_creators_stats_staging AND stellar.account_creators_stats`},
}

// RunCreatorsRollup executes one full recompute + atomic exchange.
func RunCreatorsRollup(ctx context.Context, addr string, logf func(format string, args ...any)) error {
	return runRollupCycle(ctx, addr, "creators rollup", creatorsRollupStatements, logf)
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
