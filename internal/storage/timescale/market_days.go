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
	// AssetID is the base asset's canonical string, exactly as stored.
	AssetID string
	// VWAP is the day's volume-weighted average price in dollars, as
	// exact NUMERIC text (ADR-0003 — never a float). Weighted by BASE
	// volume across the day's hour buckets, which recovers the day's
	// true sum(quote)/sum(base): each hour's vwap is that hour's own
	// sum(quote)/sum(base), so weighting it by that hour's base volume
	// and dividing by the total base volume is an identity, not an
	// approximation.
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

// DailyMarketDays returns one [MarketDay] per (asset, UTC day) for each
// of `assets` over the inclusive day range [from, to], folding EVERY
// dollar spelling in `quotes` into a single day figure.
//
// # Why the quote spellings are unioned rather than chosen between
//
// A Stellar token's dollar book is split across spellings of the
// dollar — the issuer's classic USDC, that asset's SAC wrapper, and the
// synthetic `fiat:USD` an off-chain venue quotes in. They carry
// DISJOINT venue populations, exactly as XLM's three canonical ids do,
// so the market's real breadth is their sum and reading one spelling
// measures a fraction of the market as though it were all of it. This
// is the alias union [pricingguard.SubstanceGate] applies before
// measuring substance, and the same USD-quote set
// [Store.GetAssetATH] already treats as one dollar.
//
// # Why prices_1h, and not prices_1d or prices_1m
//
// `prices_1d` carries the day's VWAP and its dollar volume but nothing
// about WHEN inside the day the trading happened, and two of the
// serving floor's three legs are about exactly that.
//
// `prices_1m` has the serving gate's own grain, and its reach is a
// DEPLOYMENT SETTING rather than a property of the schema: migration
// 0002 gave it a 30-day retention, 0031 removed that on 2026-05-14, and
// 0156 attached a 90-day policy to that view alone — shipped
// `scheduled => false`, dropping nothing until an operator arms it, and
// then dropping every day forever. A series whose history silently
// truncates the day a job is armed is not a series, so the floor is not
// built on that grain.
//
// `prices_1h` has never carried a retention policy (migration 0002: "No
// retention policy on 1h+ — indefinite by design") and is one of the
// views the backfill tool re-materialises after every chunk
// ([CAGGsLiveForever]), so it is the coarsest-reaching grain that still
// answers the "when inside the day" question. It is also ~60x fewer
// rows to scan than the minute grain over a multi-year window.
//
// What it does NOT promise is materialisation depth. Migrations 0115
// and 0147 dropped all seven price CAGGs and left re-materialisation to
// the operator, so on a deployment where that was not completed this
// reader returns nothing for the affected span — which reads as "the
// token did not trade". Callers that count silent days must bound the
// count to the span this reader actually answered for.
//
// # What this deliberately does NOT read
//
// Only the stored direction with the ASSET as base. The sibling readers
// in this package fold both directions with an exact 1/vwap; doing it
// here would additionally need the flipped row's base volume
// re-derived to keep the weighting exact, and no admitted member is
// known to keep a book stored dollar-first. The consequence of the
// narrowing is a false ABSENCE and never a wrong price: such a market
// reads here as no market at all.
//
// An empty `assets` or `quotes` returns (nil, nil) — no keys is not a
// query. Closed buckets only (ADR-0015).
func (s *Store) DailyMarketDays(
	ctx context.Context,
	assets, quotes []canonical.Asset,
	from, to time.Time,
) ([]MarketDay, error) {
	if len(assets) == 0 || len(quotes) == 0 {
		return nil, nil
	}
	baseKeys := make([]string, len(assets))
	for i, a := range assets {
		baseKeys[i] = a.String()
	}
	quoteKeys := make([]string, len(quotes))
	for i, q := range quotes {
		quoteKeys[i] = q.String()
	}
	// time_bucket, not date_trunc: date_trunc('day', timestamptz) folds
	// at the SESSION timezone, so a server whose TimeZone is not UTC
	// would cut days somewhere other than midnight UTC — and the two
	// legs of a premium would then be bucketed on different clocks.
	const q = `
        SELECT time_bucket('1 day', bucket)                                     AS day,
               base_asset,
               (sum(vwap * volume) / sum(volume))::text                         AS vwap,
               COALESCE(sum(volume_usd), 0)::text                               AS volume_usd,
               count(DISTINCT bucket)                                           AS hours,
               COALESCE(EXTRACT(EPOCH FROM (max(bucket) - min(bucket)))::bigint, 0) AS span_seconds,
               COALESCE(sum(trade_count), 0)                                    AS trades
          FROM prices_1h
         WHERE base_asset  = ANY($1)
           AND quote_asset = ANY($2)
           AND bucket + INTERVAL '1 hour' <= now()
           AND bucket >= $3
           AND bucket <= $4
           AND vwap IS NOT NULL
           AND volume > 0
         GROUP BY day, base_asset
         ORDER BY day ASC, base_asset ASC
    `
	rows, err := s.db.QueryContext(ctx, q, baseKeys, quoteKeys, from.UTC(), to.UTC())
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
