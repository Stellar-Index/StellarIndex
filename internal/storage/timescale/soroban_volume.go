package timescale

import (
	"context"
	"fmt"
)

// sorobanVolume24hUSDQuery derives the trailing-24h USD trade volume for
// an asset, anchoring XLM-legged trades to the on-chain XLM/USD VWAP — the
// L2.2 "phase 2" derivation, applied per-asset. It exists for
// pure-Soroban SEP-41 tokens: [Store.Volume24hUSDForAsset] only sees the
// insert-time `usd_volume` column, so a Soroban token whose XLM-legged
// trades were stored unvalued shows a bogus "0" USD volume on its asset
// detail.
//
// Valuation is per TRADE, never per prices_1m row: each trade contributes
// its insert-time `usd_volume` when present, else its XLM leg valued at
// query time. A prices_1m row sums every source's trades for one
// (bucket, base, quote), so a bucket can be partly valued (one insert's FX
// lookup failed, a neighbour's succeeded); an either/or choice on the
// row's `volume_usd` would drop the unvalued trades, and prices_1m keeps
// no residual volume to value them from. COALESCE per trade takes exactly
// one valuation each, so nothing is dropped or double-counted.
//
// The XLM leg is `base_amount` for an XLM-base trade and `quote_amount`
// for an XLM-quote one — the same stroop sums prices_1m exposes as
// `volume` and `vwap * volume` — and `/1e7 * xlm_usd` converts it to USD.
// Trades with no valuation and no XLM leg (pure SEP-41/SEP-41) still
// contribute nothing — valuing those needs a per-token oracle, matching
// the GetSourceStats boundary.
//
// The window is the closed 1-minute buckets of the last 24h, the same
// buckets prices_1m serves (ADR-0015); `ts >= now() - 24h` is the
// index-usable superset of the bucket lower bound. The `xlm_usd` CTE is
// the same bounded most-recent XLM→USD anchor GetSourceStats uses; a NULL
// anchor degrades the XLM-leg fallback to NULL, which SUM skips, and the
// outer COALESCE floors the all-NULL case to "0". $1 binds the asset's
// canonical key (trades.base_asset / quote_asset form, e.g. a `C…` id).
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
        ),
        asset_trades AS (
          SELECT time_bucket('1 minute', ts) AS bucket,
                 base_asset, quote_asset, base_amount, quote_amount, usd_volume
            FROM trades
           WHERE (base_asset = $1 OR quote_asset = $1)
             AND ts >= now() - INTERVAL '24 hours'
        )
        SELECT COALESCE(sum(
          COALESCE(usd_volume, CASE
            WHEN base_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
              THEN (base_amount / 1e7::numeric) * (SELECT vwap FROM xlm_usd)
            WHEN quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
              THEN (quote_amount / 1e7::numeric) * (SELECT vwap FROM xlm_usd)
            ELSE NULL
          END)
        ), 0)::text
          FROM asset_trades
         WHERE bucket >= now() - INTERVAL '24 hours'
           AND bucket <= now() - INTERVAL '1 minute'
    `

// SorobanVolume24hUSDForAsset is the XLM-anchored trailing-24h USD-volume
// variant of [Store.Volume24hUSDForAsset], for pure-Soroban SEP-41 assets
// whose liquidity is quoted in XLM (or another SEP-41 token) rather than a
// USD-pegged classic. Each trade contributes its insert-time `usd_volume`,
// or failing that its XLM leg valued through the on-chain XLM/USD VWAP —
// see [sorobanVolume24hUSDQuery] for the NUMERIC derivation and its scope
// boundary (pure SEP-41/SEP-41 legs still contribute 0).
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
