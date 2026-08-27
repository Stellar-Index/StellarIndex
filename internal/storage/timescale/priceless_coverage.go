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
	// Volume24hUSD is the trailing-24h priced volume — the substance-gate
	// serve-floor input. A recent market below the floor is one the gate
	// withholds fail-closed, so its pricelessness is EXPECTED (a recorded
	// withheld verdict), not a coverage gap.
	Volume24hUSD float64
	// TopAccountPairVolShare is the fraction of the asset's 7d priced
	// volume that trades in the single busiest UNORDERED (maker,taker)
	// account pair. High share == volume-painting wash (the scam-AUD
	// signature: ~108/109 of its trades one wallet pair), which the
	// classifier excludes so a wash farm cannot self-select into the alert.
	TopAccountPairVolShare float64
}

// coverageQuoteProxies is the USD/XLM-proxy quote set a servable price is
// derived through — identical to the /v1/assets catalogue's direct_usd +
// asset_vs_xlm CTEs (USDC classic + its SAC, fiat:USD, native + the XLM
// SAC). Kept in lockstep with listAssetsBaseSelect: an asset priced only
// through one of these quotes is "priced" for the coverage check.
const coverageQuoteProxies = `'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
	'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
	'fiat:USD',
	'native',
	'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA'`

// popularPricelessCandidatesSQL extracts, per trades base_asset that has
// ANY priced volume in the trailing 7 days, the signal set the tripwire
// classifies. It PRE-FILTERS to priceless assets above a coarse RAW-volume
// floor ($1 / 1 trade) so the (small) candidate set the worker classifies
// is bounded without paying a full-history scan; the market-character
// discount, the popularity floor and the withheld verdict are applied by
// the pure classifier, never here — this query only measures.
//
// The (maker,taker) account pair is UNORDERED (LEAST/GREATEST) so a
// round-trip A->B / B->A folds into the one concentrated pair it
// economically is, matching the volume-character rollup design.
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
top_pair AS (
  SELECT base_asset AS asset_id, MAX(pv)::double precision AS top_pair_vol
    FROM (
      SELECT base_asset,
             SUM(usd_volume) AS pv
        FROM trades
       WHERE ts >= now() - INTERVAL '7 days'
         AND usd_volume IS NOT NULL
         AND maker IS NOT NULL AND taker IS NOT NULL
       GROUP BY base_asset, LEAST(maker, taker), GREATEST(maker, taker)
    ) p
   GROUP BY base_asset
),
priced_direct AS (
  SELECT DISTINCT base_asset AS asset_id
    FROM prices_1m
   WHERE bucket >= now() - INTERVAL '24 hours'
     AND vwap IS NOT NULL
     AND quote_asset IN (` + coverageQuoteProxies + `)
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
SELECT
    v.asset_id,
    (p.asset_id IS NOT NULL)                                    AS has_price,
    v.vol_7d,
    v.trades_7d,
    COALESCE(v24.vol_24h, 0)                                    AS vol_24h,
    CASE WHEN v.vol_7d > 0
         THEN COALESCE(tp.top_pair_vol, 0) / v.vol_7d
         ELSE 0 END                                             AS top_pair_share
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
