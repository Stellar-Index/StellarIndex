package v1

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// CachedIssuersReader wraps [IssuersReader] with a per-process TTL cache and single-flight for the expensive list; GetIssuer and ListIssuerAssets pass through.
type CachedIssuersReader struct {
	upstream IssuersReader
	ttl      time.Duration
	// list is stale-while-revalidate because the scan takes 3-4 s on r1.
	list *swrCache[[]timescale.IssuerSummary]
}

// NewCachedIssuersReader wraps upstream with a TTL cache; ttl=0 disables caching and the caller picks the TTL.
func NewCachedIssuersReader(upstream IssuersReader, ttl time.Duration) *CachedIssuersReader {
	return &CachedIssuersReader{
		upstream: upstream,
		ttl:      ttl,
		list:     newSWRCache[[]timescale.IssuerSummary](nil, "issuers", "api-issuers-fill", ttl, 0),
	}
}

// GetIssuer is a pass-through: a sub-ms PK lookup that per-strkey caching would only scatter.
func (c *CachedIssuersReader) GetIssuer(ctx context.Context, gStrkey string) (timescale.IssuerRow, error) {
	return c.upstream.GetIssuer(ctx, gStrkey)
}

// ListIssuerAssets is a pass-through bounded by timescale.issuerAssetsHardCap, needed because spammers mint many codes and airdrop trustlines.
func (c *CachedIssuersReader) ListIssuerAssets(ctx context.Context, gStrkey string) ([]timescale.IssuerAsset, error) {
	return c.upstream.ListIssuerAssets(ctx, gStrkey)
}

// ListIssuers is cached as one IssuersListMaxLimit page that every request prefix-slices, so sweeping `?limit=` cannot force one upstream scan per value.
func (c *CachedIssuersReader) ListIssuers(ctx context.Context, limit int) ([]timescale.IssuerSummary, error) {
	list, _, _, err := c.ListIssuersAt(ctx, limit)
	return list, err
}

// ListIssuersAt is ListIssuers plus the served entry's fill time (zero on an uncached, ttl<=0, read) and whether it is past its TTL.
func (c *CachedIssuersReader) ListIssuersAt(ctx context.Context, limit int) ([]timescale.IssuerSummary, time.Time, bool, error) {
	if c.ttl <= 0 {
		list, err := c.upstream.ListIssuers(ctx, limit)
		return list, time.Time{}, false, err
	}
	key := newCacheKey("ListIssuers").int(IssuersListMaxLimit).build()
	list, at, stale, err := c.list.get(ctx, "list_issuers", key, func(ctx context.Context) ([]timescale.IssuerSummary, error) {
		return c.upstream.ListIssuers(ctx, IssuersListMaxLimit)
	})
	if err != nil || limit <= 0 || limit >= len(list) {
		return list, at, stale, err
	}
	// Cap capacity so a caller's append cannot write into the shared entry.
	return list[:limit:limit], at, stale, nil
}
