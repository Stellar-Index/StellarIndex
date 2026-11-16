package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// LendingClaimTotals is one claim stream's totals over its whole history and over the 30 days ending at the as-of time.
type LendingClaimTotals struct {
	Claims, Claims30d     int64
	Claimers, Claimers30d int64
	Amount, Amount30d     canonical.Amount // base units of the claimed amount, summed as NUMERIC
}

// BLNTBackstopClaimTotals is one BLNT backstop tier's claims for a pool: Amount is the BLNT claimed, LP the backstop
// tokens it bought, and Shares the tier shares the claim minted to the claimer.
type BLNTBackstopClaimTotals struct {
	Tier string
	LendingClaimTotals
	LP, LP30d         canonical.Amount
	Shares, Shares30d canonical.Amount
}

// LendingEmissionClaims is a pool's emission claims; Backstop is nil for Blend, whose backstop claim event names no pool.
type LendingEmissionClaims struct {
	Pool     LendingClaimTotals
	Backstop []BLNTBackstopClaimTotals
}

// claimWindow is the 30-day window (asOf - 30 days, asOf]; $2 is the as-of time.
const claimWindow = `ledger_close_time > $2::timestamptz - INTERVAL '30 days'`

// claimTotalsCols selects the LendingClaimTotals columns in scan order.
const claimTotalsCols = `count(*), count(*) FILTER (WHERE ` + claimWindow + `),
		count(DISTINCT user_address), count(DISTINCT user_address) FILTER (WHERE ` + claimWindow + `),
		COALESCE(sum(amount), 0)::text, COALESCE(sum(amount) FILTER (WHERE ` + claimWindow + `), 0)::text`

const (
	blendPoolClaimsSQL = `SELECT ` + claimTotalsCols + `
		  FROM blend_emissions
		 WHERE pool = $1 AND event_kind = 'claim' AND ledger_close_time <= $2::timestamptz`
	blntPoolClaimsSQL = `SELECT ` + claimTotalsCols + `
		  FROM blnt_events
		 WHERE contract_id = $1 AND contract_role = 'pool' AND event_kind = 'claim' AND ledger_close_time <= $2::timestamptz`
	blntBackstopClaimsSQL = `SELECT tier, ` + claimTotalsCols + `,
		       COALESCE(sum(amount2), 0)::text, COALESCE(sum(amount2) FILTER (WHERE ` + claimWindow + `), 0)::text,
		       COALESCE(sum((attributes->>'shares')::numeric), 0)::text,
		       COALESCE(sum((attributes->>'shares')::numeric) FILTER (WHERE ` + claimWindow + `), 0)::text
		  FROM blnt_events
		 WHERE pool = $1 AND contract_role = 'backstop' AND event_kind = 'claim' AND tier IS NOT NULL
		   AND ledger_close_time <= $2::timestamptz
		 GROUP BY tier ORDER BY tier`
)

func (t *LendingClaimTotals) dests() []any {
	return []any{&t.Claims, &t.Claims30d, &t.Claimers, &t.Claimers30d, &t.Amount, &t.Amount30d}
}

// LendingEmissionClaims totals a pool's emission claims up to asOf: Blend's from blend_emissions, BLNT's pool and
// per-tier backstop claims from blnt_events.
func (s *Store) LendingEmissionClaims(ctx context.Context, pool string, isBLNT bool, asOf time.Time) (LendingEmissionClaims, error) {
	q := blendPoolClaimsSQL
	if isBLNT {
		q = blntPoolClaimsSQL
	}
	var out LendingEmissionClaims
	if err := s.db.QueryRowContext(ctx, q, pool, asOf.UTC()).Scan(out.Pool.dests()...); err != nil {
		return LendingEmissionClaims{}, fmt.Errorf("timescale: LendingEmissionClaims pool %s: %w", pool, err)
	}
	if !isBLNT {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, blntBackstopClaimsSQL, pool, asOf.UTC())
	if err != nil {
		return LendingEmissionClaims{}, fmt.Errorf("timescale: LendingEmissionClaims backstop %s: %w", pool, err)
	}
	defer func() { _ = rows.Close() }()
	out.Backstop, err = scanBackstopClaims(rows)
	if err != nil {
		return LendingEmissionClaims{}, fmt.Errorf("timescale: LendingEmissionClaims backstop %s: %w", pool, err)
	}
	return out, nil
}

func scanBackstopClaims(rows *sql.Rows) ([]BLNTBackstopClaimTotals, error) {
	out := []BLNTBackstopClaimTotals{}
	for rows.Next() {
		var b BLNTBackstopClaimTotals
		dst := append([]any{&b.Tier}, b.dests()...)
		dst = append(dst, &b.LP, &b.LP30d, &b.Shares, &b.Shares30d)
		if err := rows.Scan(dst...); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return out, nil
}
