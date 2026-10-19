package v1

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// marketSourcesCacheMaxEntries bounds the map: keys are client-chosen asset ids.
const marketSourcesCacheMaxEntries = 1024

// CachedMarketSourceReader wraps [MarketSourceReader] with a stale-while-revalidate cache: the per-asset aggregate takes ~2 s for
// XLM on r1 and backs a pie on every asset and pair page, while a trailing-24h share barely moves within the TTL.
type CachedMarketSourceReader struct {
	upstream MarketSourceReader
	cache    *swrCache[[]timescale.SourceStats]
}

// NewCachedMarketSourceReader wraps upstream with a TTL cache.
func NewCachedMarketSourceReader(upstream MarketSourceReader, ttl time.Duration) *CachedMarketSourceReader {
	return &CachedMarketSourceReader{
		upstream: upstream,
		cache:    newSWRCache[[]timescale.SourceStats](nil, "market_sources", "api-market-sources-fill", ttl, marketSourcesCacheMaxEntries),
	}
}

// PairSourceStats is PairSourceStatsAt without the vintage.
func (c *CachedMarketSourceReader) PairSourceStats(ctx context.Context, base, quote []string) ([]timescale.SourceStats, error) {
	rows, _, _, err := c.PairSourceStatsAt(ctx, base, quote)
	return rows, err
}

// AssetSourceStats is AssetSourceStatsAt without the vintage.
func (c *CachedMarketSourceReader) AssetSourceStats(ctx context.Context, asset []string) ([]timescale.SourceStats, error) {
	rows, _, _, err := c.AssetSourceStatsAt(ctx, asset)
	return rows, err
}

// PairSourceStatsAt also returns the served entry's fill time and whether it is past its TTL.
func (c *CachedMarketSourceReader) PairSourceStatsAt(ctx context.Context, base, quote []string) ([]timescale.SourceStats, time.Time, bool, error) {
	key := newCacheKey("PairSourceStats").strSet(base).strSet(quote).build()
	return c.cache.get(ctx, "pair", key, func(ctx context.Context) ([]timescale.SourceStats, error) {
		return c.upstream.PairSourceStats(ctx, base, quote)
	})
}

// AssetSourceStatsAt also returns the served entry's fill time and whether it is past its TTL.
func (c *CachedMarketSourceReader) AssetSourceStatsAt(ctx context.Context, asset []string) ([]timescale.SourceStats, time.Time, bool, error) {
	key := newCacheKey("AssetSourceStats").strSet(asset).build()
	return c.cache.get(ctx, "asset", key, func(ctx context.Context) ([]timescale.SourceStats, error) {
		return c.upstream.AssetSourceStats(ctx, asset)
	})
}
