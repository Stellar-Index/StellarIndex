package timescale

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// LendingEmissionsUSDBucketSum is one group of lending_emissions_usd_daily rows of a pool and stream in one bucket (as
// [LendingRevenueUSDBucketSum]'s), alike in everything that decides how the protocols block reads them: revision, token,
// usd_reason, lower bounds and exclusions. Amount and AmountUSD are exact decimal sums; AmountUSD is nil when no row of
// the group is priced, which a group with a USDReason never is.
type LendingEmissionsUSDBucketSum struct {
	Pool, Stream          string
	Bucket                int
	Current               bool // the rows are of the valuation revision asked for
	EmissionToken         *string
	EmissionTokenDecimals *int
	USDReason             *string
	LowerBound            bool
	StreamLowerBound      bool
	Excluded              []string
	Approximate           bool
	Amount                string
	AmountUSD             *string
	Rows, HoursUnvalued   int64
	// LastValuedDay is the group's latest day with USD; zero when none has.
	LastValuedDay time.Time
}

// LendingEmissionsUSDBucketSumsQuery is the one GROUP BY behind [Store.LendingEmissionsUSDBucketSums]; each span reads
// its pool's primary-key range (pool, day BETWEEN from AND through).
const LendingEmissionsUSDBucketSumsQuery = `SELECT d.pool, d.stream, ` + lendingUSDWindowBucket + ` AS bucket,
		d.valuation_rev = $5 AS current, d.emission_token, d.emission_token_decimals, d.usd_reason, d.lower_bound,
		d.stream_lower_bound, d.excluded::text, bool_or(d.approximate), sum(d.amount)::text, sum(d.amount_usd)::text,
		count(*), sum(d.hours_unvalued)::bigint, max(d.day) FILTER (WHERE d.amount_usd IS NOT NULL)
	FROM unnest($1::text[], $2::date[], $3::date[], $4::date[]) AS t(pool, from_day, through_day, window_end)
	JOIN lending_emissions_usd_daily d ON d.pool = t.pool AND d.day BETWEEN t.from_day AND t.through_day
	WHERE d.stream = ANY($6::text[])
	GROUP BY 1, 2, 3, 4, 5, 6, 7, 8, 9, 10`

// LendingEmissionsUSDBucketSums sums each span's stored rows of streams per stream and bucket, exactly.
func (s *Store) LendingEmissionsUSDBucketSums(ctx context.Context, spans []LendingUSDSpan, valuationRev int16, streams []string) ([]LendingEmissionsUSDBucketSum, error) {
	if len(spans) == 0 || len(streams) == 0 {
		return nil, nil
	}
	pools, from, through, end := lendingUSDSpanArrays(spans)
	rows, err := s.db.QueryContext(ctx, LendingEmissionsUSDBucketSumsQuery, pools, from, through, end, valuationRev, streams)
	if err != nil {
		return nil, fmt.Errorf("timescale: lending emissions usd bucket sums: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []LendingEmissionsUSDBucketSum
	for rows.Next() {
		b, err := scanLendingEmissionsUSDBucketSum(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func scanLendingEmissionsUSDBucketSum(rows *sql.Rows) (LendingEmissionsUSDBucketSum, error) {
	var b LendingEmissionsUSDBucketSum
	var token, reason, usd sql.NullString
	var decimals sql.NullInt32
	var excluded string
	var last sql.NullTime
	if err := rows.Scan(&b.Pool, &b.Stream, &b.Bucket, &b.Current, &token, &decimals, &reason, &b.LowerBound,
		&b.StreamLowerBound, &excluded, &b.Approximate, &b.Amount, &usd, &b.Rows, &b.HoursUnvalued, &last); err != nil {
		return b, fmt.Errorf("timescale: scan lending emissions usd bucket sum: %w", err)
	}
	b.EmissionToken, b.USDReason, b.AmountUSD = nullStringPtr(token), nullStringPtr(reason), nullStringPtr(usd)
	if decimals.Valid {
		n := int(decimals.Int32)
		b.EmissionTokenDecimals = &n
	}
	if err := json.Unmarshal([]byte(excluded), &b.Excluded); err != nil {
		return b, fmt.Errorf("timescale: lending emissions usd bucket excluded: %w", err)
	}
	if last.Valid {
		b.LastValuedDay = last.Time.UTC()
	}
	return b, nil
}
