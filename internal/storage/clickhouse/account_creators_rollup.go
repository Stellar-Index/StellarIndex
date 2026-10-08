package clickhouse

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// AccountCreatorRow is one league-table row. AccountsCreated, FundedStroops and the ledger bounds
// are immutable history; LiveAccounts/LiveStroops are point-in-time as of the computing cycle.
type AccountCreatorRow struct {
	Rank            uint32
	Creator         string
	AccountsCreated uint64
	// FundedStroops is the sum of starting balances; zero is real (CAP-33 sponsored creation).
	FundedStroops  *big.Int
	LiveAccounts   uint64
	LiveStroops    *big.Int
	FirstLedger    uint32
	LastLedger     uint32
	FirstCreatedAt time.Time
	LastCreatedAt  time.Time
}

// AccountCreators is one cycle's snapshot: a board slice, the totals and the span aggregated.
// FromLedger/ThruLedger are DATA-DERIVED (ADR-0031): min/max over the creation rows read, never
// genesis by assumption.
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

// opCreateAccount is the op_type as stellar.operations records it (xdr.OperationType.String()).
const opCreateAccount = "OperationTypeCreateAccount"

// P23BoundaryLedger is the first Protocol 23 ledger, where the lake's record of account creation
// changes representation. Below it a CreateAccount is a `create_account` movement (ADR-0047 D2;
// classic-movements-backfill is clamped below this ledger by design); at or above it, a CAP-67
// `transfer` movement flagged by an OperationTypeCreateAccount row in stellar.operations. Reading
// only the classic arm under-ranks creators.
// Same VALUE as classicmovements.P23StartLedger and timescale.SEP41MovementsFloorLedger, not the
// same constant: internal/storage sits below internal/sources (lint-imports.sh
// L/storage-below-compute). TestP23BoundaryConstantsAgree keeps all three equal.
const P23BoundaryLedger uint32 = 58_762_517

// creatorsBoardSettings is the board join's settings. query_plan_join_swap_table = 0 PINS the
// hash-table build side to the live-account table; left to the planner, the built side would
// silently switch populations as the archive grows. Peak is then ~313 bytes per live account
// against the 8 GiB budget.
const creatorsBoardSettings = boundedScanSettings +
	", query_plan_join_swap_table = 0, max_execution_time = 1800"

// creatorsP23ArmSettings is the post-P23 arm's join. It pins the build side to the window's
// CreateAccount rows, since the movements side is two orders of magnitude larger. The 1800 s cap is
// per window, sized for headroom over the widest measured window.
const creatorsP23ArmSettings = boundedScanSettings +
	", query_plan_join_swap_table = 0, max_execution_time = 1800"

// creatorEdgesSettings is the graph arm's aggregation. Peak is ~150 bytes per distinct (creator,
// created) pair, below the board join's peak. The per-edge `creations` inherits the post-P23 arm's
// one-leg-per-op assumption.
// Deliberately NO optimize_aggregation_in_order: it cannot spill, so it cancels
// max_bytes_before_external_group_by and turns a spilling aggregation into a memory-limit failure,
// even though the sort order matches. Not walked: a pair can span lake partitions.
const creatorEdgesSettings = boundedScanSettings + ", max_execution_time = 1800"

// creatorsRollupStatements is the full recompute: WALK the archive one lake partition at a time
// into the working table (deduplicated creations from both sides of the P23 boundary), aggregate
// the board, derive the stats from that board, then EXCHANGE the staged tables atomically. Nothing
// is visible until the swap, so an interrupted cycle leaves the previous board.
// Two arms: P23 changed how a CreateAccount is recorded, not whether. They clamp on opposite sides
// of `boundary`, so every ledger is covered exactly once. The post-P23 arm joins through the
// transfer movement, which also gates on transaction success (stellar.operations keeps failed
// transactions' operations).
// The boundary is a parameter: on a reset test net every ledger is post-P23, so a baked-in pubnet
// constant would leave the classic arm owning everything and the board empty. Callers pass
// movements_floor_ledger.
// Walked, with the board join outside the walk: movement_kind is not in account_movements' ORDER
// BY, so finding creations is scan-shaped, and one statement would hold the dedupe table and the
// join's build side together, past the 8 GiB budget. A window wholly on the other side of the
// boundary prunes to no parts.
// Stats derive from the staging board so the served span describes exactly the rows ranked.
// Dedup reproduces ReplacingMergeTree explicitly (argMax over ingested_at, grouped by the full
// ORDER BY key) rather than FINAL, which would pay merge-on-read for the whole table. Per-window
// grouping is exact: both tables are partitioned by intDiv(ledger, 1e6) with the ledger leading
// ORDER BY, so no duplicate group straddles a window.
func creatorsRollupStatements(boundaryLedger uint32) []rollupStep {
	// Both arms clamp against this one value so the halves of the ledger axis cannot overlap or
	// leave a gap.
	boundary := strconv.FormatUint(uint64(boundaryLedger), 10)
	steps := []rollupStep{{sql: `TRUNCATE TABLE stellar.account_creators_ops`}}
	steps = append(steps, creatorsCreationArmSteps(boundary)...)
	steps = append(steps, creatorsBoardSteps()...)
	return append(steps, creatorsGraphSteps()...)
}

