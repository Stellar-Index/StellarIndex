// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// AssetCoverageSignals is the per-asset signal set the priceless-popular tripwire classifies; volumes are USD (trades.usd_volume, non-null rows only) and only measured signals, never thresholds.
type AssetCoverageSignals struct {
	// AssetID is the trades base or quote asset (canonical asset_id) the signals
	// are grouped on — the directory row this coverage gap would page for.
	AssetID string
	// HasPriceUSD reports whether a servable USD/XLM-proxy price exists for the asset in the substance window (a non-null
	// prices_1m VWAP against a USD proxy or native/XLM-SAC quote) or the listing serves one from asset_price_snapshot. When
	// true the asset is priced, not a coverage gap.
	HasPriceUSD bool
	// Volume7dUSD / Trades7d are the trailing-7d RAW priced volume + trade count, before any wash discount. The classifier
	// subtracts TopAccountPairVolUSD / TopAccountPairTrades7d from these before comparing to the popularity floor — see
	// popularPriceless.
	Volume7dUSD float64
	Trades7d    int64
	// Volume24hUSD is the trailing-24h priced volume, logged beside a withheld verdict. It is not that verdict: the substance
	// gate decides on three floors over the alias union, so the tripwire asks the gate itself.
	Volume24hUSD float64
	// TopAccountPairVolShare is the fraction of the asset's 7d priced volume in the single busiest UNORDERED counterparty key;
	// a high share means volume-painting wash, which the classifier excludes so a wash farm cannot self-select into the alert.
	// The key is the (maker,taker) pair where both are recorded (SDEX), else the single known account (Soroban AMMs; see
	// popularPricelessCandidatesSQL). Volume with no account (CEX feeds) never enters the numerator, so it dilutes the share;
	// AttributedVolShare says how much volume the share speaks for.
	TopAccountPairVolShare float64
	// AttributedVolShare is the fraction of the asset's 7d priced volume that carried an identifiable counterparty account,
	// i.e. what TopAccountPairVolShare was measured over. Diagnostic only: concentration is already expressed against the FULL
	// volume (TopAccountPairVolShare <= AttributedVolShare by construction), so the classifier needs no floor on it, but it
	// tells an operator whether a low concentration means broad market or a venue without accounts.
	AttributedVolShare float64
	// TopAccountPairVolUSD / TopAccountPairTrades7d are the SAME top counterparty key's raw volume and trade count (paired from
	// one row, never independent MAXes — see top_pair_ranked), subtracted from Volume7dUSD/Trades7d to get the market-character
	// popularity signal.
	TopAccountPairVolUSD   float64
	TopAccountPairTrades7d int64
}

// coverageQuoteProxies is the USD/XLM-proxy quote set, composed from the resolver's lists (transitive_price.go) and kept in lockstep
// with listAssetsBaseSelect (TestProxyQuoteLists_Lockstep). The XLM SAC is bound at $1 in every query that splices this.
const coverageQuoteProxies = usdProxyQuotes + `,
	` + xlmQuotesBound1

// pricelessTradeLegs is the trailing-7d priced trade set, one row per (trade, leg): quote-leg-only assets (aquarius) need the quote
// side. Proxy legs are skipped on both sides: they are seeded as priced, so never output.
const pricelessTradeLegs = `(
  SELECT base_asset AS asset_id, ts, usd_volume, maker, taker
    FROM trades
   WHERE ts >= now() - INTERVAL '7 days'
     AND usd_volume IS NOT NULL
     AND base_asset NOT IN (` + coverageQuoteProxies + `)
  UNION ALL
  SELECT quote_asset, ts, usd_volume, maker, taker
    FROM trades
   WHERE ts >= now() - INTERVAL '7 days'
     AND usd_volume IS NOT NULL
     AND quote_asset NOT IN (` + coverageQuoteProxies + `)
  ) legs`

