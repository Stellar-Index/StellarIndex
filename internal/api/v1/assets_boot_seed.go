package v1

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Boot-seeding the /v1/assets listing cache.
//
// The prewarm cannot beat the first /v1/assets requests (the cold aggregate takes ~10s), so boot
// seeds entries from the previous process's last-good page-set (persisted to Redis, see
// cmd/stellarindex-api/assets_listing_snapshot.go). [CachedAssetsReader.fetchRows] then takes
// branch (A'): serve stale at once, one detached refresh behind it, labelled `flags.stale`.
//
// The seeded entry's `at` is the snapshot's REAL observation time, never `time.Now()`, so age
// resets only on a real upstream refresh; [Server.listAssetsExtAt] hands it to the handler.

// listAssetsCacheKey derives the [CachedAssetsReader.entries] key for a ListAssetsExt call.
//
// It MUST stay byte-identical to the key expression in [CachedAssetsReader.ListAssetsExt]: a seed
// under a different key is a phantom slot that leaves callers on the cold path while the cache
// looks healthy. TestListAssetsCacheKeyMatchesListAssetsExt fails on any divergence, even field
// order.
func listAssetsCacheKey(opts timescale.ListAssetsOptions) string {
	return newCacheKey("ListAssetsExt").
		int(opts.Limit).str(opts.Issuer).str(opts.Code).str(opts.Type).
		str(opts.Cursor).str(opts.Q).order(int(opts.Order)).build()
}

// SeedListing installs a previously observed page-set stamped with its real observation time and reports whether it did; it refuses empty, zero or future-dated rows and never clobbers a live entry.
func (c *CachedAssetsReader) SeedListing(
	opts timescale.ListAssetsOptions, rows []timescale.AssetRow, observedAt time.Time,
) bool {
	if c == nil || c.ttl <= 0 || len(rows) == 0 {
		return false
	}
	if observedAt.IsZero() || observedAt.After(time.Now()) {
		return false
	}
	key := listAssetsCacheKey(opts)

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; exists {
		return false
	}
	evictOldestAssetsEntry(c.entries)
	c.entries[key] = &assetsCacheEntry{at: observedAt, rows: rows}
	return true
}

// ListAssetsExtAt is [CachedAssetsReader.ListAssetsExt] plus observation time and staleness so /v1/assets stamps an honest as_of; observedAt is zero when the cache is disabled.
func (c *CachedAssetsReader) ListAssetsExtAt(
	ctx context.Context, opts timescale.ListAssetsOptions,
) ([]timescale.AssetRow, time.Time, bool, error) {
	if c.ttl <= 0 {
		rows, err := c.upstream.ListAssetsExt(ctx, opts)
		return rows, time.Time{}, false, err
	}
	key := listAssetsCacheKey(opts)

	// Rows and observation time must come from the SAME lock acquisition: sampling `e.at` first
	// races a refresh and stamps fresh rows with the stale entry's as_of and freshness flag.
	rows, at, err := c.fetchRowsAt(ctx, "list_coins", key,
		func(ctx context.Context) ([]timescale.AssetRow, error) {
			return c.upstream.ListAssetsExt(ctx, opts)
		})
	if err != nil {
		return nil, time.Time{}, false, err
	}
	// A zero `at` means the rows were answered live, which the ...At contract defines as fresh.
	if at.IsZero() {
		return rows, time.Time{}, false, nil
	}
	return rows, at, time.Since(at) >= c.ttl, nil
}

// listAssetsExtAt reads through [CachedAssetsReader.ListAssetsExtAt] when the wired reader has it,
// else the plain AssetsReader call. The type-assert avoids widening [AssetsReader] for every test
// double and the bare store. The degraded (zero time, false) is deliberate: an uncached reader
// answers live, so the caller stamps as_of=now.
func (s *Server) listAssetsExtAt(
	ctx context.Context, opts timescale.ListAssetsOptions,
) ([]timescale.AssetRow, time.Time, bool, error) {
	if at, ok := s.AssetsReader.(interface {
		ListAssetsExtAt(context.Context, timescale.ListAssetsOptions) ([]timescale.AssetRow, time.Time, bool, error)
	}); ok {
		return at.ListAssetsExtAt(ctx, opts)
	}
	rows, err := s.AssetsReader.ListAssetsExt(ctx, opts)
	return rows, time.Time{}, false, err
}
