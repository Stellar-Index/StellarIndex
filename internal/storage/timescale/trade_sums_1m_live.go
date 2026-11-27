package timescale

import (
	"context"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TradeSums1mLiveMaxSpan bounds [Store.TradeSums1mLive]: trade_sums_1m refreshes every minute and trails by one (0217), so a
// healthy view leaves a few unmaterialised minutes; fifteen covers a slow refresh while keeping the raw aggregate one pair's short tail.
const TradeSums1mLiveMaxSpan = 15 * time.Minute

// tradeSums1mLiveTemplate is trade_sums_1m's SELECT (migration 0217) over raw trades in literal [%[1]s, %[2]s); it must stay
// expression-for-expression the view's, which TestTradeSums1mLive_MatchesMaterialisedView pins. The outer pair and source
// predicates push down into it, so each union arm reads trades_pair_ts_idx inside the chunks of the bounds.
const tradeSums1mLiveTemplate = `(
        SELECT time_bucket('1 minute', ts)                                          AS bucket,
               source,
               base_asset,
               quote_asset,
               sum(base_amount)  FILTER (WHERE base_amount > 0 AND quote_amount > 0) AS base_sum,
               sum(quote_amount) FILTER (WHERE base_amount > 0 AND quote_amount > 0) AS quote_sum,
               count(*)          FILTER (WHERE base_amount > 0 AND quote_amount > 0) AS trade_count,
               min(quote_amount / base_amount)
                                 FILTER (WHERE base_amount > 0 AND quote_amount > 0) AS min_price,
               max(quote_amount / base_amount)
                                 FILTER (WHERE base_amount > 0 AND quote_amount > 0) AS max_price
          FROM trades
         WHERE ts >= TIMESTAMPTZ '%[1]s'
           AND ts  < TIMESTAMPTZ '%[2]s'
         GROUP BY bucket, source, base_asset, quote_asset) AS trade_sums_live`

// TradeSums1mLiveQuery is the SQL of [Store.TradeSums1mLive] for [from, to); its args are base, quote and the non-VWAP source list.
// Exported for the plan-shape integration test.
func TradeSums1mLiveQuery(from, to time.Time) string {
	f, t := from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)
	rel := fmt.Sprintf(tradeSums1mLiveTemplate, f, t)
	return fmt.Sprintf(tradeSums1mRangeTemplate, f, t, rel)
}

// TradeSums1mLive computes the rows [Store.TradeSums1mInRange] would read for minutes [from, to) straight from trades, for the
// minutes the view has not materialised yet. Both bounds must be whole minutes at most [TradeSums1mLiveMaxSpan] apart.
func (s *Store) TradeSums1mLive(ctx context.Context, p canonical.Pair, from, to time.Time) ([]TradeMinuteSum, error) {
	if !from.Equal(from.Truncate(time.Minute)) || !to.Equal(to.Truncate(time.Minute)) {
		return nil, fmt.Errorf("timescale: TradeSums1mLive: [%v, %v) is not whole minutes", from, to)
	}
	if to.Before(from) || to.Sub(from) > TradeSums1mLiveMaxSpan {
		return nil, fmt.Errorf("timescale: TradeSums1mLive: span %v outside [0, %v]", to.Sub(from), TradeSums1mLiveMaxSpan)
	}
	return s.queryTradeMinuteSums(ctx, "TradeSums1mLive", TradeSums1mLiveQuery(from, to), p) //nolint:gosec // G201: formatted time.Time values, not input
}
