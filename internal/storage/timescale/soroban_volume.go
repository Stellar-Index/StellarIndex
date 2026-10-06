package timescale

import (
	"context"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// sorobanVolume24hUSDQuery derives the trailing-24h USD trade volume for
// an asset, for pure-Soroban SEP-41 tokens: [Store.Volume24hUSDForAsset]
// only sees the insert-time `usd_volume` column, so a Soroban token whose
// XLM-legged trades were stored unvalued would show a bogus "0".
//
// Valuation is per TRADE, never per prices_1m row: each trade contributes
// its insert-time `usd_volume` when present, else its XLM leg (base_amount
// or quote_amount, in stroops) at the XLM/USD anchor of the trade's own
// minute ([xlmUSDAnchorGridCTE]) — never today's rate. A trade with
// neither (no XLM leg, or no anchor within xlmUSDAnchorMaxAge of it) is
// excluded and counted, so the caller can flag the sum a lower bound.
//
// The window is the closed 1-minute buckets of the last 24h (ADR-0015).
// $1 binds the asset's canonical key (trades.base_asset / quote_asset
// form, e.g. a `C…` id); $2 the network's native-XLM SAC.
//
//nolint:gosec // G202: fragments are constant SQL built from literals and $N placeholders
var sorobanVolume24hUSDQuery = `
        WITH ` + xlmUSDAnchorGridCTE("xlm_usd_grid", "now() - INTERVAL '24 hours'", "$2::text") + `,
        asset_trades AS (
          SELECT time_bucket('1 minute', ts) AS bucket,
                 base_asset, quote_asset, base_amount, quote_amount, usd_volume
            FROM trades
           WHERE (base_asset = $1 OR quote_asset = $1)
             AND ts >= now() - INTERVAL '24 hours'
        ),
        valued AS (
          SELECT COALESCE(t.usd_volume, CASE
                   WHEN t.base_asset IN ('native', $2::text)
                     THEN (t.base_amount / 1e7::numeric) * xa.vwap
                   WHEN t.quote_asset IN ('native', $2::text)
                     THEN (t.quote_amount / 1e7::numeric) * xa.vwap
                 END) AS usd
            FROM asset_trades t
            ` + xlmUSDGridJoin("xa", "t.bucket") + `
           WHERE t.bucket >= now() - INTERVAL '24 hours'
             AND t.bucket <= now() - INTERVAL '1 minute'
        )
        SELECT COALESCE(sum(usd), 0)::text, count(*) FILTER (WHERE usd IS NULL)
          FROM valued
    `

// SorobanVolume24hUSDForAsset is the XLM-anchored trailing-24h USD-volume
// variant of [Store.Volume24hUSDForAsset], for pure-Soroban SEP-41 assets
// whose liquidity is quoted in XLM rather than a USD-pegged classic. See
// [sorobanVolume24hUSDQuery] for the trade-time valuation.
//
// Returns "0" (not an error) when the asset had no valued trades in the
// window. lowerBound is true when some trade in the window could not be
// valued and was excluded. `assetKey` is the canonical asset string
// trades.base_asset / quote_asset stores.
func (s *Store) SorobanVolume24hUSDForAsset(ctx context.Context, assetKey string) (usd string, lowerBound bool, err error) {
	var unpriced int64
	if err := s.db.QueryRowContext(ctx, sorobanVolume24hUSDQuery, assetKey, canonical.NativeSACContractID()).Scan(&usd, &unpriced); err != nil {
		return "", false, fmt.Errorf("timescale: SorobanVolume24hUSDForAsset(%s): %w", assetKey, err)
	}
	return usd, unpriced > 0, nil
}
