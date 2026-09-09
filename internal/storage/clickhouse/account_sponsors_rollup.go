package clickhouse

import (
	"context"
	"fmt"
	"time"
)

// Sponsorship operation type names as stellar.operations records them.
const (
	opBeginSponsoring  = "OperationTypeBeginSponsoringFutureReserves"
	opEndSponsoring    = "OperationTypeEndSponsoringFutureReserves"
	opRevokeSponsoring = "OperationTypeRevokeSponsorship"
)

// AccountSponsorRow is one row of the sponsor league table: an account
// and the sponsorship arrangements it has entered into (#351).
//
// Every figure here is IMMUTABLE HISTORY — what this account did, not
// what is currently in force. SponsorshipsStarted counts arrangements
// begun; RevocationsIssued counts revocations this account was the
// source of. Neither is a count of sponsorships still standing, because
// an arrangement also ends when the sponsored entry simply goes away
// (a trustline removed, an offer cancelled, an account merged), and no
// operation records that. See AccountSponsors for what this cannot say.
type AccountSponsorRow struct {
	Rank                uint32
	Sponsor             string
	SponsorshipsStarted uint64
	DistinctSponsored   uint64
	RevocationsIssued   uint64
	FirstLedger         uint32
	LastLedger          uint32
	FirstSeenAt         time.Time
	LastSeenAt          time.Time
}

// AccountSponsors is one cycle's snapshot: the requested slice of the
// board, the totals, and the ledger span the cycle actually aggregated.
//
// WHAT THIS DELIBERATELY DOES NOT CARRY: the live sponsored set. These
// numbers are derived by replaying sponsorship OPERATIONS, which gives
// history plus a derived state, never a directly observed one. A
// sponsorship also lapses when the sponsored entry is deleted or the
// sponsored account merges away, and neither emits a sponsorship
// operation — so a "currently sponsoring" figure computed from this
// source would OVERSTATE. Observing it needs the sponsoringID inside
// ledger_entries_current's entry_xdr; that is a separate projection and
// is not served.
//
// FromLedger/ThruLedger are data-derived (ADR-0031): min/max over the
// operations the cycle read. The floor lands at protocol 14, where
// sponsorship was introduced — the feature's own genesis, not a gap.
type AccountSponsors struct {
	Board                  []AccountSponsorRow
	SponsorsTotal          int64
	SponsorshipsTotal      int64
	DistinctSponsoredTotal int64
	RevocationsTotal       int64
	// AmbiguousTxs counts transactions carrying more than one distinct
	// sponsor, which are excluded from per-sponsor attribution. Published
	// so the exclusion is visible rather than silent.
	AmbiguousTxs int64
	FromLedger   uint32
	ThruLedger   uint32
	FromTime     time.Time
	ThruTime     time.Time
	ComputedAt   time.Time
}

// perTxCTE resolves each transaction's sponsor and the set of accounts
// it sponsored there. A sandwich's End operation is sourced by the
// SPONSORED account, so the sponsored identity needs no body decode.
const perTxCTE = `
	per_tx AS (
	    SELECT lseq, tidx,
	           uniqExactIf(src, otype = '` + opBeginSponsoring + `') AS n_sponsors,
	           anyIf(src, otype = '` + opBeginSponsoring + `') AS sponsor,
	           groupArrayIf(src, otype = '` + opEndSponsoring + `') AS sponsored_set,
	           countIf(otype = '` + opBeginSponsoring + `') AS begins,
	           min(ctime) AS ctime
	    FROM stellar.account_sponsors_ops
	    GROUP BY lseq, tidx
	)`

