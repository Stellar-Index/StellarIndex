package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// LendingRevenueUSDSpan is one pool's days to sum, [FromDay, ThroughDay], bucketed back from WindowEnd.
type LendingRevenueUSDSpan struct {
	Pool                           string
	FromDay, ThroughDay, WindowEnd time.Time
}

// LendingRevenueUSDBucketSum is one (pool, asset, bucket, current) group of lending_revenue_usd_daily. Bucket counts back
// from the span's WindowEnd: 0 that day, 1 the six before it, 2 the 23 before those, 3 every earlier day. Current is false
// for rows of another valuation or derive revision. Legs are exact decimal sums in the stored order, nil when no row of
// the group is priced.
type LendingRevenueUSDBucketSum struct {
	Pool, Asset string
	Bucket      int
	Current     bool
	Legs        [7]*string
	Days        int64
	// HoursUnvalued counts the unpriced hours of rows no verdict refused; WithheldDays counts the refused rows.
	HoursUnvalued, WithheldDays int64
	// ExcludedCount sums stale_rows, accrual_excluded_versions and unexplained_loss_versions.
	ExcludedCount int64
}

// LendingRevenueUSDBucketSumsQuery is the one GROUP BY behind [Store.LendingRevenueUSDBucketSums]; each span reads its
// pool's primary-key range (pool, day BETWEEN from AND through) for every asset.
const LendingRevenueUSDBucketSumsQuery = `SELECT d.pool, d.asset,
		CASE WHEN d.day > t.window_end - 1 THEN 0 WHEN d.day > t.window_end - 7 THEN 1
			WHEN d.day > t.window_end - 30 THEN 2 ELSE 3 END AS bucket,
		(d.valuation_rev = $5 AND d.interest_derive_rev = $6) AS current,
		sum(d.interest_accrued_usd)::text, sum(d.supplier_share_usd)::text, sum(d.supplier_credited_usd)::text,
		sum(d.backstop_take_usd)::text, sum(d.protocol_fee_usd)::text, sum(d.backstop_drawn_usd)::text,
		sum(d.default_loss_usd)::text, count(*),
		coalesce(sum(d.hours_unvalued) FILTER (WHERE d.walk_reason IS DISTINCT FROM $7), 0)::bigint,
		count(*) FILTER (WHERE d.walk_reason = $7),
		sum(d.stale_rows + d.accrual_excluded_versions + d.unexplained_loss_versions)::bigint
	FROM unnest($1::text[], $2::date[], $3::date[], $4::date[]) AS t(pool, from_day, through_day, window_end)
	JOIN lending_revenue_usd_daily d ON d.pool = t.pool AND d.day BETWEEN t.from_day AND t.through_day
	GROUP BY 1, 2, 3, 4`

// LendingRevenueUSDBucketSums sums each span's stored days per asset and bucket, exactly; withheld is the walk_reason a
// verdict refusal is stored with.
func (s *Store) LendingRevenueUSDBucketSums(ctx context.Context, spans []LendingRevenueUSDSpan, valuationRev, deriveRev int16, withheld string) ([]LendingRevenueUSDBucketSum, error) {
	if len(spans) == 0 {
		return nil, nil
	}
	pools := make([]string, len(spans))
	from, through, end := make([]string, len(spans)), make([]string, len(spans)), make([]string, len(spans))
	for i, sp := range spans {
		pools[i] = sp.Pool
		from[i], through[i] = sp.FromDay.UTC().Format(time.DateOnly), sp.ThroughDay.UTC().Format(time.DateOnly)
		end[i] = sp.WindowEnd.UTC().Format(time.DateOnly)
	}
	rows, err := s.db.QueryContext(ctx, LendingRevenueUSDBucketSumsQuery, pools, from, through, end, valuationRev, deriveRev, withheld)
	if err != nil {
		return nil, fmt.Errorf("timescale: lending revenue usd bucket sums: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []LendingRevenueUSDBucketSum
	for rows.Next() {
		var b LendingRevenueUSDBucketSum
		var legs [7]sql.NullString
		if err := rows.Scan(&b.Pool, &b.Asset, &b.Bucket, &b.Current, &legs[0], &legs[1], &legs[2], &legs[3], &legs[4],
			&legs[5], &legs[6], &b.Days, &b.HoursUnvalued, &b.WithheldDays, &b.ExcludedCount); err != nil {
			return nil, fmt.Errorf("timescale: scan lending revenue usd bucket sum: %w", err)
		}
		for i, l := range legs {
			if l.Valid {
				b.Legs[i] = &l.String
			}
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