// creatorsCreationArmSteps walks the archive into the working table:
// the classic arm below the boundary and the post-P23 arm above it.
func creatorsCreationArmSteps(boundary string) []rollupStep {
	return []rollupStep{
		// Classic arm, walked: one lake partition below the boundary.
		{walk: true, windowBinds: 1, sql: `INSERT INTO stellar.account_creators_ops
	     (creator, created, amount, ledger, closed_at)
	 SELECT address AS creator,
	        argMax(counterparty, ingested_at) AS created,
	        argMax(amount, ingested_at) AS amount,
	        ledger,
	        toDateTime(argMax(ledger_close_time, ingested_at), 'UTC') AS closed_at
	 FROM stellar.account_movements
	 WHERE ledger BETWEEN ? AND ?
	   AND ledger < ` + boundary + `
	   AND movement_kind = 'create_account' AND direction = 'sent'
	 GROUP BY address, ledger, tx_hash, op_index, leg_index, direction
	 ` + rollupWalkSettings},
		// Post-P23 arm, walked. Each source carries its own window predicate (a join condition
		// prunes neither table's partitions), so the template binds the window twice.
		{walk: true, windowBinds: 2, sql: `INSERT INTO stellar.account_creators_ops
	     (creator, created, amount, ledger, closed_at)
	 SELECT m.address AS creator,
	        argMax(m.counterparty, m.ingested_at) AS created,
	        argMax(m.amount, m.ingested_at) AS amount,
	        m.ledger AS ledger,
	        toDateTime(argMax(m.ledger_close_time, m.ingested_at), 'UTC') AS closed_at
	 FROM stellar.account_movements AS m
	 INNER JOIN (
	     SELECT ledger_seq, tx_hash, op_index
	     FROM stellar.operations
	     WHERE ledger_seq BETWEEN ? AND ?
	       AND ledger_seq >= ` + boundary + `
	       AND op_type = '` + opCreateAccount + `'
	     GROUP BY ledger_seq, tx_hash, op_index
	 ) AS o ON m.ledger = o.ledger_seq AND m.tx_hash = o.tx_hash AND m.op_index = o.op_index
	 WHERE m.ledger BETWEEN ? AND ?
	   AND m.ledger >= ` + boundary + `
	   AND m.movement_kind = 'transfer' AND m.direction = 'sent'
	 GROUP BY m.address, m.ledger, m.tx_hash, m.op_index, m.leg_index, m.direction
	 ` + creatorsP23ArmSettings},
		// ASSUMPTION, not an enforced invariant: exactly ONE `sent` transfer leg per create_account
		// op. leg_index is in the GROUP BY, so a second `sent` leg for one creation would emit two
		// rows and silently double that creator's accounts_created. Re-check by counting (ledger,
		// tx_hash, op_index) keys with uniqExact(leg_index) > 1 over the joined set.
		// Dropping leg_index would make it robust but changes a query verified at partition scale,
		// on money, with no fixture proving the collapse picks the right counterparty.
	}
}