// factsCTE explodes the per-transaction view into one row per countable
// fact. Only single-sponsor transactions contribute attribution; the
// rest are counted as ambiguous by the stats arm.
const factsCTE = `
	facts AS (
	    SELECT sponsor AS src, '' AS counterparty, 'begin' AS kind, lseq, ctime, begins AS w
	    FROM per_tx WHERE n_sponsors = 1
	    UNION ALL
	    SELECT sponsor AS src, arrayJoin(sponsored_set) AS counterparty, 'end' AS kind, lseq, ctime, 1 AS w
	    FROM per_tx WHERE n_sponsors = 1
	    UNION ALL
	    SELECT src, '' AS counterparty, 'revoke' AS kind, lseq, ctime, 1 AS w
	    FROM stellar.account_sponsors_ops WHERE otype = '` + opRevokeSponsoring + `'
	)`

// sponsorsGatedArmSettings is the settings clause for this cycle's one
// walked step, which reads two lake archives and joins them.
//
// query_plan_join_swap_table = 0 PINS which side is built into the hash
// table, for the reason the sibling cycle's post-P23 arm pins its own
// (creatorsP23ArmSettings) and over a far more lopsided pair. The build
// side is that window's sponsorship operations BEFORE the gate drops the
// failed ones — 2,475,607 rows in partition 55, the widest measured on r1
// 2026-09-07 — and the streaming side is that window's transactions,
// 318,127,649 rows in the same partition. That is 128.5 to 1 there and
// 460 to 1 in partition 63
// (1,129,122 operations against 519,663,457 transactions), so left to a
// planner estimate a swap would build a hash table over hundreds of
// millions of transactions, in a step that measures 1.68 GiB pinned.
//
// The execution cap is 1800 s rather than the walk's 600 s, matching the
// sibling's joining arm. Gated windows measured 18.9-40.7 s on r1
// 2026-09-07 (partition 63 the slowest; the widest by MEMORY is
// partition 55, and the two are not the same window), so this is a 44x
// margin on a box that also runs galexie and the sibling rollups. The
// wider cap goes with the wider input: the streaming side grows with ALL
// transaction volume rather than with sponsorship traffic, and that
// population alone went from 318,127,649 rows in partition 55 to
// 519,663,457 in partition 63, so it is the term whose margin erodes
// fastest.
const sponsorsGatedArmSettings = boundedScanSettings +
	", query_plan_join_swap_table = 0, max_execution_time = 1800"

// sponsorEdgesSettings is the settings clause for the graph arm's one
// aggregation: the working table's 20,016,173 sponsorship operations
// re-derived through perTxCTE and collapsed to 4,088,814 distinct
// (sponsor, sponsored) pairs.
//
// Measured on r1 2026-09-09 at max_threads=2: 4.01 GiB / 28.1 s — just
// BELOW this cycle's existing peak, the board join's 4.09 GiB, so the
// graph arm does not move the cycle's ceiling.
//
// No optimize_aggregation_in_order here: the working table is ORDER BY
// (lseq, tidx, oidx) and this aggregation groups by sponsor, which is not
// a prefix of it, so there is no order to exploit. The sibling creator
// arm did once set it, where the order DID match, and that is why this
// step survived a cycle the creator one died in — in-order aggregation
// cannot spill, so it cancels the external-group-by limit above.
// The execution cap is 1800 s rather than the walk's 600 s
// because this step is not walked: a pair can span lake partitions, so a
// per-window aggregation would emit the same edge more than once.
const sponsorEdgesSettings = boundedScanSettings + ", max_execution_time = 1800"

