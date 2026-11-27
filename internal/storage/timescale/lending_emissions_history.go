package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// EmissionScheduleEvent is one reserve_emission_update: from At the token id emits EPS (at 10^7) until Expiration (unix s).
type EmissionScheduleEvent struct {
	TokenID    uint32
	At         time.Time
	EPS        canonical.Amount
	Expiration canonical.Amount
}

// Claim scopes: a bucket's pool total, and its claims split by how many reserve token ids each names.
const (
	EmissionClaimScopePool    = "pool"
	EmissionClaimScopeSingle  = "single"  // names exactly one token id, carried in TokenID
	EmissionClaimScopeSeveral = "several" // names two or more: one amount for all, not splittable
	EmissionClaimScopeNone    = "none"    // names no token id
)

// EmissionClaimBucket is one bucket's pool claims in one scope. Amount sums amount_claimed; AmountUnread counts NULL amounts.
type EmissionClaimBucket struct {
	Start        time.Time
	Scope        string
	TokenID      *uint32
	Claims       int64
	Claimers     int64
	Amount       canonical.Amount
	AmountUnread int64
}

// EmissionAllocBucket is one bucket's backstop gulp_emissions naming the pool; Unread counts events missing either amount.
type EmissionAllocBucket struct {
	Start         time.Time
	Events        int64
	BackstopShare canonical.Amount // new_backstop_emissions
	PoolShare     canonical.Amount // new_pool_emissions
	Unread        int64
}

// EmissionBackstopClaimBucket is one bucket's BLNT backstop claims for the pool in one tier.
type EmissionBackstopClaimBucket struct {
	Start    time.Time
	Tier     string
	Claims   int64
	Claimers int64
	BLNT     canonical.Amount // blnt_amount, the emission token claimed
	LP       canonical.Amount // lp_amount, the backstop LP tokens it bought
	Shares   canonical.Amount // tier shares minted to the claimer
	Unread   int64            // claims missing any of the three
}

// EmissionHistoryQuery is the window [From, To) at hourly or daily UTC buckets, and which streams to read.
type EmissionHistoryQuery struct {
	From, To                               time.Time
	Daily                                  bool
	Schedule, BackstopAlloc, BackstopClaim bool
}

// LendingEmissionsHistory is a pool's emission events over a window. Schedule also holds, per token id, the last update at
// or before From: the rate in force when the window opens.
type LendingEmissionsHistory struct {
	Schedule       []EmissionScheduleEvent
	Claims         []EmissionClaimBucket
	Alloc          []EmissionAllocBucket
	BackstopClaims []EmissionBackstopClaimBucket
}

// The row sources, each led by an index on (pool or contract_id, ledger_close_time DESC).
const (
	blendPoolEventsFrom     = `FROM blend_emissions WHERE pool = $1 AND event_kind = `
	blntPoolEventsFrom      = `FROM blnt_events WHERE contract_id = $1 AND contract_role = 'pool' AND event_kind = `
	blendBackstopEventsFrom = `FROM blend_backstop_events WHERE pool = $1 AND event_kind = `
	blntBackstopEventsFrom  = `FROM blnt_events WHERE pool = $1 AND contract_role = 'backstop' AND event_kind = `
	emissionBucket          = `date_trunc($4::text, ledger_close_time, 'UTC')`
	emissionWindow          = ` AND ledger_close_time >= $2::timestamptz AND ledger_close_time < $3::timestamptz`
)

