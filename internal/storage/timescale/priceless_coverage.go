// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"fmt"
)

// AssetCoverageSignals is the per-asset signal set the priceless-popular
// tripwire (internal/pricelesscoverage) classifies. All volumes are
// USD-denominated (trades.usd_volume, non-null rows only); the tripwire's
// pure classifier turns these into a fire/no-fire verdict — this struct
// carries only the measured signals, never the thresholds.
type AssetCoverageSignals struct {
	// AssetID is the trades base_asset (canonical asset_id) the signals
	// are grouped on — the directory row this coverage gap would page for.
	AssetID string
	// HasPriceUSD reports whether a servable USD/XLM-proxy price exists for
	// the asset in the substance window (a non-null prices_1m VWAP against
	// a USD proxy or native/XLM-SAC quote). When true the asset is priced,
	// not a coverage gap.
	HasPriceUSD bool
	// Volume7dUSD / Trades7d are the trailing-7d priced volume + trade
	// count the popularity floor is measured against (AFTER the classifier
	// discounts wash — see TopAccountPairVolShare).
	Volume7dUSD float64
	Trades7d    int64
	// Volume24hUSD is the trailing-24h priced volume, logged beside a
	// withheld verdict. It is not that verdict: the substance gate
	// decides on three floors over the alias union, so the tripwire asks
	// the gate itself.
	Volume24hUSD float64
	// TopAccountPairVolShare is the fraction of the asset's 7d priced
	// volume that trades in the single busiest UNORDERED counterparty
	// key. High share == volume-painting wash (the scam-AUD signature:
	// ~108/109 of its trades one wallet pair), which the classifier
	// excludes so a wash farm cannot self-select into the alert.
	//
	// The key is the (maker,taker) account pair where both sides are
	// recorded (SDEX) and the single known account where only one is
	// (every Soroban AMM — see popularPricelessCandidatesSQL). Volume
	// with NO account on either side (the external CEX feeds) can never
	// enter the numerator, so it dilutes the share downward — see
	// AttributedVolShare for how much of the volume the share speaks for.
	TopAccountPairVolShare float64
	// AttributedVolShare is the fraction of the asset's 7d priced volume
	// that carried an identifiable counterparty account, i.e. the share
	// of the market TopAccountPairVolShare could be measured over. It is
	// diagnostic — the classifier needs no floor on it, because the
	// concentration is already expressed against the FULL volume
	// (TopAccountPairVolShare <= AttributedVolShare by construction) —
	// but it tells an operator reading the alert whether a low
	// concentration means "broad market" or "venue without accounts".
	AttributedVolShare float64
}

// coverageQuoteProxies is the USD/XLM-proxy quote set a servable price is
// derived through — identical to the /v1/assets catalogue's direct_usd +
// asset_vs_xlm CTEs (USDC classic + its SAC, fiat:USD, native + the XLM
// SAC). Kept in lockstep with listAssetsBaseSelect: an asset priced only
// through one of these quotes is "priced" for the coverage check.
//
// Composed from the resolver's own lists (transitive_price.go) rather
// than restated, so the tripwire and Store.TransitiveUSDPrice cannot
// disagree about what a proxy is; TestProxyQuoteLists_Lockstep pins the
// catalogue's literal IN-lists to the same set.
const coverageQuoteProxies = usdProxyQuotes + `,
	` + xlmQuotes