// sponsorsRollupStatements is the full recompute cycle.
//
// Step 2 is the only pass over stellar.operations, and it is WALKED: one
// statement per 1M-ledger lake partition, each landing that partition's
// narrow, deduplicated projection of APPLIED sponsorship operations into
// the working table. Every served figure then derives from THOSE rows, so
// the board and the coverage span that qualifies it cannot describe
// different data. It reads no body_xdr — see the module doc in
// deploy/clickhouse/account_sponsors_rollup.sql for why that matters and
// how the equivalence was proven.
//
// WHY THAT PASS JOINS THE TRANSACTION. stellar.operations has no success
// gate: the lake keeps what the ledger CONTAINED, so extractOps retains
// the operations of transactions that failed (extract.go's Ops arm, by
// design). A sponsorship arrangement inside a failed transaction never
// took effect, and counting it puts arrangements on the board that were
// never entered into. Measured on r1 2026-09-07, 2,426,813 of the
// archive's 22,413,991 sponsorship operations (10.8%) sit in failed
// transactions — 47,570 of partition 63's 563,655 Begin operations (8.4%)
// and 251 of its 1,819 Revoke operations (13.8%), rising to 61.6% of
// partition 39. Ungated, the board served 11,162,397 sponsorships_started
// and 87,193 revocations_issued against true figures of 9,972,887 and
// 39,492: the revocation count was better than twice its real value, 323
// of the 2,740 ranked accounts had every operation they were credited
// with inside a failed transaction, and 2,408 of the 2,417 that survive
// the correction were ranked in the wrong place (#494).
//
// The gate is a join to stellar.transactions on the full (ledger_seq,
// tx_index) transaction identity — which is that table's whole ORDER BY
// key — rather than a filter on a column stellar.operations carries,
// because it carries none. The sibling cycle gates by pairing the
// operation with an APPLIED EFFECT instead (a CAP-67 transfer movement,
// which exists only where the creation happened), and that route was
// checked here first: it does not exist for sponsorship. Under CAP-33 the
// is-sponsoring-future-reserves-for relationship lives only for the
// duration of the transaction and is written to no ledger entry, so
// measured on r1 2026-09-07 over ledgers 63,000,000-63,010,000, 0 of
// 4,745 Begin and End operations have any stellar.ledger_entry_changes
// row at their own (ledger_seq, tx_hash, op_index) — including all 4,294
// of them that DID apply. Only Revoke leaves an entry change (2 of 2),
// and finding which entry needs the body_xdr decode this rollup exists to
// avoid. transactions.successful is therefore the only evidence of
// application there is for the two operation types that produce
// sponsorships_started.
//
// Both sides of that join are collapsed explicitly rather than trusted:
// stellar.transactions is a ReplacingMergeTree whose duplicates are
// present in bulk — over ledgers 63,000,000-63,099,999 every one of its
// 33,380,486 distinct (ledger_seq, tx_index) keys carries more than one
// row — so a join that did not collapse them would multiply each
// operation by its transaction's row count and inflate the very counts
// this gate exists to correct. The GROUP BY over the operation identity
// absorbs that multiplication, and the HAVING resolves the flag the same
// way the projection resolves its own columns: argMax over ingested_at,
// the table's version column. No key was observed carrying conflicting
// successful values, so this reads as the stricter form of the same
// answer rather than a different one.
//
// WHY THAT PASS IS WALKED. Unwalked it is one indivisible statement over
// a 24.74-billion-row, 2.18 TiB archive joined to a second one larger
// still, and everything it holds grows with the chain: its wall time, its
// dedupe hash table — one argMax state per sponsorship operation in all
// of history — and the join's build side. Measured on r1 2026-09-07 at
// max_threads=2, a single gated partition already costs 18.9 s /
// 234.85 MiB (partition 40, 164,670 applied operations), 31.5 s /
// 1.00 GiB (partition 62, 1,013,943), 40.7 s / 1.28 GiB (partition 63,
// 1,033,738) and 20.1 s / 1.68 GiB (partition 55, 2,410,901 — the widest
// by memory, and not the slowest), against a unit whose TimeoutStartSec
// is 150 min. Walking
// makes each statement's cost a function of one partition, and makes the
// cycle's progress visible per window instead of only on success or
// failure.
//
// The gate is what the walk pays for. Over the same partitions the
// ungated statement cost 29.5 s / 143.79 MiB, 26.6 s / 705.21 MiB,
// 35.0 s / 769.67 MiB and 18.6 s / 1.01 GiB. The widest window is
// partition 55 on BOTH sides, and there the gate adds 67% — 1.01 GiB to
// 1.68 GiB — which is the figure any future ceiling must be sized from,
// not the 1.28 GiB of the slowest window. It leaves 6.32 GiB of headroom
// against the 8 GiB budget. The cycle's peak is higher and is not this
// arm's: the board join of step 5 measures 4.09 GiB over the same working
// table, leaving 3.91 GiB, and this arm does not touch it — so the gate
// moved the widest WALKED window, not the cycle's ceiling. Below sponsorship's own genesis the arm
// is free rather than merely cheap: with no operation to build a hash
// table from, windows 5, 20 and 31 cost 0.01-0.29 s at under 6 MiB, so
// the 32 windows under ledger 32,747,295 add nothing measurable to the
// cycle.
//
// Grouping per window is exact rather than approximate: stellar.operations
// and stellar.transactions are both PARTITION BY
// intDiv(ledger_seq, 1000000) with ledger_seq leading their ORDER BY, so
// every row sharing an ORDER BY key shares a partition, no duplicate
// group can straddle a window boundary, and no transaction lands in a
// different window from its own operations. The window predicate is also
// a primary-key range on both tables, so it prunes granules and not only
// partitions — and it is carried TWICE because a join condition prunes
// neither side, which is why this template declares two window binds.
//
// Nothing is written to a staging arm until the walk has finished, so an
// interrupted cycle leaves the live board as the previous cycle left it,
// and the single EXCHANGE remains the only moment anything becomes
// visible.
var sponsorsRollupStatements = []rollupStep{
	{sql: `TRUNCATE TABLE stellar.account_sponsors_ops`},
	{walk: true, windowBinds: 2, sql: `INSERT INTO stellar.account_sponsors_ops (lseq, tidx, oidx, otype, src, ctime)
	 SELECT o.ledger_seq, o.tx_index, o.op_index,
	        argMax(o.op_type, o.ingested_at) AS otype,
	        argMax(o.source_account, o.ingested_at) AS src,
	        argMax(o.close_time, o.ingested_at) AS ctime
	 FROM stellar.transactions AS t
	 INNER JOIN (
	     SELECT ledger_seq, tx_index, op_index, op_type, source_account, close_time, ingested_at
	     FROM stellar.operations
	     WHERE ledger_seq BETWEEN ? AND ?
	       AND op_type IN ('` + opBeginSponsoring + `', '` + opEndSponsoring + `', '` + opRevokeSponsoring + `')
	 ) AS o ON t.ledger_seq = o.ledger_seq AND t.tx_index = o.tx_index
	 WHERE t.ledger_seq BETWEEN ? AND ?
	 GROUP BY o.ledger_seq, o.tx_index, o.op_index
	 HAVING argMax(t.successful, t.ingested_at) = 1
	 ` + sponsorsGatedArmSettings},
	{sql: `TRUNCATE TABLE stellar.account_sponsors_rollup_staging`},
	{sql: `TRUNCATE TABLE stellar.account_sponsors_stats_staging`},
	{sql: `INSERT INTO stellar.account_sponsors_rollup_staging
	     (rank, sponsor, sponsorships_started, distinct_sponsored, revocations_issued,
	      first_ledger, last_ledger, first_seen_at, last_seen_at)
	 WITH` + perTxCTE + `,` + factsCTE + `
	 SELECT row_number() OVER (ORDER BY sponsorships_started DESC, sponsor) AS rank,
	        sponsor, sponsorships_started, distinct_sponsored, revocations_issued,
	        first_ledger, last_ledger, first_seen_at, last_seen_at
	 FROM (
	     SELECT src AS sponsor,
	            toUInt64(sumIf(w, kind = 'begin')) AS sponsorships_started,
	            toUInt64(uniqExactIf(counterparty, kind = 'end')) AS distinct_sponsored,
	            toUInt64(sumIf(w, kind = 'revoke')) AS revocations_issued,
	            min(lseq) AS first_ledger,
	            max(lseq) AS last_ledger,
	            toDateTime(min(ctime), 'UTC') AS first_seen_at,
	            toDateTime(max(ctime), 'UTC') AS last_seen_at
	     FROM facts
	     GROUP BY sponsor
	 )
	 ` + boundedScanSettings + `, max_execution_time = 3600`},
	{sql: `INSERT INTO stellar.account_sponsors_stats_staging (metric, value)
	 WITH` + perTxCTE + `
	 SELECT metric, value FROM (
	     SELECT 'sponsors_total' AS metric, toInt64(count()) AS value
	     FROM stellar.account_sponsors_rollup_staging
	     UNION ALL
	     SELECT 'sponsorships_total', toInt64(sum(sponsorships_started))
	     FROM stellar.account_sponsors_rollup_staging
	     UNION ALL
	     SELECT 'distinct_sponsored_total', toInt64(sum(distinct_sponsored))
	     FROM stellar.account_sponsors_rollup_staging
	     UNION ALL
	     SELECT 'revocations_total', toInt64(sum(revocations_issued))
	     FROM stellar.account_sponsors_rollup_staging
	     UNION ALL
	     SELECT 'ambiguous_txs', toInt64(countIf(n_sponsors > 1))
	     FROM per_tx
	     UNION ALL
	     SELECT 'from_ledger', toInt64(min(lseq)) FROM stellar.account_sponsors_ops
	     UNION ALL
	     SELECT 'thru_ledger', toInt64(max(lseq)) FROM stellar.account_sponsors_ops
	     UNION ALL
	     SELECT 'from_time', toInt64(toUnixTimestamp(min(ctime))) FROM stellar.account_sponsors_ops
	     UNION ALL
	     SELECT 'thru_time', toInt64(toUnixTimestamp(max(ctime))) FROM stellar.account_sponsors_ops
	 )
	 ` + boundedScanSettings + `, max_execution_time = 900`},
	// ── The graph arm (#351) ───────────────────────────────────────
	// The board says WHO sponsored the most. These two say WHOM — one
	// row per distinct (sponsor, sponsored) pair, in both sort orders so
	// each direction is a primary-key range read. Derived from the same
	// per-transaction attribution the board uses and from the same
	// working table, so the two cannot describe different data: measured
	// on r1 2026-09-09 the pair count equals the board's
	// distinct_sponsored_total (4,088,814) and the event sum equals its
	// sponsorships_total (9,987,381), exactly.
	{sql: `TRUNCATE TABLE stellar.account_sponsor_edges_staging`},
	{sql: `TRUNCATE TABLE stellar.account_sponsor_edges_by_sponsored_staging`},
	{sql: `INSERT INTO stellar.account_sponsor_edges_staging
	     (sponsor, sponsored, sponsorships_started, first_ledger, last_ledger, first_at, last_at)
	 WITH` + perTxCTE + `
	 SELECT sponsor, sponsored,
	        toUInt64(count()) AS sponsorships_started,
	        min(lseq) AS first_ledger,
	        max(lseq) AS last_ledger,
	        toDateTime(min(ctime), 'UTC') AS first_at,
	        toDateTime(max(ctime), 'UTC') AS last_at
	 FROM (
	     SELECT sponsor, arrayJoin(sponsored_set) AS sponsored, lseq, ctime
	     FROM per_tx WHERE n_sponsors = 1
	 )
	 GROUP BY sponsor, sponsored
	 ` + sponsorEdgesSettings},
	// Reverse ordering filled FROM the staging arm just written, so the
	// per-transaction attribution is derived once per cycle rather than
	// twice.
	{sql: `INSERT INTO stellar.account_sponsor_edges_by_sponsored_staging
	     (sponsored, sponsor, sponsorships_started, first_ledger, last_ledger, first_at, last_at)
	 SELECT sponsored, sponsor, sponsorships_started, first_ledger, last_ledger, first_at, last_at
	 FROM stellar.account_sponsor_edges_staging
	 ` + boundedScanSettings + `, max_execution_time = 1800`},
	// Board, the span that qualifies it, and the graph it decomposes into
	// swap in one metadata transaction — a board beside a stale span is
	// the overstatement this surface exists to avoid, and a graph beside
	// a board from a different cycle is the same defect one level down.
	{sql: `EXCHANGE TABLES stellar.account_sponsors_rollup_staging AND stellar.account_sponsors_rollup,
	                 stellar.account_sponsors_stats_staging AND stellar.account_sponsors_stats,
	                 stellar.account_sponsor_edges_staging AND stellar.account_sponsor_edges,
	                 stellar.account_sponsor_edges_by_sponsored_staging AND stellar.account_sponsor_edges_by_sponsored`},
}

