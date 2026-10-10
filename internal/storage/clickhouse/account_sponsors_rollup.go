package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Sponsorship operation type names as stellar.operations records them.
const (
	opBeginSponsoring  = "OperationTypeBeginSponsoringFutureReserves"
	opEndSponsoring    = "OperationTypeEndSponsoringFutureReserves"
	opRevokeSponsoring = "OperationTypeRevokeSponsorship"
)

// AccountSponsorRow is one league-table row. Every figure is IMMUTABLE HISTORY (arrangements begun,
// revocations issued), not sponsorships still standing: an arrangement also ends when the entry
// goes away (trustline removed, offer cancelled, account merged) and no operation records that.
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

// AccountSponsors is one cycle's snapshot: a board slice, the totals and the span aggregated.
// It deliberately carries NO live sponsored set: figures come from replaying sponsorship
// OPERATIONS, and a sponsorship also lapses when the entry is deleted or the sponsored account
// merges, with no operation. A "currently sponsoring" number from this source would OVERSTATE;
// observing it needs sponsoringID inside ledger_entries_current's entry_xdr, a separate projection
// that is not served.
// FromLedger/ThruLedger are data-derived (ADR-0031); the floor at protocol 14 is sponsorship's own
// genesis, not a gap.
type AccountSponsors struct {
	Board                  []AccountSponsorRow
	SponsorsTotal          int64
	SponsorshipsTotal      int64
	DistinctSponsoredTotal int64
	RevocationsTotal       int64
	// AmbiguousTxs counts transactions with more than one distinct sponsor, excluded from
	// per-sponsor attribution; published so the exclusion is visible.
	AmbiguousTxs int64
	FromLedger   uint32
	ThruLedger   uint32
	FromTime     time.Time
	ThruTime     time.Time
	ComputedAt   time.Time
}

// perTxCTE resolves each transaction's sponsor and sponsored set; a sandwich's End op is sourced by
// the SPONSORED account, so no body decode is needed.
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

// factsCTE explodes per_tx into one row per countable fact; only single-sponsor transactions
// attribute (the rest are counted as ambiguous by the stats arm).
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

// sponsorsGatedArmSettings is the settings for the cycle's one walked step, which joins two lake
// archives.
// query_plan_join_swap_table = 0 PINS the build side to the window's sponsorship operations (before
// the gate drops failed ones; at most ~2.5 M rows) against that window's transactions (hundreds of
// millions, 128x to 460x larger), so a planner swap cannot build a hash table over the
// transactions. The step measures 1.68 GiB pinned.
// The 1800 s cap (not the walk's 600 s) matches the sibling's joining arm: gated windows run 19-41
// s, and the streaming side grows with ALL transaction volume, the term whose margin erodes
// fastest.
const sponsorsGatedArmSettings = boundedScanSettings +
	", query_plan_join_swap_table = 0, max_execution_time = 1800"

// sponsorEdgesSettings is the graph arm's one aggregation (sponsorship ops collapsed to distinct
// (sponsor, sponsored) pairs); peak ~4 GiB, just below the board join's, so it does not raise the
// cycle's ceiling.
// No optimize_aggregation_in_order: the working table is ORDER BY (lseq, tidx, oidx) and this
// groups by sponsor, so there is no order to exploit, and in-order aggregation cannot spill, so it
// cancels the external-group-by limit (it killed the sibling creator cycle).
// The 1800 s cap because this step is not walked: a pair can span lake partitions, so a per-window
// aggregation would emit an edge more than once.
const sponsorEdgesSettings = boundedScanSettings + ", max_execution_time = 1800"