// emissionScheduleSQL reads every update in the window plus, per token id, the last one at or before From.
func emissionScheduleSQL(from string) string {
	return `WITH ev AS (
	SELECT (attributes->>'res_token_id')::bigint AS token_id, ledger_close_time AS t, ledger, event_index,
	       (attributes->>'eps')::numeric AS eps, (attributes->>'expiration')::numeric AS exp
	  ` + from + `'reserve_emission_update' AND ledger_close_time < $3::timestamptz
	   AND attributes ? 'res_token_id' AND attributes ? 'eps' AND attributes ? 'expiration'
)
(SELECT DISTINCT ON (token_id) token_id, t, ledger, event_index, eps::text, exp::text
   FROM ev WHERE t <= $2::timestamptz ORDER BY token_id, t DESC, ledger DESC, event_index DESC)
UNION ALL
(SELECT token_id, t, ledger, event_index, eps::text, exp::text FROM ev WHERE t > $2::timestamptz)
ORDER BY 1, 2, 3, 4`
}

func emissionClaimsSQL(from string) string {
	return `WITH c AS (
	SELECT ` + emissionBucket + ` AS b, user_address, amount,
	       CASE WHEN jsonb_typeof(attributes->'reserve_token_ids') = 'array'
	            THEN jsonb_array_length(attributes->'reserve_token_ids') ELSE 0 END AS n,
	       attributes->'reserve_token_ids'->>0 AS first_id
	  ` + from + `'claim'` + emissionWindow + `
), k AS (
	SELECT b, user_address, amount,
	       CASE WHEN n = 1 THEN 'single' WHEN n > 1 THEN 'several' ELSE 'none' END AS scope,
	       CASE WHEN n = 1 THEN first_id::bigint END AS token_id
	  FROM c
)
SELECT b, CASE WHEN GROUPING(scope) = 1 THEN 'pool' ELSE scope END, token_id,
       count(*), count(DISTINCT user_address), COALESCE(sum(amount), 0)::text, count(*) FILTER (WHERE amount IS NULL)
  FROM k
 GROUP BY GROUPING SETS ((b), (b, scope, token_id))
 ORDER BY 1, 2, 3`
}

func emissionAllocSQL(from string) string {
	return `SELECT ` + emissionBucket + ` AS b, count(*), COALESCE(sum(amount), 0)::text, COALESCE(sum(amount2), 0)::text,
	       count(*) FILTER (WHERE amount IS NULL OR amount2 IS NULL)
	  ` + from + `'gulp_emissions'` + emissionWindow + `
	 GROUP BY 1 ORDER BY 1`
}

const blntBackstopClaimHistorySQL = `SELECT ` + emissionBucket + ` AS b, tier, count(*), count(DISTINCT user_address),
	       COALESCE(sum(amount), 0)::text, COALESCE(sum(amount2), 0)::text,
	       COALESCE(sum((attributes->>'shares')::numeric), 0)::text,
	       count(*) FILTER (WHERE amount IS NULL OR amount2 IS NULL OR attributes->>'shares' IS NULL)
	  ` + blntBackstopEventsFrom + `'claim' AND tier IS NOT NULL` + emissionWindow + `
	 GROUP BY 1, 2 ORDER BY 1, 2`

// LendingEmissionScheduleQuery is the schedule read behind [Store.LendingEmissionsHistory], exported for the plan check.
func LendingEmissionScheduleQuery(isBLNT bool) string {
	return emissionScheduleSQL(poolEventsFrom(isBLNT))
}

// LendingEmissionClaimsQuery is the claim read behind [Store.LendingEmissionsHistory], exported for the plan check.
func LendingEmissionClaimsQuery(isBLNT bool) string { return emissionClaimsSQL(poolEventsFrom(isBLNT)) }

func poolEventsFrom(isBLNT bool) string {
	if isBLNT {
		return blntPoolEventsFrom
	}
	return blendPoolEventsFrom
}

