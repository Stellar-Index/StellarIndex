package clickhouse

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
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

// opCreateAccount is the CreateAccount operation type as
// stellar.operations records it (op_type is written from
// xdr.OperationType.String(), like the sponsorship names in
// account_sponsors_rollup.go).
const opCreateAccount = "OperationTypeCreateAccount"

// P23BoundaryLedger is the first ledger of Protocol 23 (Whisk, pubnet
// 2025-09-03) — the ledger at which the lake's record of an account
// creation CHANGES REPRESENTATION. It does not stop there, and neither
// may this board.
//
// BELOW it a CreateAccount lands in stellar.account_movements as a
// `create_account` movement pair (ADR-0047 D2), written by
// `stellarindex-ops classic-movements-backfill`, whose -to flag is
// hard-clamped below this ledger: that archive is historical-only by
// design. Measured on r1 2026-09-07, max(ledger) for
// movement_kind='create_account' is 58,762,516 against a lake tip of
// 64,310,629, and the count at or above this ledger is 0 — not a gap, a
// boundary.
//
// AT OR ABOVE it, CAP-67 folded classic payments into the token-event
// model, so the same creation's funding leg is a `transfer` movement
// written by the live ch-cap67-movements follow daemon, and the fact
// that the transfer WAS a creation is carried by the
// OperationTypeCreateAccount row in stellar.operations. Reading only the
// classic arm therefore ranks creators over a population that ends at
// this ledger — 4,715,612 creations short as of the measurement above
// (#493).
//
// Same VALUE as internal/sources/classicmovements.P23StartLedger and
// internal/storage/timescale.SEP41MovementsFloorLedger, not the same
// CONSTANT: internal/storage sits below internal/sources in the repo's
// import direction (scripts/ci/lint-imports.sh's L/storage-below-compute
// rule, test files included), and a clickhouse→timescale edge would
// couple the lake package to the served tier to borrow a literal.
// TestP23BoundaryConstantsAgree (internal/api/v1/explorer_movements_test.go)
// is the executable assertion that keeps all three from drifting — that
// package can import every layer.
const P23BoundaryLedger uint32 = 58_762_517

// p23Boundary is P23BoundaryLedger as SQL text. Both creation arms clamp
// against this one value rather than repeating the literal, so the two
// halves of the ledger axis cannot drift apart or overlap.
var p23Boundary = strconv.FormatUint(uint64(P23BoundaryLedger), 10)

// creatorsBoardSettings is the settings clause for the board's join over
// the working table. The memory shape is the house full-history scan
// class; query_plan_join_swap_table = 0 additionally PINS which side of
// the join is built into the hash table.
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

// creatorsP23ArmSettings is the settings clause for the post-P23
// creation arm, the one walked step in this cycle that joins.
//
// It pins the build side for the same reason the board does, over a
// different pair. The right side is that window's
// OperationTypeCreateAccount rows — 936,483 in partition 63 on r1
// 2026-09-07, growing with the creation rate — and the left side is the
// window's `transfer` movements, which grow with Soroban token traffic
// and are already two orders of magnitude larger (about 300 M per
// partition). Unpinned, a planner estimate that swapped them would build
// a hash table over the transfer population and blow the budget in a
// step that measures 1.43 GiB pinned.
//
// The execution cap is the board's 1800 s rather than the walk's 600 s.
// The cap is per window and is sized for headroom over the widest
// measured window: post-P23 windows cost 16.0-106.9 s on r1 2026-09-07
// (partition 62 the widest), so 1800 s keeps roughly the 17x margin the
// 600 s cap was chosen to give the classic arm's 24.7 s windows, on a
// box that also runs galexie and the sibling rollups.
const creatorsP23ArmSettings = boundedScanSettings +
	", query_plan_join_swap_table = 0, max_execution_time = 1800"

