package timescale

import (
	"context"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TransitivePrice is a USD price reached through ONE intermediate hop, returned with that hop, for assets trading against neither
// direct_usd nor asset_vs_xlm.
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
	// HopQuote is the asset_id the hop's own USD price was read against: a USD proxy, or the XLM
	// form when the price is hop/XLM times the anchor. Empty when the hop is XLM itself. The caller
	// gates exactly this market; one hop can appear once per route.
	HopQuote string
}

// usdProxyQuotes are quotes whose VWAP is already a USD price; keep identical to the catalogue's direct_usd CTE and the coverage
// tripwire's `coverageQuoteProxies`, or an asset is priced by one and priceless by another.
const usdProxyQuotes = `'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
	'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
	'fiat:USD'`

// transitiveHopCandidates bounds ranked routes returned (a hop has up to two: direct USD and via XLM): enough that a thin top hop
// cannot hide a sound one, few enough to keep per-route gating cheap.
const transitiveHopCandidates = 10

// TransitiveUSDPriceCandidates derives USD prices for `assetID` through each counterparty that itself has a USD price, best first; an
// empty slice with nil error means no route exists.
func (s *Store) TransitiveUSDPriceCandidates(ctx context.Context, assetID string) ([]TransitivePrice, error) {
	rows, err := s.db.QueryContext(ctx, transitiveUSDPriceSQL, assetID, transitiveHopCandidates, canonical.NativeSACContractID())
	if err != nil {
		return nil, fmt.Errorf("timescale: TransitiveUSDPriceCandidates[%s]: %w", assetID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []TransitivePrice
	for rows.Next() {
		var tp TransitivePrice
		if err := rows.Scan(&tp.PriceUSD, &tp.Hop, &tp.HopVolume24hUSD, &tp.HopQuote); err != nil {
			return nil, fmt.Errorf("timescale: TransitiveUSDPriceCandidates[%s]: scan: %w", assetID, err)
		}
		out = append(out, tp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: TransitiveUSDPriceCandidates[%s]: %w", assetID, err)
	}
	return out, nil
}

// transitiveUSDPriceSQL is TransitiveUSDPriceCandidates' query, hoisted for funlen. $1 = asset_id, $2 = limit, $3 = native-XLM SAC.
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
-- The hop's OWN USD price, one row per ROUTE, each naming the market it was read from so the
-- caller can substance-gate that market and no other:
--   0. the hop IS XLM (either identity form) — its price is xlm_usd, or
--      none: never a raw XLM/USD row older than the anchor's freshness.
--      Without this arm an asset whose only market is against the XLM
--      SAC had no route (r1 2026-08-28, CBIJ…).
--   1. direct against a USD proxy (d);
--   2. against XLM (hx), times the XLM/USD anchor as of hx's own minute
--      (hxa): the hop's latest bucket in EITHER stored direction (hop/XLM
--      as is, XLM/hop inverted — the shape swap-direction sources such as
--      aquarius write). A stale direction must not outrank a fresher one;
--      the hop/XLM row wins a tied bucket.
-- Routes 1 and 2 are both returned: one dust print on the hop's USD market must neither set
-- the price nor hide the deep XLM route.
hop_usd AS (
    SELECT h.hop, h.hop_vol, r.hop_usd, r.hop_quote, r.route
      FROM hops h
      LEFT JOIN LATERAL (
        SELECT p.vwap, p.quote_asset FROM prices_1m p
         WHERE p.base_asset = h.hop
           AND p.quote_asset IN (` + usdProxyQuotes + `)
           AND p.bucket <= now() - INTERVAL '1 minute'
           AND p.bucket >= now() - INTERVAL '24 hours'
           AND p.vwap IS NOT NULL
         ORDER BY p.bucket DESC, ` + usdQuotePref + ` LIMIT 1
      ) d ON true
      LEFT JOIN LATERAL (
        SELECT e.v, e.bucket, e.form FROM (
          (SELECT p.vwap AS v, p.bucket, p.quote_asset AS form, 1 AS pref FROM prices_1m p
            WHERE p.base_asset = h.hop
              AND p.quote_asset IN (` + xlmQuotesBound3 + `)
              AND p.bucket <= now() - INTERVAL '1 minute'
              AND p.bucket >= now() - INTERVAL '24 hours'
              AND p.vwap IS NOT NULL
            ORDER BY p.bucket DESC, ` + xlmFormPrefOpenBound3 + `p.quote_asset) LIMIT 1)
          UNION ALL
          (SELECT 1 / NULLIF(p.vwap, 0), p.bucket, p.base_asset, 2 FROM prices_1m p
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
      CROSS JOIN LATERAL (
        SELECT (SELECT vwap FROM xlm_usd) AS hop_usd, '' AS hop_quote, 0 AS route
         WHERE h.hop IN (` + xlmQuotesBound3 + `)
        UNION ALL
        SELECT d.vwap, d.quote_asset, 1
         WHERE h.hop NOT IN (` + xlmQuotesBound3 + `)
        UNION ALL
        SELECT hx.v * hxa.vwap, hx.form, 2
         WHERE h.hop NOT IN (` + xlmQuotesBound3 + `)
      ) r
),
-- This asset's price IN the hop: the latest bucket in either direction,
-- (asset, hop) as is or (hop, asset) inverted, so a stale row in one
-- direction cannot outrank a fresh one in the other. (asset, hop) wins a tie.
leg AS (
    SELECT hu.hop,
           hu.hop_vol,
           hu.hop_usd,
           hu.hop_quote,
           hu.route,
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
SELECT (leg_vwap * hop_usd)::text, hop, COALESCE(hop_vol, 0)::text, hop_quote
  FROM leg
 WHERE leg_vwap IS NOT NULL AND leg_vwap > 0
 ORDER BY hop_vol DESC NULLS LAST, hop, route
 LIMIT $2`
