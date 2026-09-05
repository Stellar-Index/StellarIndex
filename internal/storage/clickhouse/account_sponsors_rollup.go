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

// sponsorsRollupStatements is the full recompute cycle.
//
// Step 2 is the only pass over stellar.operations. It lands a narrow,
// deduplicated projection of sponsorship operations into a working
// table; every served figure then derives from THOSE rows, so the board
// and the coverage span that qualifies it cannot describe different
// data. It reads no body_xdr — see the module doc in
// deploy/clickhouse/account_sponsors_rollup.sql for why that matters
// and how the equivalence was proven.
var sponsorsRollupStatements = []string{
	`TRUNCATE TABLE stellar.account_sponsors_ops`,
	`INSERT INTO stellar.account_sponsors_ops (lseq, tidx, oidx, otype, src, ctime)
	 SELECT ledger_seq, tx_index, op_index,
	        argMax(op_type, ingested_at) AS otype,
	        argMax(source_account, ingested_at) AS src,
	        argMax(close_time, ingested_at) AS ctime
	 FROM stellar.operations
	 WHERE op_type IN ('` + opBeginSponsoring + `', '` + opEndSponsoring + `', '` + opRevokeSponsoring + `')
	 GROUP BY ledger_seq, tx_index, op_index
	 SETTINGS max_threads = 4, max_memory_usage = 8589934592,
	          max_bytes_before_external_group_by = 4000000000, max_execution_time = 7200`,
	`TRUNCATE TABLE stellar.account_sponsors_rollup_staging`,
	`TRUNCATE TABLE stellar.account_sponsors_stats_staging`,
	`INSERT INTO stellar.account_sponsors_rollup_staging
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
	 SETTINGS max_threads = 4, max_memory_usage = 8589934592,
	          max_bytes_before_external_group_by = 4000000000,
	          max_bytes_before_external_sort = 4000000000, max_execution_time = 3600`,
	`INSERT INTO stellar.account_sponsors_stats_staging (metric, value)
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
	 SETTINGS max_threads = 4, max_memory_usage = 8589934592, max_execution_time = 900`,
	// Board and the span that qualifies it swap in one metadata
	// transaction — a board beside a stale span is the overstatement this
	// surface exists to avoid.
	`EXCHANGE TABLES stellar.account_sponsors_rollup_staging AND stellar.account_sponsors_rollup,
	                 stellar.account_sponsors_stats_staging AND stellar.account_sponsors_stats`,
}

// RunSponsorsRollup executes one full recompute + atomic exchange.
func RunSponsorsRollup(ctx context.Context, addr string, logf func(format string, args ...any)) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	for i, stmt := range sponsorsRollupStatements {
		if err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("clickhouse: sponsors rollup step %d/%d: %w", i+1, len(sponsorsRollupStatements), err)
		}
		logf("step %d/%d done", i+1, len(sponsorsRollupStatements))
	}
	return nil
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
