// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Daily observed dollar market per asset, the market twin of [Store.DailyOraclePrices]: that reader
// gives the published worth, this one what was observed paid. A premium to net asset value is their
// ratio, so both share a clock and grain and neither is derived from the other.

// MarketDay is one UTC day of one asset's observed dollar market: the VWAP plus the three thin-market measurements, returned rather than filtered because the floor is the caller's policy.
type MarketDay struct {
	// Day is the UTC day bucket (time_bucket('1 day', bucket)).
	Day time.Time
	// AssetID is the requested asset's string. Rows stored under any
	// alias spelling of it (its SAC wrapper) are folded onto it.
	AssetID string
	// VWAP is the day's volume-weighted average price in dollars as exact NUMERIC text, never a
	// float. Weighting each hour by volume_priced recovers the true sum(quote)/sum(base) over
	// priceable trades exactly; `volume` also counts zero-leg trades and would skew it.
	VWAP string
	// VolumeUSD is the day's dollar volume over the same buckets. The
	// first leg of the substance floor.
	VolumeUSD string
	// Hours is how many distinct hour buckets carried a trade.
	//
	// The serving gate counts minute buckets, but an hour is the finest grain a historical day
	// can be held to (minute reach is a deployment setting, see the prices_1h note below).
	Hours int64
	// SpanSeconds is max(bucket) - min(bucket) inside the day: the "market existed at more than
	// one point in time" leg of the floor, at hour grain.
	SpanSeconds int64
	// Trades is the day's trade count. Carried as evidence for a
	// reader, not as a gate.
	Trades int64
}

// dailyMarketDaysQuery sums every dollar spelling per asset and UTC day from prices_1h (prices_1d lacks intra-day timing; prices_1m can
// carry retention). Both stored orientations: SDEX stores the maker's sold asset as base, so a fill where the maker sold dollars sits
// under (dollar, asset) with vwap in asset per dollar; its legs are swapped back. Closed buckets only; package-level so a test pins
// sargability.
const dailyMarketDaysQuery = `
        WITH family AS (
            SELECT spelling, member
              FROM unnest($1::text[], $5::text[]) AS f(spelling, member)
        ),
        legs AS (
            SELECT bucket, family.member, vwap * volume_priced AS usd_leg, volume_priced AS asset_leg,
                   volume_usd, trade_count
              FROM prices_1h
              JOIN family ON family.spelling = prices_1h.base_asset
             WHERE base_asset  = ANY($1)
               AND quote_asset = ANY($2)
               AND bucket <= now() - INTERVAL '1 hour'
               AND bucket >= $3
               AND bucket <  $4::timestamptz + INTERVAL '1 day'
               AND vwap IS NOT NULL
               AND volume_priced > 0
            UNION ALL
            SELECT bucket, family.member, volume_priced AS usd_leg, vwap * volume_priced AS asset_leg,
                   volume_usd, trade_count
              FROM prices_1h
              JOIN family ON family.spelling = prices_1h.quote_asset
             WHERE base_asset  = ANY($2)
               AND quote_asset = ANY($1)
               AND NOT (base_asset = ANY($1) AND quote_asset = ANY($2))
               AND bucket <= now() - INTERVAL '1 hour'
               AND bucket >= $3
               AND bucket <  $4::timestamptz + INTERVAL '1 day'
               AND vwap IS NOT NULL
               AND volume_priced > 0
        )
        SELECT time_bucket('1 day', bucket)                                     AS day,
               member,
               (sum(usd_leg) / sum(asset_leg))::text                            AS vwap,
               COALESCE(sum(volume_usd), 0)::text                               AS volume_usd,
               count(DISTINCT bucket)                                           AS hours,
               COALESCE(EXTRACT(EPOCH FROM (max(bucket) - min(bucket)))::bigint, 0) AS span_seconds,
               COALESCE(sum(trade_count), 0)                                    AS trades
          FROM legs
         GROUP BY day, member
         ORDER BY day ASC, member ASC
    `

func (s *Store) DailyMarketDays(
	ctx context.Context,
	assets, quotes []canonical.Asset,
	from, to time.Time,
) ([]MarketDay, error) {
	if len(assets) == 0 || len(quotes) == 0 {
		return nil, nil
	}
	spellings, members := marketDayFamilies(assets)
	quoteKeys := make([]string, len(quotes))
	for i, q := range quotes {
		quoteKeys[i] = q.String()
	}
	// time_bucket, not date_trunc: date_trunc('day', timestamptz) folds at the SESSION timezone,
	// so a non-UTC server would cut days off midnight UTC and bucket a premium's two legs on
	// different clocks.
	rows, err := s.db.QueryContext(ctx, dailyMarketDaysQuery, spellings, quoteKeys,
		from.UTC().Truncate(24*time.Hour), to.UTC().Truncate(24*time.Hour), members)
	if err != nil {
		return nil, fmt.Errorf("timescale: DailyMarketDays: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]MarketDay, 0, 512)
	for rows.Next() {
		var d MarketDay
		if err := rows.Scan(&d.Day, &d.AssetID, &d.VWAP, &d.VolumeUSD,
			&d.Hours, &d.SpanSeconds, &d.Trades); err != nil {
			return nil, fmt.Errorf("timescale: DailyMarketDays scan: %w", err)
		}
		d.Day = d.Day.UTC()
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: DailyMarketDays rows: %w", err)
	}
	return out, nil
}

// marketDayFamilies binds each requested asset's alias spellings ($1) with the asset they fold onto ($5); a shared alias goes to the
// first asset.
func marketDayFamilies(assets []canonical.Asset) (spellings, members []string) {
	seen := make(map[string]struct{}, len(assets)*2)
	claim := func(form, member string) {
		if _, dup := seen[form]; dup {
			return
		}
		seen[form] = struct{}{}
		spellings = append(spellings, form)
		members = append(members, member)
	}
	for _, a := range assets {
		claim(a.String(), a.String())
	}
	for _, a := range assets {
		for _, form := range canonical.AssetAliasStrings(a) {
			claim(form, a.String())
		}
	}
	return spellings, members
}
