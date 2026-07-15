package v1

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// AssetsReader is the seam the /v1/assets handlers read through to
// surface asset-catalogue data (price / volume / market_cap /
// sparkline / ATH). The legacy /v1/coins HTTP surface has been
// removed (no production consumers); this interface stays because
// /v1/assets sources the same data through it.
//
// timescale.Store satisfies it via ListAssetsExt + GetAssetBySlug
// + GetNativeAssetRow + GetAssetTopMarkets + GetAssetPriceHistory24h
// + GetAssetMarketsCount.
type AssetsReader interface {
	ListAssetsExt(ctx context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error)
	GetAssetBySlug(ctx context.Context, slug string) (timescale.AssetRow, error)
	// GetAssetByAssetID looks up a classic asset by its canonical
	// asset_id (CODE-ISSUER form). Distinct from [GetAssetBySlug]
	// which looks up by the friendly short slug (USDC, AQUA).
	GetAssetByAssetID(ctx context.Context, assetID string) (timescale.AssetRow, error)
	GetNativeAssetRow(ctx context.Context) (timescale.AssetRow, error)
	GetAssetTopMarkets(ctx context.Context, assetID string, limit int) ([]timescale.AssetTopMarket, error)
	GetAssetPriceHistory24h(ctx context.Context, assetID string) ([]timescale.AssetPricePoint, error)
	GetAssetPriceHistory7d(ctx context.Context, assetID string) ([]timescale.AssetPricePoint, error)
	GetAssetsPriceHistory24hBatch(ctx context.Context, assetIDs []string) (map[string][]timescale.AssetPricePoint, error)
	GetAssetsPriceHistory7dBatch(ctx context.Context, assetIDs []string) (map[string][]timescale.AssetPricePoint, error)
	GetAssetMarketsCount(ctx context.Context, assetID string) (int64, error)
	GetAssetATH(ctx context.Context, assetID string) (*timescale.AssetATH, error)
	GetAssetsATHBatch(ctx context.Context, assetIDs []string) (map[string]timescale.AssetATH, error)
	GetAssetTradeCount24h(ctx context.Context, assetID string) (int64, error)
}

// AssetATH is the all-time-high USD price + bucket-day pair. Embedded
// in AssetDetail.ATH.
type AssetATH struct {
	USD string `json:"usd"`
	At  string `json:"at"`
}

// AssetTopMarket is one entry in the per-asset top-markets preview.
// `Counterparty` is the OTHER side of the pair (the asset that's
// not the one being queried). `Side` is "base" or "quote".
type AssetTopMarket struct {
	Counterparty  string  `json:"counterparty"`
	Side          string  `json:"side"`
	Volume24hUSD  *string `json:"volume_24h_usd,omitempty"`
	TradeCount24h int64   `json:"trade_count_24h"`
}

// AssetPricePoint is one hourly/daily USD-price sample in
// AssetDetail.PriceHistory24h / PriceHistory7d. `T` is the bucket
// end as an RFC3339 timestamp; `P` is the rounded-to-10dp USD price
// or nil when no trades that bucket produced a VWAP.
type AssetPricePoint struct {
	T string  `json:"t"`
	P *string `json:"p,omitempty"`
}