// popularPricelessCandidatesSQL extracts, per trades base_asset that has
// ANY priced volume in the trailing 7 days, the signal set the tripwire
// classifies. It PRE-FILTERS to priceless assets above a coarse RAW-volume
// floor ($1 / 1 trade) so the (small) candidate set the worker classifies
// is bounded without paying a full-history scan; the market-character
// discount, the popularity floor and the withheld verdict are applied by
// the pure classifier, never here — this query only measures.
//
// The counterparty key is UNORDERED (LEAST/GREATEST) so a round-trip
// A->B / B->A folds into the one concentrated pair it economically is,
// matching the volume-character rollup design.
//
// ONE POPULATION. Only the SDEX decoder records both sides of a fill:
// on every Soroban AMM (aquarius, soroswap, phoenix, comet,
// sushiswap_v3) the resting side is the POOL — a venue every trade of
// that market shares, not an independent economic actor — so those rows
// carry a taker and a NULL maker (r1 2026-09-19: 100% of the rows of
// all five AMM sources, 27.8k in 24h). Requiring both columns non-NULL
// in the numerator while the vol7d denominator took every row measured
// the two over DIFFERENT populations: the share of an AMM-only asset
// was 0 by construction, so the wash exclusion could never fire for it
// and a farm painting volume on an AMM self-selected straight into the
// alert (r1 2026-09-19: two AMM-only assets above the $10k popularity
// floor at 0.95 / 0.9999 single-taker concentration, both reading 0).
// The key therefore DEGENERATES to the one known account when a side is
// unknown — for an AMM, "one wallet swapping back and forth through the
// pool", which is the AMM-shaped ping-pong signature.
//
// Rows with NO account on either side (the external CEX feeds —
// binance, coinbase, kraken, bitstamp record neither) still cannot
// enter the numerator, so unattributed volume dilutes the share
// DOWNWARD: the tripwire errs toward paging a human, never toward
// silently suppressing a gap it cannot measure. attributed_vol_share
// reports how much of the asset's volume the share was measured over.
//
// COST: widening the population costs this leg ~14s on r1 (EXPLAIN
// ANALYZE 2026-09-19: 6.6s -> 20.7s over the same 7d scan). The rows
// read are unchanged; the planner declines to parallelise the wider
// aggregate. That keeps a full sweep around 70s, well inside
// DefaultSweepTimeout (5 min) and the 10-minute cadence.
const popularPricelessCandidatesSQL = `
WITH vol7d AS (
  SELECT base_asset AS asset_id,
         SUM(usd_volume)::double precision AS vol_7d,
         COUNT(*)                          AS trades_7d
    FROM trades
   WHERE ts >= now() - INTERVAL '7 days'
     AND usd_volume IS NOT NULL
   GROUP BY base_asset
),
vol24h AS (
  SELECT base_asset AS asset_id,
         SUM(usd_volume)::double precision AS vol_24h
    FROM trades
   WHERE ts >= now() - INTERVAL '24 hours'
     AND usd_volume IS NOT NULL
   GROUP BY base_asset
),
actor_key AS (
  SELECT base_asset AS asset_id,
         LEAST(COALESCE(maker, taker), COALESCE(taker, maker))    AS actor_lo,
         GREATEST(COALESCE(maker, taker), COALESCE(taker, maker)) AS actor_hi,
         SUM(usd_volume) AS pv
    FROM trades
   WHERE ts >= now() - INTERVAL '7 days'
     AND usd_volume IS NOT NULL
     AND (maker IS NOT NULL OR taker IS NOT NULL)
   GROUP BY 1, 2, 3
),
top_pair AS (
  SELECT asset_id,
         MAX(pv)::double precision AS top_pair_vol,
         SUM(pv)::double precision AS attributed_vol
    FROM actor_key
   GROUP BY asset_id
),
-- "Priced directly" = the catalogue's direct_usd / asset_vs_xlm reach,
-- in BOTH stored directions of the XLM leg, PLUS the proxies themselves.
--
-- The XLM leg must be read both ways because sources that write SWAP
-- direction (aquarius: base = token_in, no canonical.Orient) store an
-- asset bought with XLM as (XLM-SAC, asset) — the SAC as BASE. Reading
-- only base_asset = X left that whole market invisible: r1 2026-08-28,
-- CBIJ… had $730k/7d against the XLM SAC and fired this tripwire with
-- no withheld verdict, because nothing could price it.
--
-- The proxies are seeded explicitly because they never appear as a
-- BASE against another proxy — XLM/USD is keyed base_asset='native',
-- so the XLM SAC (and fiat:USD, and USDC's two forms) could never enter
-- priced_direct and one_hop could therefore never route THROUGH them.
` + pricelessPricedCTEs + `SELECT
    v.asset_id,
    (p.asset_id IS NOT NULL)                                    AS has_price,
    v.vol_7d,
    v.trades_7d,
    COALESCE(v24.vol_24h, 0)                                    AS vol_24h,
    CASE WHEN v.vol_7d > 0
         THEN COALESCE(tp.top_pair_vol, 0) / v.vol_7d
         ELSE 0 END                                             AS top_pair_share,
    CASE WHEN v.vol_7d > 0
         THEN COALESCE(tp.attributed_vol, 0) / v.vol_7d
         ELSE 0 END                                             AS attributed_vol_share
  FROM vol7d v
  LEFT JOIN vol24h   v24 ON v24.asset_id = v.asset_id
  LEFT JOIN top_pair tp  ON tp.asset_id  = v.asset_id
  LEFT JOIN priced   p   ON p.asset_id   = v.asset_id
 WHERE p.asset_id IS NULL
`

