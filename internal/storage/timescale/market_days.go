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

// MarketDay is one UTC day of one asset's observed dollar market: the day's VWAP plus the three
// measurements the thin-market floor is made of. They travel with the price, not as a WHERE clause,
// because the floor is the caller's policy and a too-thin day is a finding, not a row to drop.
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

// DailyMarketDays returns one [MarketDay] per (asset, UTC day) over the
// inclusive range [from, to], folding EVERY dollar spelling in `quotes`.
//
// The dollar spellings (classic USDC, its SAC, `fiat:USD`) carry disjoint venue populations, so the
// market is their sum, the same union [pricingguard.SubstanceGate] and [Store.GetAssetATH] use.
// Each asset's [canonical.AssetAliases] forms are mapped back to it in SQL so `hours` counts
// distinct hours once; a spelling shared by two requested assets is credited to the first only.
//
// It reads prices_1h: prices_1d has no intra-day timing, and prices_1m can carry an armable 90-day
// retention that would silently truncate the series. prices_1h has no retention
// ([CAGGsLiveForever]) but its depth is not promised: an unmaterialised span reads as "did not
// trade", so callers counting silent days must bound the count to the span this reader covers.
//
// Only rows with the asset as base are read; a dollar-first book reads as no market (a false
// absence, never a wrong price). Empty `assets` or `quotes` returns (nil, nil). Closed buckets only;
// bounds floor to their UTC day and `to` reads through its end.
//
// dailyMarketDaysQuery is package-level so a query-shape test can pin its sargability.
const dailyMarketDaysQuery = `
        WITH family AS (
            SELECT spelling, member
              FROM unnest($1::text[], $5::text[]) AS f(spelling, member)
        )
        SELECT time_bucket('1 day', bucket)                                     AS day,
               family.member,
               (sum(vwap * volume_priced) / sum(volume_priced))::text           AS vwap,
               COALESCE(sum(volume_usd), 0)::text                               AS volume_usd,
               count(DISTINCT bucket)                                           AS hours,
               COALESCE(EXTRACT(EPOCH FROM (max(bucket) - min(bucket)))::bigint, 0) AS span_seconds,
               COALESCE(sum(trade_count), 0)                                    AS trades
          FROM prices_1h
          JOIN family ON family.spelling = prices_1h.base_asset
         WHERE base_asset  = ANY($1)
           AND quote_asset = ANY($2)
           AND bucket <= now() - INTERVAL '1 hour'
           AND bucket >= $3
           AND bucket <  $4::timestamptz + INTERVAL '1 day'
           AND vwap IS NOT NULL
           AND volume_priced > 0
         GROUP BY day, family.member
         ORDER BY day ASC, family.member ASC
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

// marketDayFamilies binds every alias spelling of each requested asset ($1) alongside the asset it
// folds onto ($5), as parallel arrays. Each spelling is bound once: an asset keeps its own, and a
// shared alias goes to the first asset naming it.
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