// sponsorsRollupStatements is the full recompute: WALK stellar.operations one 1M-ledger lake
// partition at a time into the working table (narrow, deduplicated, APPLIED sponsorship ops, no
// body_xdr; see deploy/clickhouse/account_sponsors_rollup.sql), derive every served figure from
// those rows so the board and its coverage span cannot describe different data, then EXCHANGE
// atomically. An interrupted cycle leaves the previous board.
// The walk joins the transaction because stellar.operations has no success gate: it keeps
// operations of FAILED transactions (extractOps, by design), and an arrangement in a failed
// transaction never took effect (~10.8% of the archive's sponsorship ops; ungated,
// revocations_issued was over twice its real value). Under CAP-33 the sponsoring relationship
// writes no ledger entry, so transactions.successful is the only evidence of application for
// Begin and End.
// The join is on the full (ledger_seq, tx_index) identity, the transactions table's whole ORDER BY
// key. Both sides are collapsed explicitly: stellar.transactions is a ReplacingMergeTree whose rows
// are duplicated in bulk, so an uncollapsed join would multiply each operation by its transaction's
// row count. The GROUP BY over operation identity absorbs that and HAVING resolves `successful` by
// argMax over ingested_at, like the projection.
// Walked because unwalked it is one statement over a 2 TiB archive joined to a larger one; wall
// time, the dedupe table and the join build side all grow with the chain. Widest measured peak per
// window is 1.68 GiB against the 8 GiB budget; the board join (4.09 GiB) is the cycle's ceiling.
// Per-window grouping is exact: both tables are PARTITION BY intDiv(ledger_seq, 1000000) with
// ledger_seq leading ORDER BY, so no duplicate group or transaction straddles a window. The window
// predicate is carried TWICE because a join condition prunes neither side, hence two window binds.
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
	 WITH` + perTxCTE + `,` + factsCTE + `
	 SELECT metric, value FROM (
	     SELECT 'sponsors_total' AS metric, toInt64(count()) AS value
	     FROM stellar.account_sponsors_rollup_staging
	     UNION ALL
	     SELECT 'sponsorships_total', toInt64(sum(sponsorships_started))
	     FROM stellar.account_sponsors_rollup_staging
	     UNION ALL
	     SELECT 'distinct_sponsored_total', toInt64(uniqExactIf(counterparty, kind = 'end'))
	     FROM facts
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
	// Graph arm. The board says WHO sponsored the most; these say WHOM, one row per distinct
	// (sponsor, sponsored) pair in both sort orders so each direction is a primary-key range read.
	// Same per-transaction attribution and working table as the board, so the event sum equals
	// sponsorships_total exactly. The pair count is NOT distinct_sponsored_total: an account
	// sponsored by several sponsors adds a pair per sponsor but counts once there (a global
	// distinct).
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
	// The reverse ordering is filled from the staging arm, deriving the attribution once per cycle.
	{sql: `INSERT INTO stellar.account_sponsor_edges_by_sponsored_staging
	     (sponsored, sponsor, sponsorships_started, first_ledger, last_ledger, first_at, last_at)
	 SELECT sponsored, sponsor, sponsorships_started, first_ledger, last_ledger, first_at, last_at
	 FROM stellar.account_sponsor_edges_staging
	 ` + boundedScanSettings + `, max_execution_time = 1800`},
	// Swap board, span and graph in one metadata transaction, so none is served beside another
	// cycle's.
	{sql: `EXCHANGE TABLES stellar.account_sponsors_rollup_staging AND stellar.account_sponsors_rollup,
	                 stellar.account_sponsors_stats_staging AND stellar.account_sponsors_stats,
	                 stellar.account_sponsor_edges_staging AND stellar.account_sponsor_edges,
	                 stellar.account_sponsor_edges_by_sponsored_staging AND stellar.account_sponsor_edges_by_sponsored`},
}

// RunSponsorsRollup executes one full recompute + atomic exchange.
func RunSponsorsRollup(ctx context.Context, addr string, logf func(format string, args ...any)) error {
	return runRollupCycle(ctx, addr, "sponsors rollup", sponsorsRollupStatements, logf)
}

// AccountSponsors reads the rollup snapshot: the top `limit` rows plus totals and span. ok=false
// (not an error) when no cycle has completed or the board has no span to qualify it.
// A non-empty `account` narrows Board to that sponsor's row; see ExplorerReader.AccountCreators
// (rank is whole-aggregation, and an address past the cap would read as absent, not unranked).
func (r *ExplorerReader) AccountSponsors(ctx context.Context, limit int, account string) (AccountSponsors, bool, error) {
	if !r.probeSchema(ctx, &r.accountSponsorsProbe,
		`SELECT rank FROM stellar.account_sponsors_rollup LIMIT 1`, true) {
		return AccountSponsors{}, false, nil
	}
	var out AccountSponsors
	if err := r.readSponsorsBoard(ctx, &out, limit, account); err != nil {
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

const sponsorsBoardCols = `rank, sponsor, sponsorships_started, distinct_sponsored, revocations_issued,
	       first_ledger, last_ledger, first_seen_at, last_seen_at, computed_at`

// sponsorsBoardKeyedSQL is the ?account= read: one row per sponsor with its whole-aggregation rank,
// served by the idx_sponsors_rollup_sponsor skip index (the table is ORDER BY rank).
const sponsorsBoardKeyedSQL = `SELECT ` + sponsorsBoardCols + `
	FROM stellar.account_sponsors_rollup WHERE sponsor = ?`

func (r *ExplorerReader) readSponsorsBoard(ctx context.Context, out *AccountSponsors, limit int, account string) error {
	var (
		rows driver.Rows
		err  error
	)
	if account != "" {
		rows, err = r.conn.Query(ctx, sponsorsBoardKeyedSQL, account)
	} else {
		rows, err = r.conn.Query(ctx, `
		SELECT `+sponsorsBoardCols+`
		FROM stellar.account_sponsors_rollup ORDER BY rank LIMIT ?`, limit)
	}
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

// readSponsorsStats runs after readSponsorsBoard: when the board scanned no
// row (a keyed miss), the cycle's time comes from the stats rows.
func (r *ExplorerReader) readSponsorsStats(ctx context.Context, out *AccountSponsors) error {
	rows, err := r.conn.Query(ctx, `SELECT metric, value, computed_at FROM stellar.account_sponsors_stats`)
	if err != nil {
		return fmt.Errorf("clickhouse: account sponsors stats: %w", err)
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
			return fmt.Errorf("clickhouse: scan account sponsors stat: %w", err)
		}
		statsAt = laterTime(statsAt, computedAt)
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
	if out.ComputedAt.IsZero() {
		out.ComputedAt = statsAt
	}
	return rows.Err()
}
