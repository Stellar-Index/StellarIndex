package v1

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// CachedLendingReader wraps [LendingReader] with a stale-while-revalidate cache on ListBlendPools, which aggregates both Blend
// tables in full (~350 ms on r1) on every call; the per-pool reads pass through.
type CachedLendingReader struct {
	upstream LendingReader
	ttl      time.Duration
	pools    *swrCache[[]timescale.BlendPoolSummary]
}

// NewCachedLendingReader wraps upstream; ttl=0 disables caching.
func NewCachedLendingReader(upstream LendingReader, ttl time.Duration) *CachedLendingReader {
	return &CachedLendingReader{
		upstream: upstream,
		ttl:      ttl,
		pools:    newSWRCache[[]timescale.BlendPoolSummary](nil, "lending_pools", "api-lending-pools-fill", ttl, 0),
	}
}

// ListBlendPools is ListBlendPoolsAt without the fill time.
func (c *CachedLendingReader) ListBlendPools(ctx context.Context) ([]timescale.BlendPoolSummary, error) {
	rows, _, _, err := c.ListBlendPoolsAt(ctx)
	return rows, err
}

// ListBlendPoolsAt returns the pool list, its fill time (zero on an uncached read) and whether it is past its TTL.
func (c *CachedLendingReader) ListBlendPoolsAt(ctx context.Context) ([]timescale.BlendPoolSummary, time.Time, bool, error) {
	if c.ttl <= 0 {
		rows, err := c.upstream.ListBlendPools(ctx)
		return rows, time.Time{}, false, err
	}
	rows, at, stale, err := c.pools.get(ctx, "list_blend_pools", "ListBlendPools", c.upstream.ListBlendPools)
	// Cap capacity so a caller's append cannot write into the shared entry.
	return rows[:len(rows):len(rows)], at, stale, err
}

// BlendPoolAssets is a pass-through.
func (c *CachedLendingReader) BlendPoolAssets(ctx context.Context, pool string) ([]string, error) {
	return c.upstream.BlendPoolAssets(ctx, pool)
}

// BlendPoolVersion is a pass-through.
func (c *CachedLendingReader) BlendPoolVersion(ctx context.Context, pool string) (blend.PoolVersion, error) {
	return c.upstream.BlendPoolVersion(ctx, pool)
}

// BlendReserveConfigs is a pass-through.
func (c *CachedLendingReader) BlendReserveConfigs(ctx context.Context, pool string) (map[string]blend.ReserveConfig, error) {
	return c.upstream.BlendReserveConfigs(ctx, pool)
}
