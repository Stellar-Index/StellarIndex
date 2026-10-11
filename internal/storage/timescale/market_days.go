// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Daily OBSERVED dollar market per asset — the market twin of
// [Store.DailyOraclePrices].
//
// The oracle reader answers "what did somebody PUBLISH this instrument
// to be worth on this day". This one answers "what was somebody
// observed PAYING for the token on this day". A premium to net asset
// value is their ratio, so the two have to be readable on the same
// clock at the same grain, and neither may ever be derived from the
// other.

// MarketDay is one UTC day of one asset's observed dollar market: the
// day's volume-weighted average price, plus the three measurements the
// serving thin-market floor is made of.
//
// The substance measurements travel WITH the price rather than being
// applied as a WHERE clause, because a floor is policy and this is a
// measurement. The caller applies the policy and can then say on the
// wire what it refused and why — a day whose market was too thin to
// price is a finding about that day, not a row to drop in silence.
type MarketDay struct {
	// Day is the UTC day bucket (time_bucket('1 day', bucket)).
	Day time.Time
	// AssetID is the requested asset's string. Rows stored under any
	// alias spelling of it (its SAC wrapper) are folded onto it.
	AssetID string
	// VWAP is the day's volume-weighted average price in dollars, as
	// exact NUMERIC text (ADR-0003 — never a float). Weighted by each
	// hour's volume_priced, which recovers the day's true
	// sum(quote)/sum(base) over priceable trades: each hour's vwap is that
	// hour's own ratio over the same trades, so the weighting is an
	// identity, not an approximation. `volume` also counts zero-leg
	// trades and would skew it.
	VWAP string
	// VolumeUSD is the day's dollar volume over the same buckets. The
	// first leg of the substance floor.
	VolumeUSD string
	// Hours is how many distinct hour buckets carried a trade.
	//
	// The serving gate counts buckets at MINUTE grain. An hour is the
	// finest grain a HISTORICAL day can be held to — see the reader's
	// "Why prices_1h" note for why the minute grain's reach is a
	// deployment setting rather than a property of the schema.
	Hours int64
	// SpanSeconds is max(bucket) - min(bucket) inside the day: the
	// "a market must have existed at more than one point in time" leg
	// of the floor, measured at the same hour grain.
	SpanSeconds int64
	// Trades is the day's trade count. Carried as evidence for a
	// reader, not as a gate.
	Trades int64
}

// DailyMarketDays returns one [MarketDay] per (asset, UTC day) over the
// inclusive range [from, to], folding EVERY dollar spelling in `quotes`.
//
// The dollar spellings (classic USDC, its SAC, `fiat:USD`) carry disjoint
// venue populations, so the market is their sum, the same union
// [pricingguard.SubstanceGate] and [Store.GetAssetATH] use. Each asset's
// [canonical.AssetAliases] forms are likewise mapped back to it in SQL, so
// `hours` counts distinct hours once. A spelling shared by two requested
// assets is credited to the first only.
//
// It reads prices_1h: prices_1d has no intra-day timing, and prices_1m can
// carry an armable 90-day retention (migration 0156) that would silently
// truncate the series. prices_1h has no retention ([CAGGsLiveForever]) but
// its depth is not promised (migrations 0115/0147 left re-materialisation to
// the operator): an unmaterialised span reads as "did not trade", so callers
// counting silent days must bound the count to the span this reader covers.
//
// Only rows with the asset as base are read; a dollar-first book reads as
// no market (a false absence, never a wrong price). Empty `assets` or
// `quotes` returns (nil, nil). Closed buckets only (ADR-0015). Bounds floor
// to their UTC day; `to` reads through the end of its day.
//
// dailyMarketDaysQuery is package-level so a query-shape test can pin its
// sargability, as for [closedVWAPAtOrBeforeQueryTemplate].
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
	// time_bucket, not date_trunc: date_trunc('day', timestamptz) folds
	// at the SESSION timezone, so a server whose TimeZone is not UTC
	// would cut days somewhere other than midnight UTC — and the two
	// legs of a premium would then be bucketed on different clocks.
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

// marketDayFamilies binds every alias spelling of each requested asset
// ($1) alongside the requested asset it folds onto ($5), as parallel
// arrays. Each spelling is bound once: a requested asset always keeps
// its own spelling, and an alias form goes to the first asset naming it.
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
