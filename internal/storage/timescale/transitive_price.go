package timescale

import (
	"context"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TransitivePrice is a USD price reached through ONE intermediate hop, returned with that hop, for assets trading against neither direct_usd nor asset_vs_xlm.
type TransitivePrice struct {
	// PriceUSD is the derived USD price as an exact NUMERIC string
	// (ADR-0003 — never float64 on a money path).
	PriceUSD string
	// Hop is the intermediate asset_id the price was derived through.
	// The caller substance-gates this leg separately.
	Hop string
	// HopVolume24hUSD is the trailing-24h USD volume of the asset<->hop
	// market (the near leg) — the key candidates are ranked by. It says
	// nothing about the hop's own USD/XLM market; the caller gates that.
	HopVolume24hUSD string
}

// usdProxyQuotes are the quotes whose VWAP is ALREADY a USD price. It must stay identical to the
// catalogue's direct_usd CTE and the coverage tripwire's `coverageQuoteProxies`, or an asset can
// be priced by one and priceless by another.
const usdProxyQuotes = `'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
	'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
	'fiat:USD'`

// transitiveHopCandidates bounds how many ranked hops
// TransitiveUSDPriceCandidates returns: enough that a thin top hop cannot
// hide a sound one behind it, few enough that the caller's per-hop gating
// stays a small fixed cost on a serving path.
const transitiveHopCandidates = 5

// TransitiveUSDPriceCandidates derives USD prices for `assetID` through each counterparty that itself has a USD price, best first; an empty slice with nil error means no route exists.
func (s *Store) TransitiveUSDPriceCandidates(ctx context.Context, assetID string) ([]TransitivePrice, error) {
	rows, err := s.db.QueryContext(ctx, transitiveUSDPriceSQL, assetID, transitiveHopCandidates, canonical.NativeSACContractID())
	if err != nil {
		return nil, fmt.Errorf("timescale: TransitiveUSDPriceCandidates[%s]: %w", assetID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []TransitivePrice
	for rows.Next() {
		var tp TransitivePrice
		if err := rows.Scan(&tp.PriceUSD, &tp.Hop, &tp.HopVolume24hUSD); err != nil {
			return nil, fmt.Errorf("timescale: TransitiveUSDPriceCandidates[%s]: scan: %w", assetID, err)
		}
		out = append(out, tp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: TransitiveUSDPriceCandidates[%s]: %w", assetID, err)
	}
	return out, nil
}

// transitiveUSDPriceSQL is TransitiveUSDPriceCandidates' query, hoisted to keep the function under
// the funlen threshold. $1 = asset_id, $2 = candidate limit, $3 = the network's native-XLM SAC.
//
//nolint:gosec // G202: fragments are constant SQL (helper output built from literals and $N placeholders); values bind via $N
var transitiveUSDPriceSQL = `
WITH xlm_usd AS (` + xlmUSDAnchorAt("now()", "$3::text") + `),
-- Every counterparty this asset traded against in the window, in BOTH
-- stored directions, with the depth of the asset<->hop market.
hops AS (
    SELECT CASE WHEN base_asset = $1 THEN quote_asset ELSE base_asset END AS hop,
           SUM(volume_usd) AS hop_vol
      FROM prices_1m
     WHERE (base_asset = $1 OR quote_asset = $1)
       AND bucket <= now() - INTERVAL '1 minute'
       AND bucket >= now() - INTERVAL '24 hours'
     GROUP BY 1
),
-- The hop's OWN USD price, in preference order:
--   1. the hop IS XLM (either identity form) — its price is xlm_usd, or
--      none: never a raw XLM/USD row older than the anchor's freshness.
--      Without this arm an asset whose only market is against the XLM
--      SAC had no route (r1 2026-08-28, CBIJ…).
--   2. direct against a USD proxy;
--   3. against XLM (hx), times the XLM/USD anchor as of hx's own minute
--      (hxa): the hop's latest bucket in EITHER stored direction (hop/XLM
--      as is, XLM/hop inverted — the shape swap-direction sources such as
--      aquarius write). A stale direction must not outrank a fresher one;
--      the hop/XLM row wins a tied bucket.
hop_usd AS (
    SELECT h.hop,
           h.hop_vol,
           CASE WHEN h.hop IN (` + xlmQuotesBound3 + `)
                THEN (SELECT vwap FROM xlm_usd)
           ELSE COALESCE(
             (SELECT p.vwap FROM prices_1m p
               WHERE p.base_asset = h.hop
                 AND p.quote_asset IN (` + usdProxyQuotes + `)
                 AND p.bucket <= now() - INTERVAL '1 minute'
                 AND p.bucket >= now() - INTERVAL '24 hours'
                 AND p.vwap IS NOT NULL
               ORDER BY p.bucket DESC, ` + usdQuotePref + ` LIMIT 1),
             hx.v * hxa.vwap
           ) END AS hop_usd
      FROM hops h
      LEFT JOIN LATERAL (
        SELECT e.v, e.bucket FROM (
          (SELECT p.vwap AS v, p.bucket, 1 AS pref FROM prices_1m p
            WHERE p.base_asset = h.hop
              AND p.quote_asset IN (` + xlmQuotesBound3 + `)
              AND p.bucket <= now() - INTERVAL '1 minute'
              AND p.bucket >= now() - INTERVAL '24 hours'
              AND p.vwap IS NOT NULL
            ORDER BY p.bucket DESC, ` + xlmFormPrefOpenBound3 + `p.quote_asset) LIMIT 1)
          UNION ALL
          (SELECT 1 / NULLIF(p.vwap, 0), p.bucket, 2 FROM prices_1m p
            WHERE p.base_asset IN (` + xlmQuotesBound3 + `)
              AND p.quote_asset = h.hop
              AND p.bucket <= now() - INTERVAL '1 minute'
              AND p.bucket >= now() - INTERVAL '24 hours'
              AND p.vwap IS NOT NULL
            ORDER BY p.bucket DESC, ` + xlmFormPrefOpenBound3 + `p.base_asset) LIMIT 1)
        ) e
        WHERE e.v IS NOT NULL
        ORDER BY e.bucket DESC, e.pref LIMIT 1
      ) hx ON true
      LEFT JOIN LATERAL (` + xlmUSDAnchorAt("hx.bucket", "$3::text") + `) hxa ON true
),
-- This asset's price IN the hop: the latest bucket in either direction,
-- (asset, hop) as is or (hop, asset) inverted, so a stale row in one
-- direction cannot outrank a fresh one in the other. (asset, hop) wins a tie.
leg AS (
    SELECT hu.hop,
           hu.hop_vol,
           hu.hop_usd,
           (SELECT e.v FROM (
              (SELECT p.vwap AS v, p.bucket, 1 AS pref FROM prices_1m p
                WHERE p.base_asset = $1 AND p.quote_asset = hu.hop
                  AND p.bucket <= now() - INTERVAL '1 minute'
                  AND p.bucket >= now() - INTERVAL '24 hours'
                  AND p.vwap IS NOT NULL
                ORDER BY p.bucket DESC LIMIT 1)
              UNION ALL
              (SELECT 1 / NULLIF(p.vwap, 0), p.bucket, 2 FROM prices_1m p
                WHERE p.base_asset = hu.hop AND p.quote_asset = $1
                  AND p.bucket <= now() - INTERVAL '1 minute'
                  AND p.bucket >= now() - INTERVAL '24 hours'
                  AND p.vwap IS NOT NULL
                ORDER BY p.bucket DESC LIMIT 1)
            ) e
            WHERE e.v IS NOT NULL
            ORDER BY e.bucket DESC, e.pref LIMIT 1) AS leg_vwap
      FROM hop_usd hu
     WHERE hu.hop_usd IS NOT NULL AND hu.hop_usd > 0
)
SELECT (leg_vwap * hop_usd)::text, hop, COALESCE(hop_vol, 0)::text
  FROM leg
 WHERE leg_vwap IS NOT NULL AND leg_vwap > 0
 ORDER BY hop_vol DESC NULLS LAST, hop
 LIMIT $2`
