package v1

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// AssetsReader is the seam /v1/assets handlers read catalogue data through (price, volume, market_cap, sparkline, ATH); timescale.Store satisfies it.
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

// AssetATH is the all-time-high USD price and bucket day, embedded in AssetDetail.ATH.
type AssetATH struct {
	USD string `json:"usd"`
	At  string `json:"at"`
}

// AssetTopMarket is one top-markets preview entry; `Counterparty` is the other side of the pair and `Side` is "base" or "quote".
type AssetTopMarket struct {
	Counterparty  string  `json:"counterparty"`
	Side          string  `json:"side"`
	Volume24hUSD  *string `json:"volume_24h_usd,omitempty"`
	TradeCount24h int64   `json:"trade_count_24h"`
}

// AssetPricePoint is one USD-price sample: `T` is the bucket end (RFC3339), `P` the 10dp price or nil when the bucket had no VWAP.
type AssetPricePoint struct {
	T string  `json:"t"`
	P *string `json:"p,omitempty"`
}