// PopularPricelessCandidates returns the coverage-signal set for every
// priceless trades base_asset with priced 7d volume — the input the
// priceless-popular tripwire classifies. Priced assets are excluded in
// SQL (they are not coverage gaps); everything else the classifier judges.
func (s *Store) PopularPricelessCandidates(ctx context.Context) ([]AssetCoverageSignals, error) {
	rows, err := s.db.QueryContext(ctx, popularPricelessCandidatesSQL)
	if err != nil {
		return nil, fmt.Errorf("timescale: PopularPricelessCandidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AssetCoverageSignals
	for rows.Next() {
		var sig AssetCoverageSignals
		if err := rows.Scan(
			&sig.AssetID,
			&sig.HasPriceUSD,
			&sig.Volume7dUSD,
			&sig.Trades7d,
			&sig.Volume24hUSD,
			&sig.TopAccountPairVolShare,
			&sig.AttributedVolShare,
		); err != nil {
			return nil, fmt.Errorf("timescale: PopularPricelessCandidates scan: %w", err)
		}
		out = append(out, sig)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: PopularPricelessCandidates rows: %w", err)
	}
	return out, nil
}

// pricelessPricedCTEs is the "what counts as priced" half of the tripwire's
// query — priced_direct, one_hop and priced — as one text so that a
// single-asset probe asks EXACTLY the same question the sweep asks. Both
// queries splice it after their own leading CTEs; it ends with the
// `priced` CTE closed, no trailing comma.
const pricelessPricedCTEs = `priced_direct AS (
  SELECT DISTINCT base_asset AS asset_id
    FROM prices_1m
   WHERE bucket >= now() - INTERVAL '24 hours'
     AND vwap IS NOT NULL
     AND quote_asset IN (` + coverageQuoteProxies + `)
  UNION
  SELECT DISTINCT quote_asset AS asset_id
    FROM prices_1m
   WHERE bucket >= now() - INTERVAL '24 hours'
     AND vwap > 0
     AND base_asset IN (` + xlmQuotes + `)
  UNION
  SELECT unnest(ARRAY[` + coverageQuoteProxies + `])
),
-- ONE transitive hop, kept in lockstep with Store.TransitiveUSDPrice.
--
-- This CTE decides what counts as "priced" for the tripwire, and it is
-- QUOTE-based, not served-price-based: it asks "can this asset reach a
-- USD/XLM proxy?", not "did /v1/assets serve a number?". So when the
-- serving side gained a one-hop derivation, this had to gain the same
-- reach in the same commit — otherwise a newly-priced asset keeps
-- firing the alert forever, because the tripwire alone still believes
-- it unreachable.
--
-- One hop ONLY, matching the resolver. Deeper chains are deliberately
-- not counted: each additional hop compounds the trust placed in an
-- intermediate market, and the resolver will not serve them either.
--
-- The floors below are NOT decoration. Without them this arm counts an
-- asset as priced merely for TOUCHING a priced asset, while the serving
-- side still withholds it because the connecting market is too thin —
-- so the asset silently leaves coverage monitoring and never gets a
-- price. Measured before adding them: of 955 newly-reachable assets, 4
-- clear the popularity floor and TWO of those four (USDMPOOL at $798,
-- yHELIX at $296 over 24h) fail the volume floor. Those two would have
-- gone quiet while remaining genuinely unpriced.
--
-- Grouped per (asset, hop) rather than per asset, because the resolver
-- gates ONE chosen hop — aggregating across every priced counterparty
-- would clear the floors on combined depth no single market has.
one_hop AS (
  SELECT CASE WHEN p.base_asset = d.asset_id THEN p.quote_asset ELSE p.base_asset END AS asset_id,
         d.asset_id                                                    AS hop,
         SUM(p.volume_usd)                                             AS vol_usd,
         COUNT(DISTINCT p.bucket)                                      AS buckets,
         EXTRACT(EPOCH FROM (MAX(p.bucket) - MIN(p.bucket)))           AS span_s
    FROM prices_1m p
    JOIN priced_direct d
      ON (d.asset_id = p.quote_asset OR d.asset_id = p.base_asset)
   WHERE p.bucket >= now() - INTERVAL '24 hours'
     AND p.bucket <= now() - INTERVAL '1 minute'
     AND p.vwap IS NOT NULL
   GROUP BY 1, 2
),
priced AS (
  SELECT asset_id FROM priced_direct
  UNION
  SELECT DISTINCT asset_id
    FROM one_hop
   WHERE asset_id <> hop
     AND vol_usd >= 1000    -- pricingguard DefaultSubstanceMinVolumeUSD
     AND buckets >= 20      -- pricingguard DefaultSubstanceMinBuckets
     AND span_s >= 21600    -- pricingguard DefaultSubstanceMinSpan (6h)
)
`

// assetIsPricedSQL asks the sweep's own priced set about one asset id.
const assetIsPricedSQL = `
WITH ` + pricelessPricedCTEs + `
SELECT EXISTS (SELECT 1 FROM priced WHERE asset_id = $1)
`

// AssetIsPriced reports whether assetID is in the set the priceless-popular
// tripwire treats as priced — the same CTEs, the same substance floors —
// so the sweep can ask about an ALIAS of a candidate (a SAC contract id
// resolved to its classic asset) without a second definition of "priced".
func (s *Store) AssetIsPriced(ctx context.Context, assetID string) (bool, error) {
	var priced bool
	if err := s.db.QueryRowContext(ctx, assetIsPricedSQL, assetID).Scan(&priced); err != nil {
		return false, fmt.Errorf("timescale: AssetIsPriced: %w", err)
	}
	return priced, nil
}
