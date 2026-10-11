package v1

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Boot-seeding the /v1/assets listing cache.
//
// The prewarm cannot beat the first /v1/assets requests (the cold aggregate takes ~10s), so
// boot seeds entries from the previous process's last-good page-set (persisted to Redis, see
// cmd/stellarindex-api/assets_listing_snapshot.go) with the rows' REAL observation time.
// [CachedAssetsReader.fetchRows] then takes branch (A'): serve stale at once, one detached
// refresh behind it, labelled `flags.stale`.
//
// Two invariants keep that honest:
//
//   - The seeded entry's `at` is the snapshot's observation time, never `time.Now()`, so age
//     resets only on a real upstream refresh.
//   - [Server.listAssetsExtAt] reports observation time and staleness to the handler.

// listAssetsCacheKey derives the [CachedAssetsReader.entries] key for a
// ListAssetsExt call.
//
// This MUST stay byte-identical to the key expression in [CachedAssetsReader.ListAssetsExt].
// A seed under a different key is a phantom slot that leaves every caller on the cold path
// while the cache looks healthy. That is invisible at runtime, so
// TestListAssetsCacheKeyMatchesListAssetsExt observes the key ListAssetsExt really creates
// and fails on any divergence, even a field order.
func listAssetsCacheKey(opts timescale.ListAssetsOptions) string {
	return newCacheKey("ListAssetsExt").
		int(opts.Limit).str(opts.Issuer).str(opts.Code).str(opts.Type).
		str(opts.Cursor).str(opts.Q).order(int(opts.Order)).build()
}

// SeedListing installs a previously-observed listing page-set as a cache entry for `opts`,
// timestamped with the rows' real observation time, and reports whether it was installed.
//
// It refuses (false, leaving cold-fill behaviour) when:
//
//   - the cache is disabled (ttl <= 0);
//   - `rows` is empty, since a seed must never publish an empty listing;
//   - `observedAt` is zero or in the future: a future-dated entry would read as fresh forever;
//   - an entry already exists at the key, so a live fill or in-flight waiters are never clobbered.
//
// Callers apply their own maximum age (assetsListingSeedMaxAge).
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

// ListAssetsExtAt is [CachedAssetsReader.ListAssetsExt] plus the served row-set's observation
// time and staleness flag, so /v1/assets stamps an honest `as_of` and `flags.stale` over rows
// from an expired or boot-seeded entry. observedAt is the zero time (caller stamps as_of=now,
// not stale) when the cache is disabled, since an uncached read is live-fresh.
func (c *CachedAssetsReader) ListAssetsExtAt(
	ctx context.Context, opts timescale.ListAssetsOptions,
) ([]timescale.AssetRow, time.Time, bool, error) {
	if c.ttl <= 0 {
		rows, err := c.upstream.ListAssetsExt(ctx, opts)
		return rows, time.Time{}, false, err
	}
	key := listAssetsCacheKey(opts)

	// Rows and their observation time come back from the SAME lock acquisition. Sampling
	// `e.at` first is a race: a refresh in between returns fresh rows stamped with the stale
	// entry's time, publishing correct data under a wrong as_of and freshness flag.
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

// listAssetsExtAt reads the listing through [CachedAssetsReader.ListAssetsExtAt] when the wired
// reader provides it, else degrades to the plain AssetsReader call.
//
// The type-assert avoids widening the narrow [AssetsReader] interface, which would oblige every
// test double and the bare store to implement it. The degraded return is deliberately (zero
// time, false): a reader with no cache answers live, so the caller stamps as_of=now.
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
