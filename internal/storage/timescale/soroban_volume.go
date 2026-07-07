package timescale

import (
	"context"
	"fmt"
)

// sorobanVolume24hUSDQuery derives the trailing-24h USD trade volume for
// an asset, anchoring XLM-legged trades to the on-chain XLM/USD VWAP — the
// L2.2 "phase 2" derivation, applied per-asset. It exists for
// pure-Soroban SEP-41 tokens: [Store.Volume24hUSDForAsset] only sees the
// insert-time `usd_volume` column, which is populated when a trade's quote
// is a USD-pegged classic (or its SAC wrapper) — so a Soroban token that
// trades against XLM (its primary liquidity route) or another SEP-41 token
// contributes 0 there and shows a bogus "0" USD volume on its asset
// detail. This query keeps every USD-pegged-quote leg via `volume_usd` AND
// adds the XLM-quoted / XLM-based legs valued through `xlm_usd`.
//
// Reads the prices_1m CAGG, NOT the raw trades hypertable — same source +
// same (base OR quote, 24h) window as Volume24hUSDForAsset — so it is
// cheap and consistent. The math is exact in NUMERIC:
//
//   - `volume`          = Σ(base_amount)              (base-leg stroops)
//   - `vwap` * `volume` = Σ(quote_amount)             (quote-leg stroops)
//     because the CAGG defines vwap = Σ(quote_amount)/Σ(base_amount),
//     the same identity /v1/ohlc relies on to derive quote volume.
//
// So for an XLM-base pair (XLM/<token>) the XLM stroops are `volume`, and
// for an XLM-quote pair (<token>/XLM) they are `vwap * volume`; either way
// `/1e7 * xlm_usd` converts to USD. `volume_usd > 0` is the faithful
// discriminator for a USD-pegged-quote pair: the CAGG's `volume_usd =
// Σ(coalesce(usd_volume,0))` is >0 exactly when the quote was recognised
// as USD-pegged at insert (quote_amount is always >0 per the trades CHECK),
// and 0 otherwise — so the CASE picks exactly one valuation per pair and
// never double-counts. Pairs with neither a USD-pegged quote nor an XLM
// leg (pure SEP-41/SEP-41) still contribute nothing — valuing those needs
// a per-token oracle (separate work), matching the GetSourceStats boundary.
//
// The `xlm_usd` CTE is the same bounded (24h, closed) most-recent XLM→USD
// anchor GetSourceStats / GetSourceVolumeHistory use; a NULL anchor (no
// XLM/USD bucket in 24h) degrades the XLM-leg CASEs to NULL, which SUM
// skips — the USD-pegged legs still sum and the outer COALESCE floors the
// all-NULL case to "0". $1 binds the asset's canonical key (base_asset /
// quote_asset wire form, e.g. a `C…` contract id).
const sorobanVolume24hUSDQuery = `
        WITH xlm_usd AS (
          SELECT vwap
            FROM prices_1m
           WHERE base_asset = 'native'
             AND quote_asset IN (
               'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
               'fiat:USD'
             )
             AND vwap IS NOT NULL
             AND bucket >= now() - INTERVAL '24 hours'
           ORDER BY bucket DESC
           LIMIT 1
        )
        SELECT COALESCE(sum(
          CASE
            WHEN volume_usd > 0
              THEN volume_usd
            WHEN base_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
              THEN (volume / 1e7::numeric) * (SELECT vwap FROM xlm_usd)
            WHEN quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
              THEN (vwap * volume / 1e7::numeric) * (SELECT vwap FROM xlm_usd)
            ELSE NULL
          END
        ), 0)::text
          FROM prices_1m
         WHERE (base_asset = $1 OR quote_asset = $1)
           AND bucket >= now() - INTERVAL '24 hours'
           AND bucket  < now()
    `

// SorobanVolume24hUSDForAsset is the XLM-anchored trailing-24h USD-volume
// variant of [Store.Volume24hUSDForAsset], for pure-Soroban SEP-41 assets
// whose liquidity is quoted in XLM (or another SEP-41 token) rather than a
// USD-pegged classic. It sums USD-pegged-quote legs (via the insert-time
// `usd_volume` the plain reader uses) AND XLM-legged trades valued through
// the on-chain XLM/USD VWAP — see [sorobanVolume24hUSDQuery] for the exact
// NUMERIC derivation + its scope boundary (pure SEP-41/SEP-41 legs still
// need a per-token oracle and contribute 0).
//
// Returns "0" (not an error) when the asset had no valuable trades in the
// window — same convention as Volume24hUSDForAsset. `assetKey` is the
// canonical asset string trades.base_asset / quote_asset stores.
func (s *Store) SorobanVolume24hUSDForAsset(ctx context.Context, assetKey string) (string, error) {
	var out string
	if err := s.db.QueryRowContext(ctx, sorobanVolume24hUSDQuery, assetKey).Scan(&out); err != nil {
		return "", fmt.Errorf("timescale: SorobanVolume24hUSDForAsset(%s): %w", assetKey, err)
	}
	return out, nil
}
