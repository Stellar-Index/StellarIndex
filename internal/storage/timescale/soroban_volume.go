package timescale

import (
	"context"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// sorobanVolume24hUSDQuery derives the trailing-24h USD trade volume for a pure-Soroban SEP-41
// asset. [Store.Volume24hUSDForAsset] only sees the insert-time `usd_volume`, so a token whose
// XLM-legged trades were stored unvalued would show a bogus "0".
//
// Valuation is per TRADE, never per prices_1m row: each trade contributes its `usd_volume` when
// present, else its XLM leg (stroops) at the XLM/USD anchor of the trade's own minute
// ([xlmUSDAnchorGridCTE]), never today's rate. A trade with neither (no XLM leg, or no anchor
// within xlmUSDAnchorMaxAge) is excluded and counted so the caller can flag a lower bound.
//
// The window is the closed 1-minute buckets of the last 24h. $1 binds the asset's canonical key
// as stored in trades.base_asset / quote_asset (e.g. a `C…` id); $2 the native-XLM SAC.
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

// SorobanVolume24hUSDForAsset is the XLM-anchored variant of [Store.Volume24hUSDForAsset] for
// pure-Soroban SEP-41 assets quoted in XLM; see [sorobanVolume24hUSDQuery] for the valuation.
//
// It returns "0" (not an error) when no trade in the window could be valued. lowerBound is true
// when some trade was excluded as unvaluable. `assetKey` is the canonical asset string stored in
// trades.base_asset / quote_asset.
func (s *Store) SorobanVolume24hUSDForAsset(ctx context.Context, assetKey string) (usd string, lowerBound bool, err error) {
	var unpriced int64
	if err := s.db.QueryRowContext(ctx, sorobanVolume24hUSDQuery, assetKey, canonical.NativeSACContractID()).Scan(&usd, &unpriced); err != nil {
		return "", false, fmt.Errorf("timescale: SorobanVolume24hUSDForAsset(%s): %w", assetKey, err)
	}
	return usd, unpriced > 0, nil
}
