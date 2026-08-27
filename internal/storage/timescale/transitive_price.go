package timescale

import (
	"context"
	"database/sql"
	"fmt"
)

// TransitivePrice is a USD price reached through ONE intermediate hop,
// together with the hop it went through so the caller can substance-gate
// that leg independently.
//
// # WHY THIS EXISTS
//
// The catalogue prices the long tail through exactly two hard-coded
// shapes: `direct_usd` (quoted in a USD proxy) and `asset_vs_xlm`
// (quoted in XLM, times xlm_usd). An asset that trades against NEITHER
// is unpriceable no matter how deep its market — and both catalogue
// queries are additionally built on `classic_assets`, which is
// structurally classic-only (`issuer_g_strkey NOT NULL`), so no
// Soroban-native contract asset can reach them at all.
//
// Measured on r1 2026-08-27: `CAUP7NFA…` traded $71.8k over 6,418 trades
// in 7 days and served no price, because its ONLY counterparty is
// `CBIJ…`, which is itself a Soroban-native contract. Both legs are
// substantial — CAUP7/CBIJ is $18,872 over 1,216 buckets spanning 24h,
// and CBIJ/XLM is $18,908 over 27 buckets spanning 19.1h — so the price
// is derivable; nothing was deriving it.
//
// SAFETY. This deliberately returns the hop rather than just a number.
// A transitive price is only as trustworthy as its weakest leg, so the
// caller MUST gate both (asset→hop and hop→proxy) through the substance
// gate before serving. Publishing a two-hop price without checking the
// intermediate would let a thin middle market reprice everything
// downstream of it — the exact manipulation the substance floors exist
// to prevent.
type TransitivePrice struct {
	// PriceUSD is the derived USD price as an exact NUMERIC string
	// (ADR-0003 — never float64 on a money path).
	PriceUSD string
	// Hop is the intermediate asset_id the price was derived through.
	// The caller substance-gates this leg separately.
	Hop string
	// HopVolume24hUSD is the intermediate market's trailing-24h USD
	// volume — the reason this hop was chosen over the alternatives.
	HopVolume24hUSD string
}

// usdProxyQuotes are the quotes whose VWAP is ALREADY a USD price.
// Deliberately the same set the catalogue's direct_usd CTE and the
// coverage tripwire's `coverageQuoteProxies` use — these three lists
// must move together or an asset can be "priced" by one and "priceless"
// by another.
const usdProxyQuotes = `'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
	'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
	'fiat:USD'`

// xlmQuotes are XLM in both identity forms — the classic 'native' and
// its Stellar Asset Contract. A VWAP against these is a price in XLM and
// needs multiplying by xlm_usd.
const xlmQuotes = `'native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA'`

// TransitiveUSDPrice derives a USD price for `assetID` through its
// deepest counterparty that itself has a USD price.
//
// Returns ok=false (nil error) when no such path exists — absence of a
// route is a measurement, not a failure.
//
// Mechanics, and the two details that make it correct:
//
//   - DIRECTION. prices_1m stores a pair in BOTH directions, and `vwap`
//     is always "price of base in quote". A row (asset, hop) is used as
//     is; a row (hop, asset) is INVERTED. Reading either without
//     inverting would produce a reciprocal price — off by orders of
//     magnitude, not a rounding error.
//   - HOP CHOICE. Ordered by the hop market's 24h USD volume, so the
//     deepest available intermediate wins. A tie or a thin winner is
//     still the caller's problem to gate; this picks the best candidate,
//     it does not decide publishability.
//
// Closed buckets only (ADR-0015): every read excludes the in-flight
// minute, matching every other price surface.
func (s *Store) TransitiveUSDPrice(ctx context.Context, assetID string) (TransitivePrice, bool, error) {
	const q = `
WITH xlm_usd AS (
    SELECT vwap
      FROM prices_1m
     WHERE base_asset = 'native'
       AND quote_asset IN (` + usdProxyQuotes + `)
       AND bucket <= now() - INTERVAL '1 minute'
       AND bucket >= now() - INTERVAL '24 hours'
       AND vwap IS NOT NULL
     ORDER BY bucket DESC
     LIMIT 1
),
-- Every counterparty this asset traded against in the window, in BOTH
-- stored directions, with the depth of that market.
hops AS (
    SELECT CASE WHEN base_asset = $1 THEN quote_asset ELSE base_asset END AS hop,
           SUM(volume_usd) AS hop_vol
      FROM prices_1m
     WHERE (base_asset = $1 OR quote_asset = $1)
       AND bucket <= now() - INTERVAL '1 minute'
       AND bucket >= now() - INTERVAL '24 hours'
     GROUP BY 1
),
-- The hop's OWN USD price: direct against a USD proxy, else via XLM.
hop_usd AS (
    SELECT h.hop,
           h.hop_vol,
           COALESCE(
             (SELECT p.vwap FROM prices_1m p
               WHERE p.base_asset = h.hop
                 AND p.quote_asset IN (` + usdProxyQuotes + `)
                 AND p.bucket <= now() - INTERVAL '1 minute'
                 AND p.bucket >= now() - INTERVAL '24 hours'
                 AND p.vwap IS NOT NULL
               ORDER BY p.bucket DESC LIMIT 1),
             (SELECT p.vwap FROM prices_1m p
               WHERE p.base_asset = h.hop
                 AND p.quote_asset IN (` + xlmQuotes + `)
                 AND p.bucket <= now() - INTERVAL '1 minute'
                 AND p.bucket >= now() - INTERVAL '24 hours'
                 AND p.vwap IS NOT NULL
               ORDER BY p.bucket DESC LIMIT 1)
             * (SELECT vwap FROM xlm_usd)
           ) AS hop_usd
      FROM hops h
),
-- This asset's price IN the hop. Prefer the (asset, hop) direction;
-- fall back to inverting (hop, asset).
leg AS (
    SELECT hu.hop,
           hu.hop_vol,
           hu.hop_usd,
           COALESCE(
             (SELECT p.vwap FROM prices_1m p
               WHERE p.base_asset = $1 AND p.quote_asset = hu.hop
                 AND p.bucket <= now() - INTERVAL '1 minute'
                 AND p.bucket >= now() - INTERVAL '24 hours'
                 AND p.vwap IS NOT NULL
               ORDER BY p.bucket DESC LIMIT 1),
             1 / NULLIF((SELECT p.vwap FROM prices_1m p
               WHERE p.base_asset = hu.hop AND p.quote_asset = $1
                 AND p.bucket <= now() - INTERVAL '1 minute'
                 AND p.bucket >= now() - INTERVAL '24 hours'
                 AND p.vwap IS NOT NULL
               ORDER BY p.bucket DESC LIMIT 1), 0)
           ) AS leg_vwap
      FROM hop_usd hu
     WHERE hu.hop_usd IS NOT NULL AND hu.hop_usd > 0
)
SELECT (leg_vwap * hop_usd)::text, hop, COALESCE(hop_vol, 0)::text
  FROM leg
 WHERE leg_vwap IS NOT NULL AND leg_vwap > 0
 ORDER BY hop_vol DESC NULLS LAST
 LIMIT 1`

	var out TransitivePrice
	err := s.db.QueryRowContext(ctx, q, assetID).Scan(&out.PriceUSD, &out.Hop, &out.HopVolume24hUSD)
	switch {
	case err == sql.ErrNoRows:
		return TransitivePrice{}, false, nil
	case err != nil:
		return TransitivePrice{}, false, fmt.Errorf("timescale: TransitiveUSDPrice[%s]: %w", assetID, err)
	}
	return out, true, nil
}
