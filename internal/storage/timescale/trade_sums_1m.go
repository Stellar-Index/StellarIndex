package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TradeMinuteSum is one (source, minute) row of trade_sums_1m in one stored direction, oriented as the caller asked.
type TradeMinuteSum struct {
	Source string
	Bucket time.Time
	// BaseSum and QuoteSum sum the priceable rows' legs; Count counts them.
	BaseSum, QuoteSum canonical.Amount
	Count             int64
	// MinPrice and MaxPrice bound the rows' quote/base prices in the requested orientation.
	MinPrice, MaxPrice *big.Rat
}

// TradeSums1mInRange returns trade_sums_1m's rows for the market with bucket in [from, to) in either stored direction, minus non-VWAP
// sources, plus the view's materialisation watermark: buckets at or past it are not materialised and read as absent.
func (s *Store) TradeSums1mInRange(ctx context.Context, p canonical.Pair, from, to time.Time) ([]TradeMinuteSum, time.Time, error) {
	const wq = `
        SELECT _timescaledb_functions.to_timestamp(_timescaledb_functions.cagg_watermark(mat_hypertable_id))
          FROM _timescaledb_catalog.continuous_agg
         WHERE user_view_name = 'trade_sums_1m'`
	var watermark time.Time
	if err := s.db.QueryRowContext(ctx, wq).Scan(&watermark); err != nil {
		return nil, time.Time{}, fmt.Errorf("timescale: TradeSums1mInRange watermark: %w", err)
	}
	// Literal bounds so the planner excludes chunks at plan time, as LatestTradesForPair does.
	q := fmt.Sprintf(`
        SELECT source, bucket, true AS forward,
               base_sum, quote_sum, trade_count, min_price::text, max_price::text
          FROM trade_sums_1m
         WHERE base_asset = $1 AND quote_asset = $2
           AND bucket >= TIMESTAMPTZ '%[1]s'
           AND bucket  < TIMESTAMPTZ '%[2]s'
           AND trade_count > 0
           AND NOT (source = ANY(string_to_array($3, ',')))
         UNION ALL
        SELECT source, bucket, false AS forward,
               base_sum, quote_sum, trade_count, min_price::text, max_price::text
          FROM trade_sums_1m
         WHERE base_asset = $2 AND quote_asset = $1
           AND bucket >= TIMESTAMPTZ '%[1]s'
           AND bucket  < TIMESTAMPTZ '%[2]s'
           AND trade_count > 0
           AND NOT (source = ANY(string_to_array($3, ',')))
         ORDER BY bucket, source`, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)) //nolint:gosec // formatted time.Time values, not input
	rows, err := s.db.QueryContext(ctx, q, p.Base.String(), p.Quote.String(), nonVWAPSourcesArg())
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("timescale: TradeSums1mInRange: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TradeMinuteSum
	for rows.Next() {
		var r TradeMinuteSum
		var forward bool
		var minS, maxS sql.NullString
		if err := rows.Scan(&r.Source, &r.Bucket, &forward, &r.BaseSum, &r.QuoteSum, &r.Count, &minS, &maxS); err != nil {
			return nil, time.Time{}, fmt.Errorf("timescale: TradeSums1mInRange scan: %w", err)
		}
		lo, okLo := new(big.Rat).SetString(minS.String)
		hi, okHi := new(big.Rat).SetString(maxS.String)
		if !minS.Valid || !maxS.Valid || !okLo || !okHi || lo.Sign() <= 0 || hi.Sign() <= 0 {
			return nil, time.Time{}, fmt.Errorf("timescale: TradeSums1mInRange: %s %v price bounds %q..%q", r.Source, r.Bucket, minS.String, maxS.String)
		}
		r.MinPrice, r.MaxPrice = lo, hi
		if !forward {
			r.BaseSum, r.QuoteSum = r.QuoteSum, r.BaseSum
			r.MinPrice, r.MaxPrice = new(big.Rat).Inv(hi), new(big.Rat).Inv(lo)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("timescale: TradeSums1mInRange rows: %w", err)
	}
	return out, watermark, nil
}