// creatorsBoardSteps folds the working table into the served board and
// its stats, into the staging tables the final swap exchanges.
func creatorsBoardSteps() []rollupStep {
	return []rollupStep{
		{sql: `TRUNCATE TABLE stellar.account_creators_rollup_staging`},
		{sql: `TRUNCATE TABLE stellar.account_creators_stats_staging`},
		// live_* aggregate over the DISTINCT (creator, created) pair, not per creation op: an
		// address recycled by one creator (create, merge, create) would otherwise be counted once
		// per creation. The pair is the address's LATEST creation (an account cannot be created
		// while it exists); same-ledger ties break on creator so one row is credited.
		// accounts_created/funded_stroops stay per-event.
		{sql: `INSERT INTO stellar.account_creators_rollup_staging
	     (rank, creator, accounts_created, funded_stroops, live_accounts, live_stroops,
	      first_ledger, last_ledger, first_created_at, last_created_at)
	 SELECT row_number() OVER (ORDER BY accounts_created DESC, creator) AS rank,
	        creator, accounts_created, funded_stroops, live_accounts, live_stroops,
	        first_ledger, last_ledger, first_created_at, last_created_at
	 FROM (
	     SELECT totals.creator AS creator,
	            totals.accounts_created AS accounts_created,
	            totals.funded_stroops AS funded_stroops,
	            ifNull(live.live_accounts, 0) AS live_accounts,
	            ifNull(live.live_stroops, 0) AS live_stroops,
	            totals.first_ledger AS first_ledger,
	            totals.last_ledger AS last_ledger,
	            totals.first_created_at AS first_created_at,
	            totals.last_created_at AS last_created_at
	     FROM (
	         SELECT creator,
	                toUInt64(count()) AS accounts_created,
	                sum(toInt128(amount)) AS funded_stroops,
	                min(ledger) AS first_ledger,
	                max(ledger) AS last_ledger,
	                min(closed_at) AS first_created_at,
	                max(closed_at) AS last_created_at
	         FROM stellar.account_creators_ops
	         GROUP BY creator
	     ) AS totals
	     LEFT JOIN (
	         SELECT creator,
	                toUInt64(uniqExactIf(created, is_live)) AS live_accounts,
	                sum(toInt128(live_balance)) AS live_stroops
	         FROM (
	             SELECT c.creator AS creator,
	                    c.created AS created,
	                    max(e.account_id != '') AS is_live,
	                    any(e.balance) AS live_balance
	             FROM (
	                 SELECT created, argMax(creator, (ledger, creator)) AS creator
	                 FROM stellar.account_creators_ops
	                 GROUP BY created
	             ) AS c
	             LEFT JOIN (
	                 SELECT account_id, balance
	                 FROM stellar.ledger_entries_current FINAL
	                 WHERE entry_type = 'account' AND change_type != 'removed'
	             ) AS e ON c.created = e.account_id
	             GROUP BY c.creator, c.created
	         )
	         GROUP BY creator
	     ) AS live ON totals.creator = live.creator
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
	}
}

// creatorsGraphSteps folds the same working table into the two edge
// orderings and ends with the one swap that serves every table at once.
func creatorsGraphSteps() []rollupStep {
	return []rollupStep{
		// Graph arm: the board says WHO created the most; these say WHOM, held in both sort orders
		// so each direction is a primary-key range read. Same working table, so edges and board
		// describe the same data.
		{sql: `TRUNCATE TABLE stellar.account_creator_edges_staging`},
		{sql: `TRUNCATE TABLE stellar.account_creator_edges_by_created_staging`},
		{sql: `INSERT INTO stellar.account_creator_edges_staging
	     (creator, created, creations, funded_stroops, first_ledger, last_ledger, first_at, last_at)
	 SELECT creator, created,
	        toUInt64(count()) AS creations,
	        sum(toInt128(amount)) AS funded_stroops,
	        min(ledger) AS first_ledger,
	        max(ledger) AS last_ledger,
	        min(closed_at) AS first_at,
	        max(closed_at) AS last_at
	 FROM stellar.account_creators_ops
	 GROUP BY creator, created
	 ` + creatorEdgesSettings},
		// The reverse ordering is filled from the staging arm, not by re-aggregating the archive.
		{sql: `INSERT INTO stellar.account_creator_edges_by_created_staging
	     (created, creator, creations, funded_stroops, first_ledger, last_ledger, first_at, last_at)
	 SELECT created, creator, creations, funded_stroops, first_ledger, last_ledger, first_at, last_at
	 FROM stellar.account_creator_edges_staging
	 ` + boundedScanSettings + `, max_execution_time = 1800`},
		// Swap every live table in one metadata transaction so board, span and graph never come
		// from different cycles. The working table is never served or swapped.
		{sql: `EXCHANGE TABLES stellar.account_creators_rollup_staging AND stellar.account_creators_rollup,
	                 stellar.account_creators_stats_staging AND stellar.account_creators_stats,
	                 stellar.account_creator_edges_staging AND stellar.account_creator_edges,
	                 stellar.account_creator_edges_by_created_staging AND stellar.account_creator_edges_by_created`},
	}
}

// RunCreatorsRollup executes one full recompute + atomic exchange; boundary is the network's P23
// boundary (see creatorsRollupStatements).
func RunCreatorsRollup(ctx context.Context, addr string, boundary uint32, logf func(format string, args ...any)) error {
	return runRollupCycle(ctx, addr, "creators rollup", creatorsRollupStatements(boundary), logf)
}

// AccountCreators reads the rollup snapshot: top `limit` board rows plus totals and span. ok=false
// (not an error) when the rollup is unprovisioned or has no completed cycle: the handler 503s
// rather than serve zeros.
// A non-empty `account` narrows Board to that creator's row (rank is whole-aggregation, so paging
// cannot answer "where does this address stand"); `limit` is ignored and totals stay
// whole-aggregation.
func (r *ExplorerReader) AccountCreators(ctx context.Context, limit int, account string) (AccountCreators, bool, error) {
	if !r.probeSchema(ctx, &r.accountCreatorsProbe,
		`SELECT rank FROM stellar.account_creators_rollup LIMIT 1`, true) {
		return AccountCreators{}, false, nil
	}
	var out AccountCreators
	if err := r.readCreatorsBoard(ctx, &out, limit, account); err != nil {
		return AccountCreators{}, false, err
	}
	if err := r.readCreatorsStats(ctx, &out); err != nil {
		return AccountCreators{}, false, err
	}
	// No span, no board: guards the board arm of the exchange landing while stats are empty.
	if out.ThruLedger == 0 {
		return AccountCreators{}, false, nil
	}
	return out, true, nil
}

const creatorsBoardCols = `rank, creator, accounts_created, funded_stroops, live_accounts, live_stroops,
	       first_ledger, last_ledger, first_created_at, last_created_at, computed_at`

// creatorsBoardKeyedSQL is the ?account= read: no ORDER BY/LIMIT since there is one row per creator
// and its precomputed `rank` is the true rank. The table is ORDER BY rank, so the creator predicate
// is served by the idx_creators_rollup_creator skip index.
const creatorsBoardKeyedSQL = `SELECT ` + creatorsBoardCols + `
	FROM stellar.account_creators_rollup WHERE creator = ?`

func (r *ExplorerReader) readCreatorsBoard(ctx context.Context, out *AccountCreators, limit int, account string) error {
	var (
		rows driver.Rows
		err  error
	)
	if account != "" {
		rows, err = r.conn.Query(ctx, creatorsBoardKeyedSQL, account)
	} else {
		rows, err = r.conn.Query(ctx, `
		SELECT `+creatorsBoardCols+`
		FROM stellar.account_creators_rollup ORDER BY rank LIMIT ?`, limit)
	}
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

// readCreatorsStats maps the metric-keyed stats table onto the snapshot. A missing metric stays
// zero, which the ThruLedger guard turns into "warming". Runs after readCreatorsBoard: on a keyed
// miss the cycle time comes from these rows.
func (r *ExplorerReader) readCreatorsStats(ctx context.Context, out *AccountCreators) error {
	rows, err := r.conn.Query(ctx, `SELECT metric, value, computed_at FROM stellar.account_creators_stats`)
	if err != nil {
		return fmt.Errorf("clickhouse: account creators stats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var statsAt time.Time
	for rows.Next() {
		var (
			metric     string
			value      int64
			computedAt time.Time
		)
		if err := rows.Scan(&metric, &value, &computedAt); err != nil {
			return fmt.Errorf("clickhouse: scan account creators stat: %w", err)
		}
		statsAt = laterTime(statsAt, computedAt)
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
	if out.ComputedAt.IsZero() {
		out.ComputedAt = statsAt
	}
	return rows.Err()
}

func laterTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// clampLedger narrows a stats Int64 to uint32, refusing values the column permits but the data
// never holds.
func clampLedger(v int64) uint32 {
	if v <= 0 || v > int64(^uint32(0)) {
		return 0
	}
	return uint32(v)
}