// RunSponsorsRollup executes one full recompute + atomic exchange.
func RunSponsorsRollup(ctx context.Context, addr string, logf func(format string, args ...any)) error {
	return runRollupCycle(ctx, addr, "sponsors rollup", sponsorsRollupStatements, logf)
}

// AccountSponsors reads the rollup snapshot: the top `limit` rows plus
// the cycle's totals and covered span. ok=false (not an error) when the
// rollup has not completed a cycle, or carries a board with no span to
// qualify it.
func (r *ExplorerReader) AccountSponsors(ctx context.Context, limit int) (AccountSponsors, bool, error) {
	if !r.probeSchema(ctx, &r.accountSponsorsProbe,
		`SELECT rank FROM stellar.account_sponsors_rollup LIMIT 1`, true) {
		return AccountSponsors{}, false, nil
	}
	var out AccountSponsors
	if err := r.readSponsorsBoard(ctx, &out, limit); err != nil {
		return AccountSponsors{}, false, err
	}
	if err := r.readSponsorsStats(ctx, &out); err != nil {
		return AccountSponsors{}, false, err
	}
	if out.ThruLedger == 0 {
		return AccountSponsors{}, false, nil
	}
	return out, true, nil
}

func (r *ExplorerReader) readSponsorsBoard(ctx context.Context, out *AccountSponsors, limit int) error {
	rows, err := r.conn.Query(ctx, `
		SELECT rank, sponsor, sponsorships_started, distinct_sponsored, revocations_issued,
		       first_ledger, last_ledger, first_seen_at, last_seen_at, computed_at
		FROM stellar.account_sponsors_rollup ORDER BY rank LIMIT ?`, limit)
	if err != nil {
		return fmt.Errorf("clickhouse: account sponsors board: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			row        AccountSponsorRow
			computedAt time.Time
		)
		if err := rows.Scan(&row.Rank, &row.Sponsor, &row.SponsorshipsStarted, &row.DistinctSponsored,
			&row.RevocationsIssued, &row.FirstLedger, &row.LastLedger,
			&row.FirstSeenAt, &row.LastSeenAt, &computedAt); err != nil {
			return fmt.Errorf("clickhouse: scan account sponsor: %w", err)
		}
		out.ComputedAt = computedAt
		out.Board = append(out.Board, row)
	}
	return rows.Err()
}

func (r *ExplorerReader) readSponsorsStats(ctx context.Context, out *AccountSponsors) error {
	rows, err := r.conn.Query(ctx, `SELECT metric, value FROM stellar.account_sponsors_stats`)
	if err != nil {
		return fmt.Errorf("clickhouse: account sponsors stats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			metric string
			value  int64
		)
		if err := rows.Scan(&metric, &value); err != nil {
			return fmt.Errorf("clickhouse: scan account sponsors stat: %w", err)
		}
		switch metric {
		case "sponsors_total":
			out.SponsorsTotal = value
		case "sponsorships_total":
			out.SponsorshipsTotal = value
		case "distinct_sponsored_total":
			out.DistinctSponsoredTotal = value
		case "revocations_total":
			out.RevocationsTotal = value
		case "ambiguous_txs":
			out.AmbiguousTxs = value
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