// creatorEdgesSettings is the settings clause for the graph arm's one
// aggregation: 24,824,706 creation events in the working table collapsed
// to 20,954,070 distinct (creator, created) pairs.
//
// Measured on r1 2026-09-09 at max_threads=2: 3.14 GiB / 24.5 s. That is
// BELOW this cycle's existing peak — creatorsBoardSettings' join against
// the live account population, 3.31 GiB — so the graph arm does not move
// the cycle's ceiling, and the figure a future ceiling must be sized
// from is still the board's. Headroom here is a stated function of one
// population: about 150 bytes per distinct pair, so the 8 GiB budget is
// reached near 55 M pairs against today's 21 M.
//
// The per-edge `creations` count INHERITS the one-leg-per-op assumption
// documented on the post-P23 arm below: a second `sent` transfer leg for
// one creation would double it, silently, the same way it would double
// the board's accounts_created. The EDGE itself is robust to that — the
// pair is still (creator, created) — so a graph traversal stays correct
// even where the weight would not be. Same detector, same fix.
//
// The execution cap is the board's 1800 s rather than
// the walk's 600 s: this step is not walked — a (creator, created) pair
// can span lake partitions, so a per-window aggregation would emit the
// same edge more than once — and 1800 s is a 73x margin on the measured
// time, on a box that also runs galexie and the sibling rollups.
// creatorEdgesSettings deliberately does NOT set
// optimize_aggregation_in_order. In-order aggregation cannot spill, so
// it silently cancels the max_bytes_before_external_group_by in
// boundedScanSettings and converts an aggregation that would have gone
// to disk into a memory-limit failure. This step aggregates 24.8 M
// creation events into 20.96 M (creator, created) pairs; with the
// setting it died at the 8 GiB ceiling on 2026-09-09, without it the
// same aggregation completes inside 512 MiB.
//
// Note that in-order aggregation WAS applicable here — the working table
// sorts by (creator, ledger, created) and `creator` is this aggregation's
// leading group key — which is exactly what makes the trap worth a
// comment. The setting was not a mistake about the sort order; it is
// unsafe beside a spill limit however well the order matches.
const creatorEdgesSettings = boundedScanSettings + ", max_execution_time = 1800"

