// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"fmt"
	"time"
)

// TradesPrices1mDrift is one pair whose `trades` rows and prices_1m
// buckets disagree over a window. Amounts are exact NUMERIC text.
type TradesPrices1mDrift struct {
	BaseAsset, QuoteAsset   string
	TradeCount, CAGGCount   string
	TradeVolume, CAGGVolume string
	TradeUSD, CAGGUSD       string
}

// TradesPrices1mDrift compares, per pair, the trade count, base volume and
// USD volume of `trades` in [from, to) with the sums prices_1m serves for
// the same minutes, and returns every pair where they differ. prices_1m is
// a plain GROUP BY minute over `trades`, so on a fully materialised window
// the two agree exactly; a difference is a bucket the aggregate still
// holds from rows that have since been rewritten, added or removed.
// from and to must be whole minutes, or the bucket and row ranges differ.
func (s *Store) TradesPrices1mDrift(ctx context.Context, from, to time.Time) ([]TradesPrices1mDrift, error) {
	if !from.Equal(from.Truncate(time.Minute)) || !to.Equal(to.Truncate(time.Minute)) || !from.Before(to) {
		return nil, fmt.Errorf("timescale: TradesPrices1mDrift: [%s,%s) is not a non-empty range of whole minutes",
			from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano))
	}
	const q = `
		WITH t AS (
			SELECT base_asset, quote_asset, count(*)::numeric AS n,
			       sum(base_amount) AS vol, sum(coalesce(usd_volume, 0)) AS usd
			  FROM trades
			 WHERE ts >= $1 AND ts < $2
			 GROUP BY base_asset, quote_asset
		), p AS (
			SELECT base_asset, quote_asset, sum(trade_count)::numeric AS n,
			       sum(volume) AS vol, sum(volume_usd) AS usd
			  FROM prices_1m
			 WHERE bucket >= $1 AND bucket < $2
			 GROUP BY base_asset, quote_asset
		)
		SELECT base_asset, quote_asset,
		       coalesce(t.n, 0)::text, coalesce(p.n, 0)::text,
		       coalesce(t.vol, 0)::text, coalesce(p.vol, 0)::text,
		       coalesce(t.usd, 0)::text, coalesce(p.usd, 0)::text
		  FROM t FULL OUTER JOIN p USING (base_asset, quote_asset)
		 WHERE coalesce(t.n, 0) <> coalesce(p.n, 0)
		    OR coalesce(t.vol, 0) <> coalesce(p.vol, 0)
		    OR coalesce(t.usd, 0) <> coalesce(p.usd, 0)
		 ORDER BY base_asset, quote_asset`
	rows, err := s.db.QueryContext(ctx, q, from, to)
	if err != nil {
		return nil, fmt.Errorf("timescale: TradesPrices1mDrift: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TradesPrices1mDrift
	for rows.Next() {
		var d TradesPrices1mDrift
		if err := rows.Scan(&d.BaseAsset, &d.QuoteAsset, &d.TradeCount, &d.CAGGCount,
			&d.TradeVolume, &d.CAGGVolume, &d.TradeUSD, &d.CAGGUSD); err != nil {
			return nil, fmt.Errorf("timescale: TradesPrices1mDrift: scan: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: TradesPrices1mDrift: %w", err)
	}
	return out, nil
}
