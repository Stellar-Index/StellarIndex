package v1

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Boot-seeding the /v1/assets listing cache.
//
// A race the prewarm cannot win: the process listens within milliseconds, the
// first /v1/assets requests arrive within seconds, and the cold listing
// aggregate takes ~10s. The cost IS the first fill.
//
// So seed the entries at boot from the previous process's last-good page-set
// (persisted to Redis by the prewarm, see
// cmd/stellarindex-api/assets_listing_snapshot.go), with the rows' REAL
// observation time. [CachedAssetsReader.fetchRows] then takes branch (A'):
// serve stale immediately, one detached refresh behind it, honestly labelled
// `flags.stale` with the true `as_of`.
//
// Two invariants keep that honest:
//
//   - The seeded entry's `at` is the snapshot's observation time, never
//     `time.Now()`, so a snapshot re-persisted from a stale serve keeps its
//     ORIGINAL time and age resets only on a real upstream refresh.
//   - [Server.listAssetsExtAt] reports observation time and staleness to the
//     handler, mirroring [CachedMarketsReader.DistinctPairsExtAt] (same
//     `Flags{Stale: stale}` + `env.AsOf = observedAt`): one staleness
//     convention.

// listAssetsCacheKey derives the [CachedAssetsReader.entries] key for a
// ListAssetsExt call.
//
// DRIFT WARNING: this MUST stay byte-identical to the key expression in
// [CachedAssetsReader.ListAssetsExt]. A seed written under a different
// key is a phantom slot — it costs a Redis round trip per boot and
// leaves every caller on the 11 s cold path, reporting a healthy cache
// the whole time. That failure is invisible at runtime, so it is
// asserted instead: TestListAssetsCacheKeyMatchesListAssetsExt derives
// the real key by observing which entry ListAssetsExt actually creates
// and fails if the two expressions diverge by so much as a field order.
func listAssetsCacheKey(opts timescale.ListAssetsOptions) string {
	return newCacheKey("ListAssetsExt").
		int(opts.Limit).str(opts.Issuer).str(opts.Code).str(opts.Type).
		str(opts.Cursor).str(opts.Q).order(int(opts.Order)).build()
}

// SeedListing installs a previously-observed listing page-set as a
// cache entry for `opts`, timestamped with the rows' real observation
// time. It reports whether the seed was installed.
//
// Refuses (returns false, leaving today's cold-fill behaviour intact)
// when:
//
//   - the cache is disabled (ttl <= 0) — there are no entries to seed;
//   - `rows` is empty — a boot seed must never be able to publish an
//     empty listing where the uncached path would have published the
//     real one;
//   - `observedAt` is zero or in the future — an entry we cannot date
//     cannot be labelled honestly, and a future-dated one would read
//     as fresh forever;
//   - an entry already exists at the key — the seed runs before the
//     listener starts, but it must never clobber a live fill or orphan
//     the waiters of an in-flight one.
//
// Callers apply their own maximum age before calling; this function
// deliberately accepts any past `observedAt` so the age policy lives in
// one place (assetsListingSeedMaxAge) rather than being split in two.
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

// ListAssetsExtAt is [CachedAssetsReader.ListAssetsExt] plus the served
// row-set's observation time and staleness flag — the same shape, and
// the same purpose, as [CachedMarketsReader.DistinctPairsExtAt]:
// /v1/assets uses it to stamp an honest `as_of` and `flags.stale`
// instead of asserting `stale:false` / `as_of=now` over rows the SWR
// path served from an expired (or boot-seeded) entry.
//
// observedAt is the zero time — caller stamps as_of=now, not stale —
// when the cache is disabled, since an uncached read comes straight
// from upstream and is live-fresh.
//
// How the meta is obtained without a second, racier source of truth:
// the entry state is snapshotted BEFORE the fetch. If a servable entry
// existed then, fetchRows served exactly it (branch A or A'), and its
// pre-call `at` is that row-set's observation time. Otherwise fetchRows
// filled inline (branch B or C) and the post-call `at` is the fill
// time. The only way the pre-call read can be wrong is if a background
// refresh landed between the snapshot and fetchRows' own read — in
// which case we under-state freshness (report the older observation
// over newer rows), which is the conservative direction.
func (c *CachedAssetsReader) ListAssetsExtAt(
	ctx context.Context, opts timescale.ListAssetsOptions,
) ([]timescale.AssetRow, time.Time, bool, error) {
	if c.ttl <= 0 {
		rows, err := c.upstream.ListAssetsExt(ctx, opts)
		return rows, time.Time{}, false, err
	}
	key := listAssetsCacheKey(opts)

	// Rows and their observation time come back from the SAME lock
	// acquisition. Sampling `e.at` first and pairing it with whatever
	// rows arrived afterwards is a race: a refresh landing in between
	// returns FRESH rows stamped with the STALE entry's time and
	// stale=true, so the handler publishes correct data under a wrong
	// as_of and a wrong freshness flag.
	rows, at, err := c.fetchRowsAt(ctx, "list_coins", key,
		func(ctx context.Context) ([]timescale.AssetRow, error) {
			return c.upstream.ListAssetsExt(ctx, opts)
		})
	if err != nil {
		return nil, time.Time{}, false, err
	}
	// A zero `at` means the rows were answered live rather than from an
	// entry, which the …At contract defines as fresh.
	if at.IsZero() {
		return rows, time.Time{}, false, nil
	}
	return rows, at, time.Since(at) >= c.ttl, nil
}

// listAssetsExtAt reads the listing through [CachedAssetsReader.ListAssetsExtAt]
// when the wired reader provides it, and degrades to the plain
// AssetsReader call otherwise.
//
// The type-assert mirrors the one CachedAssetsReader.LatestCirculatingSupply
// documents: the handler holds the narrow [AssetsReader] interface, so a
// capability the concrete cache adds has to be recovered here rather than
// widened onto the interface (which would oblige every test double and the
// bare store to implement it).
//
// The degraded return is deliberately (zero time, false): a reader with no
// cache is answering live, which is exactly the "fresh, caller stamps
// as_of=now" case the …At contract defines.
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