// creatorsRollupStatements is the full recompute cycle: truncate the
// working table, WALK the archive one lake partition at a time landing
// that partition's deduplicated creations from BOTH sides of the
// Protocol 23 boundary, truncate both staging arms, aggregate the board
// from the working table, derive the stats FROM that board, then swap
// the pair atomically.
//
// WHY TWO CREATION ARMS. Protocol 23 changed how a CreateAccount is
// recorded, not whether it is. The classic arm reads the
// `create_account` movements the pre-P23 archive holds; the post-P23 arm
// reads the CAP-67 `transfer` movement that carries the same funding
// leg, joined to the OperationTypeCreateAccount row that says the
// transfer was a creation. The two clamp on opposite sides of
// P23BoundaryLedger, so their union is every ledger and their
// intersection is empty: no creation is missed and none is counted
// twice. Reading only the classic arm was #493 — a league table ranking
// over a population that ended a year before the tip.
//
// The post-P23 pairing is exact rather than approximate, and was
// measured that way on r1 2026-09-07 over ledgers 63,000,000-63,010,000:
// of 6,266 distinct create_account operations, the 5,890 in SUCCESSFUL
// transactions each match exactly one `transfer` movement leg and the
// 376 in failed transactions match none — so joining through the
// movement is what gates the board on transaction success, which
// stellar.operations alone does not do (it retains failed transactions'
// operations by design). On the same sample the movement's `address`
// equals the operation's source account in all 5,890 cases, its
// `counterparty` is the created G-strkey, and its `asset` is `native`;
// 197 of 666 sampled amounts are zero, which is CAP-33 sponsored
// creation and a real value, not a missing one.
//
// WHY THE WALK, AND WHY THE BOARD JOIN SITS OUTSIDE IT. movement_kind is
// not in account_movements' ORDER BY, so finding creations is
// scan-shaped over the whole archive. Doing that in ONE statement holds
// two growing things at once: the dedupe hash table, one state per
// creation in all of history, and the board join's build side, one row
// per live account. Measured on r1 2026-09-06 the pair summed past the
// 8 GiB budget — 8.12 GiB in FillingRightJoinSide, with the dedupe
// already spilled to 32 external parts and all 10,309,146,441 movement
// rows read. Raising the ceiling would only move which cycle fails,
// because neither population stops growing.
//
// So the scan is walked and the board join is not inside the walk. A
// window holds only its own partition's creations — the widest measured
// classic window is 1,027,707 rows at 701.17 MiB, the widest post-P23
// one 1.43 GiB — and the board join runs ONCE against the working table,
// where its cost is the account population alone (3.31 GiB measured; see
// creatorsBoardSettings). Putting it inside the walk would instead have
// rebuilt that same 10.9 M-row hash table on every one of the 65 windows
// and re-read the 19.47 M-row account entry range each time, while
// leaving the cycle's largest single memory consumer un-walked.
//
// Each arm is free on the windows the other owns: the boundary clamp is
// a predicate on the partition key of both tables, so a window wholly on
// the far side prunes to no parts at all — measured at 0 rows read and
// 2-4 ms per arm on r1 2026-09-07. The cycle therefore still pays one
// archive pass per window, not two.
//
// Nothing is written to a staging arm until the walk has finished, so an
// interrupted cycle leaves the live board as the previous cycle left it,
// and the single EXCHANGE stays the only moment anything becomes
// visible.
//
// Deriving the stats from the staging board rather than from a second
// scan is what makes the served coverage span honest by construction:
// the totals and the span describe exactly the rows the board was built
// from, so the two cannot drift apart. With both arms landing in the
// working table that span now reaches the lake tip because the board
// genuinely covers it, not because the tip was substituted for it.
//
// Both dedupe arms reproduce ReplacingMergeTree semantics explicitly
// (argMax over ingested_at, grouped by the source table's full ORDER BY
// key) rather than using FINAL, which on a 10-billion-row archive would
// pay merge-on-read for the whole table instead of for the rows that
// survive the movement_kind filter. Grouping per window is exact, not an
// approximation of grouping globally: account_movements is PARTITION BY
// intDiv(ledger, 1000000) and `ledger` is part of its ORDER BY, and
// stellar.operations is PARTITION BY intDiv(ledger_seq, 1000000) with
// ledger_seq leading its ORDER BY, so all rows sharing an ORDER BY key
// share a partition and no duplicate group can straddle a window
// boundary.
var creatorsRollupStatements = []rollupStep{
	{sql: `TRUNCATE TABLE stellar.account_creators_ops`},
	// The classic arm, walked. Reads one lake partition below the
	// boundary, writes that partition's deduplicated creations, and
	// reads no other table.
	{walk: true, windowBinds: 1, sql: `INSERT INTO stellar.account_creators_ops
	     (creator, created, amount, ledger, closed_at)
	 SELECT address AS creator,
	        argMax(counterparty, ingested_at) AS created,
	        argMax(amount, ingested_at) AS amount,
	        ledger,
	        toDateTime(argMax(ledger_close_time, ingested_at), 'UTC') AS closed_at
	 FROM stellar.account_movements
	 WHERE ledger BETWEEN ? AND ?
	   AND ledger < ` + p23Boundary + `
	   AND movement_kind = 'create_account' AND direction = 'sent'
	 GROUP BY address, ledger, tx_hash, op_index, leg_index, direction
	 ` + rollupWalkSettings},
	// The post-P23 arm, walked. Same window, the far side of the
	// boundary, and the same working table. Both sources carry their own
	// window predicate — a join condition prunes neither table's
	// partitions — which is why this template binds the window twice.
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
	       AND ledger_seq >= ` + p23Boundary + `
	       AND op_type = '` + opCreateAccount + `'
	     GROUP BY ledger_seq, tx_hash, op_index
	 ) AS o ON m.ledger = o.ledger_seq AND m.tx_hash = o.tx_hash AND m.op_index = o.op_index
	 WHERE m.ledger BETWEEN ? AND ?
	   AND m.ledger >= ` + p23Boundary + `
	   AND m.movement_kind = 'transfer' AND m.direction = 'sent'
	 GROUP BY m.address, m.ledger, m.tx_hash, m.op_index, m.leg_index, m.direction
	 ` + creatorsP23ArmSettings},
	// ASSUMPTION, not an enforced invariant: exactly ONE `sent` transfer leg
	// per create_account op. `leg_index` is in the GROUP BY above, so if a
	// future protocol ever emits a second `sent` leg for one creation, this
	// arm emits TWO rows for it and that creator's accounts_created SILENTLY
	// DOUBLES — no error, no gap, just a wrong number on a served board.
	//
	// It holds today and was re-verified read-only on r1 2026-09-08 over
	// ledgers 63,000,000-63,099,999: ZERO (ledger, tx_hash, op_index) keys
	// carry more than one leg. The detector, if this needs re-checking or a
	// probe:
	//
	//   SELECT count() FROM (
	//     SELECT m.ledger, m.tx_hash, m.op_index, uniqExact(m.leg_index) AS legs
	//     FROM stellar.account_movements AS m
	//     INNER JOIN (SELECT ledger_seq, tx_hash, op_index FROM stellar.operations
	//                 WHERE ledger_seq BETWEEN ? AND ? AND ledger_seq >= 58762517
	//                   AND op_type = 'OperationTypeCreateAccount'
	//                 GROUP BY ledger_seq, tx_hash, op_index) AS o
	//       ON m.ledger = o.ledger_seq AND m.tx_hash = o.tx_hash
	//      AND m.op_index = o.op_index
	//     WHERE m.ledger BETWEEN ? AND ? AND m.ledger >= 58762517
	//       AND m.movement_kind = 'transfer' AND m.direction = 'sent'
	//     GROUP BY m.ledger, m.tx_hash, m.op_index HAVING legs > 1)
	//
	// Dropping `leg_index` from the GROUP BY would make the arm robust by
	// construction and is a no-op on today's data — but it changes a query
	// verified at whole-partition scale, on money, and the multi-leg case has
	// no integration fixture to prove the collapse picks the right
	// counterparty. Left as an assumption ON PURPOSE, written down here rather
	// than only in a private ledger, so whoever edits this GROUP BY sees it.
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
	// ── The graph arm (#351) ───────────────────────────────────────
	// The board says WHO created the most. These two say WHOM — one row
	// per distinct (creator, created) pair, held in both sort orders so
	// each direction of the question is a primary-key range read. Same
	// working table, so the edges and the board cannot describe
	// different data.
	{sql: `TRUNCATE TABLE stellar.account_creator_edges_staging`},
	{sql: `TRUNCATE TABLE stellar.account_creator_edges_by_created_staging`},
	{sql: `INSERT INTO stellar.account_creator_edges_staging
	     (creator, created, creations, funded_stroops, first_ledger, last_ledger, first_at, last_at)
	 SELECT creator, created,
	        toUInt64(count()) AS creations,
	        toInt128(sum(amount)) AS funded_stroops,
	        min(ledger) AS first_ledger,
	        max(ledger) AS last_ledger,
	        min(closed_at) AS first_at,
	        max(closed_at) AS last_at
	 FROM stellar.account_creators_ops
	 GROUP BY creator, created
	 ` + creatorEdgesSettings},
	// The reverse ordering is filled FROM the staging arm just written,
	// not by re-aggregating the archive: the second direction costs a
	// re-sort of 21 M already-collapsed rows rather than a second pass
	// over 24.8 M creation events.
	{sql: `INSERT INTO stellar.account_creator_edges_by_created_staging
	     (created, creator, creations, funded_stroops, first_ledger, last_ledger, first_at, last_at)
	 SELECT created, creator, creations, funded_stroops, first_ledger, last_ledger, first_at, last_at
	 FROM stellar.account_creator_edges_staging
	 ` + boundedScanSettings + `, max_execution_time = 1800`},
	// Swap every live table in one metadata transaction: a board swapped
	// new beside last cycle's span would be the exact overstatement this
	// surface exists to avoid, and a graph swapped beside a board built
	// from a different cycle's working table would be the same defect one
	// level down. The working table is not served and is never swapped.
	{sql: `EXCHANGE TABLES stellar.account_creators_rollup_staging AND stellar.account_creators_rollup,
	                 stellar.account_creators_stats_staging AND stellar.account_creators_stats,
	                 stellar.account_creator_edges_staging AND stellar.account_creator_edges,
	                 stellar.account_creator_edges_by_created_staging AND stellar.account_creator_edges_by_created`},
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