// LendingEmissionsHistory reads a pool's emission events over q: Blend's from blend_emissions and blend_backstop_events,
// BLNT's from blnt_events. Every amount is summed as NUMERIC and scanned exact.
func (s *Store) LendingEmissionsHistory(ctx context.Context, pool string, isBLNT bool, q EmissionHistoryQuery) (LendingEmissionsHistory, error) {
	poolFrom, bstopFrom := poolEventsFrom(isBLNT), blendBackstopEventsFrom
	if isBLNT {
		bstopFrom = blntBackstopEventsFrom
	}
	unit := "hour"
	if q.Daily {
		unit = "day"
	}
	from, to := q.From.UTC(), q.To.UTC()
	var out LendingEmissionsHistory
	var err error
	if q.Schedule {
		if out.Schedule, err = queryEmissionRows(ctx, s.db, emissionScheduleSQL(poolFrom), scanScheduleEvent, pool, from, to); err != nil {
			return LendingEmissionsHistory{}, fmt.Errorf("timescale: LendingEmissionsHistory schedule %s: %w", pool, err)
		}
	}
	if out.Claims, err = queryEmissionRows(ctx, s.db, emissionClaimsSQL(poolFrom), scanClaimBucket, pool, from, to, unit); err != nil {
		return LendingEmissionsHistory{}, fmt.Errorf("timescale: LendingEmissionsHistory claims %s: %w", pool, err)
	}
	if q.BackstopAlloc {
		if out.Alloc, err = queryEmissionRows(ctx, s.db, emissionAllocSQL(bstopFrom), scanAllocBucket, pool, from, to, unit); err != nil {
			return LendingEmissionsHistory{}, fmt.Errorf("timescale: LendingEmissionsHistory alloc %s: %w", pool, err)
		}
	}
	if q.BackstopClaim && isBLNT {
		if out.BackstopClaims, err = queryEmissionRows(ctx, s.db, blntBackstopClaimHistorySQL, scanBackstopClaimBucket, pool, from, to, unit); err != nil {
			return LendingEmissionsHistory{}, fmt.Errorf("timescale: LendingEmissionsHistory backstop claims %s: %w", pool, err)
		}
	}
	return out, nil
}

func queryEmissionRows[T any](ctx context.Context, db *sql.DB, q string, scan func(*sql.Rows) (T, error), args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return out, nil
}

func scanScheduleEvent(rows *sql.Rows) (EmissionScheduleEvent, error) {
	var e EmissionScheduleEvent
	var id int64
	var ledger, eventIndex int
	if err := rows.Scan(&id, &e.At, &ledger, &eventIndex, &e.EPS, &e.Expiration); err != nil {
		return e, err
	}
	if id < 0 || id > 1<<32-1 {
		return e, fmt.Errorf("res_token_id %d out of u32", id)
	}
	e.TokenID, e.At = uint32(id), e.At.UTC()
	return e, nil
}

func scanClaimBucket(rows *sql.Rows) (EmissionClaimBucket, error) {
	var b EmissionClaimBucket
	var id sql.NullInt64
	if err := rows.Scan(&b.Start, &b.Scope, &id, &b.Claims, &b.Claimers, &b.Amount, &b.AmountUnread); err != nil {
		return b, err
	}
	if id.Valid {
		if id.Int64 < 0 || id.Int64 > 1<<32-1 {
			return b, fmt.Errorf("reserve token id %d out of u32", id.Int64)
		}
		v := uint32(id.Int64)
		b.TokenID = &v
	}
	b.Start = b.Start.UTC()
	return b, nil
}

func scanAllocBucket(rows *sql.Rows) (EmissionAllocBucket, error) {
	var b EmissionAllocBucket
	err := rows.Scan(&b.Start, &b.Events, &b.BackstopShare, &b.PoolShare, &b.Unread)
	b.Start = b.Start.UTC()
	return b, err
}

func scanBackstopClaimBucket(rows *sql.Rows) (EmissionBackstopClaimBucket, error) {
	var b EmissionBackstopClaimBucket
	err := rows.Scan(&b.Start, &b.Tier, &b.Claims, &b.Claimers, &b.BLNT, &b.LP, &b.Shares, &b.Unread)
	b.Start = b.Start.UTC()
	return b, err
}