// popularPricelessCandidatesSQL extracts per-asset signals for priceless assets above a coarse RAW-volume floor ($1 / 1 trade);
// the classifier owns the real verdict. The counterparty key is UNORDERED (LEAST/GREATEST). ONE POPULATION: only SDEX records
// both sides (Soroban AMMs set taker only), so the key DEGENERATES to the one known account when a side is unknown; mixing
// populations would zero an AMM-only share and never fire the wash exclusion. Unattributed rows (CEX) dilute the share DOWNWARD
// (errs toward paging); attributed_vol_share reports the basis. leg_keys is the one pass over the 7d legs (~67M rows on r1)
// that every total and the counterparty split read; three scans took 31 s, one with PopularPricelessCandidates' work_mem takes
// ~14 s.
const popularPricelessCandidatesSQL = `
WITH leg_keys AS (
  SELECT asset_id,
         LEAST(COALESCE(maker, taker), COALESCE(taker, maker))    AS actor_lo,
         GREATEST(COALESCE(maker, taker), COALESCE(taker, maker)) AS actor_hi,
         SUM(usd_volume) AS pv,
         SUM(usd_volume) FILTER (WHERE ts >= now() - INTERVAL '24 hours') AS pv_24h,
         COUNT(*)        AS trades
    FROM ` + pricelessTradeLegs + `
   GROUP BY 1, 2, 3
),
vol7d AS (
  SELECT asset_id,
         SUM(pv)::double precision                  AS vol_7d,
         SUM(trades)::bigint                        AS trades_7d,
         COALESCE(SUM(pv_24h), 0)::double precision AS vol_24h
    FROM leg_keys
   GROUP BY asset_id
),
-- A NULL key means neither side was recorded (CEX feeds): in the totals, never a counterparty.
actor_key AS (
  SELECT asset_id, actor_lo, actor_hi, pv, trades
    FROM leg_keys
   WHERE actor_lo IS NOT NULL
),
-- The top pair's trade COUNT must come from the same row as its volume
-- MAX, not an independent MAX(trades) aggregate — a busier but smaller
-- pair would otherwise donate its count to the biggest-volume pair's
-- discount, understating how much of trades_7d the wash pair itself
-- accounts for. ROW_NUMBER over one ORDER BY keeps volume and count
-- paired to the same counterparty key.
top_pair_ranked AS (
  SELECT asset_id, pv, trades,
         SUM(pv) OVER (PARTITION BY asset_id) AS attributed_vol,
         ROW_NUMBER() OVER (PARTITION BY asset_id ORDER BY pv DESC) AS rn
    FROM actor_key
),
top_pair AS (
  SELECT asset_id,
         pv::double precision     AS top_pair_vol,
         trades                   AS top_pair_trades,
         attributed_vol::double precision AS attributed_vol
    FROM top_pair_ranked
   WHERE rn = 1
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
-- The proxies are seeded explicitly so one_hop can always route THROUGH
-- them: fiat:USD and USDC's two forms never trade as a base against
-- another proxy, and an XLM form's own USD book may be quiet in the window
-- (or stored only USD-as-base) while assets still trade against it.
` + pricelessPricedCTEs + `SELECT
    v.asset_id,
    (p.asset_id IS NOT NULL)                                    AS has_price,
    v.vol_7d,
    v.trades_7d,
    v.vol_24h,
    CASE WHEN v.vol_7d > 0
         THEN COALESCE(tp.top_pair_vol, 0) / v.vol_7d
         ELSE 0 END                                             AS top_pair_share,
    CASE WHEN v.vol_7d > 0
         THEN COALESCE(tp.attributed_vol, 0) / v.vol_7d
         ELSE 0 END                                             AS attributed_vol_share,
    COALESCE(tp.top_pair_vol, 0)                                AS top_pair_vol,
    COALESCE(tp.top_pair_trades, 0)                             AS top_pair_trades
  FROM vol7d v
  LEFT JOIN top_pair tp  ON tp.asset_id  = v.asset_id
  LEFT JOIN priced   p   ON p.asset_id   = v.asset_id
 WHERE p.asset_id IS NULL
`

// PopularPricelessCandidates returns the coverage-signal set for every priceless trades asset (either leg) with priced 7d volume, the tripwire's input; priced assets are excluded in SQL.
func (s *Store) PopularPricelessCandidates(ctx context.Context) ([]AssetCoverageSignals, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("timescale: PopularPricelessCandidates begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The leg_keys hash aggregate spills ~630 MB at the 32 MB default; 128 MB keeps it in memory (256 MB gains nothing).
	// SET LOCAL reverts at the transaction's end, so the pooled connection keeps the default.
	if _, err := tx.ExecContext(ctx, "SET LOCAL work_mem = '128MB'"); err != nil {
		return nil, fmt.Errorf("timescale: PopularPricelessCandidates set work_mem: %w", err)
	}
	rows, err := tx.QueryContext(ctx, popularPricelessCandidatesSQL, canonical.NativeSACContractID())
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
			&sig.TopAccountPairVolUSD,
			&sig.TopAccountPairTrades7d,
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

// substanceFloorHaving is the substance gate on GROUP BY asset_id over prices_1m, shared with one_hop's `priced` CTE so priced_direct
// cannot drift from it (else one unqualified row would count as priced).
const substanceFloorHaving = `SUM(volume_usd) >= 1000                                   -- pricingguard DefaultSubstanceMinVolumeUSD
     AND COUNT(DISTINCT bucket) >= 20                          -- pricingguard DefaultSubstanceMinBuckets
     AND EXTRACT(EPOCH FROM (MAX(bucket) - MIN(bucket))) >= 21600 -- pricingguard DefaultSubstanceMinSpan (6h)`

// pricelessPricedCTEs is the "what counts as priced" CTEs (priced_direct, one_hop, priced) spliced into both sweep and probe so they ask
// the same question; it ends with `priced` closed, no trailing comma.
const pricelessPricedCTEs = `priced_direct AS (
  SELECT base_asset AS asset_id
    FROM prices_1m
   WHERE bucket >= now() - INTERVAL '24 hours'
     AND bucket <= now() - INTERVAL '1 minute'
     AND vwap IS NOT NULL
     AND quote_asset IN (` + coverageQuoteProxies + `)
   GROUP BY base_asset
  HAVING ` + substanceFloorHaving + `
  UNION
  SELECT quote_asset AS asset_id
    FROM prices_1m
   WHERE bucket >= now() - INTERVAL '24 hours'
     AND bucket <= now() - INTERVAL '1 minute'
     AND vwap > 0
     AND base_asset IN (` + xlmQuotesBound1 + `)
   GROUP BY quote_asset
  HAVING ` + substanceFloorHaving + `
  UNION
  SELECT unnest(ARRAY[` + coverageQuoteProxies + `])
),
-- ONE transitive hop, kept in lockstep with Store.TransitiveUSDPriceCandidates.
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
-- gates each candidate hop on its own — aggregating across every priced counterparty
-- would clear the floors on combined depth no single market has.
--
-- The listing's snapshot derivation applies none of these floors, so these
-- arms alone can call an asset the listing prices "unpriced"; priced
-- therefore also takes every asset with a servable snapshot row.
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
  UNION
  ` + pricelessServedArm + `
)
`

// pricelessServedArm is the served-price arm: an asset /v1/assets prices is no gap; same join bound as listAssetsBaseSelect.
const pricelessServedArm = `SELECT asset_id
    FROM asset_price_snapshot
   WHERE computed_at > now() - INTERVAL '` + assetPriceSnapshotMaxAge + `'`

// assetIsPricedSQL asks the sweep's own priced set about one asset id.
var assetIsPricedSQL = `
WITH ` + pricelessPricedCTEs + `
SELECT EXISTS (SELECT 1 FROM priced WHERE asset_id = $2)
`

// AssetIsPriced reports whether assetID is in the set the tripwire treats as priced (same CTEs and substance floors), so the sweep can ask about an alias without a second definition of "priced".
func (s *Store) AssetIsPriced(ctx context.Context, assetID string) (bool, error) {
	var priced bool
	if err := s.db.QueryRowContext(ctx, assetIsPricedSQL, canonical.NativeSACContractID(), assetID).Scan(&priced); err != nil {
		return false, fmt.Errorf("timescale: AssetIsPriced: %w", err)
	}
	return priced, nil
}
