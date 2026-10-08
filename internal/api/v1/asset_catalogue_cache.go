package v1

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// CachedAssetsReader wraps a [AssetsReader] with a small per-key TTL
// cache for the methods that back high-traffic listing endpoints —
// ListAssetsExt and the batched price-history calls used by
// /v1/assets?limit=200&include=sparkline. /v1/assets sources its data
// through this seam. The unified listing fires that exact request
// on every page load and the underlying SQL takes ~1.1s; without
// this cache the explorer's time-to-interactive is gated on it.
//
// All other AssetsReader methods (single-asset lookups, ATH, market
// counts) pass through unchanged — they're keyed too narrowly to
// benefit and most are already fast.
//
// Single-flight: concurrent callers during a refetch share one
// upstream call, mirroring the SourcesStats / Markets caches.
type CachedAssetsReader struct {
	// logger sinks a panic recovered in a detached refresh goroutine.
	// It is the PROCESS DEFAULT rather than the API Server's logger:
	// these reader wrappers are constructed directly in
	// cmd/stellarindex-api/main.go before the Server exists, and
	// widening the exported constructor would churn every caller for a
	// sink that ends up in the same journal either way. The
	// load-bearing signal is stellarindex_worker_panics_total, which
	// worker.Report moves regardless of the sink.
	logger *slog.Logger

	upstream AssetsReader
	ttl      time.Duration

	mu sync.Mutex
	// entries backs the legacy method-field caches (ListAssetsExt /
	// history batches via fetchRows / fetchHistoryMap).
	entries map[string]*assetsCacheEntry
	// swrEntries backs the generic single-value SWR path (swr[T] —
	// the per-asset single-row methods). Distinct map, SAME mu.
	swrEntries map[string]*swrEntry
}

// assetsCacheMaxEntries bounds the entry map.
//
// Cache keys carry client-controlled components — notably `cursor`,
// which is validated for SHAPE ONLY (it need merely contain the right
// delimiter; there is no allowlist, HMAC or canonical round-trip), plus
// free-text `q` and asset ids. Without eviction, one anonymous caller
// inside the documented 6000/min budget could mint a permanent,
// full-page entry per distinct cursor — hundreds of MB per minute of
// unreclaimable heap, walking the process to its 8G MemoryMax and into
// a systemd restart-storm.
//
// The cap + oldest-first eviction matches the bounded siblings
// (historyCacheMaxEntries, accountStateCacheMax, assetDetail) and sits
// far above the real working set: the prewarmer covers a handful of
// shapes and legitimate pagination walks a bounded number of pages.
const assetsCacheMaxEntries = 4096

// evictOldestAssetsEntry drops the oldest-filled entry when the map is at
// capacity. Caller must hold the mutex.
//
// In-flight entries (at.IsZero()) are never evicted — dropping one
// would orphan its waiters from the fill about to close their channel.
func evictOldestAssetsEntry(m map[string]*assetsCacheEntry) {
	if len(m) < assetsCacheMaxEntries {
		return
	}
	var (
		oldestKey string
		oldestAt  time.Time
	)
	for k, e := range m {
		if e.at.IsZero() {
			continue
		}
		if oldestKey == "" || e.at.Before(oldestAt) {
			oldestKey, oldestAt = k, e.at
		}
	}
	if oldestKey != "" {
		delete(m, oldestKey)
	}
}

type assetsCacheEntry struct {
	at     time.Time
	flight chan struct{}

	// One field per method we cache. Only one is populated per entry.
	rows           []timescale.AssetRow
	historyByAsset map[string][]timescale.AssetPricePoint

	// err is set by the leader before close(flight) on a failing
	// upstream call. Waiters hold a pointer to the SAME entry they
	// joined the flight on so they can read entry.err here even
	// after the leader removes the entry from the map (we don't
	// TTL-cache errors). Without this, a waiter that wakes after
	// the leader's delete derefs `c.entries[key].rows` on nil and
	// panics — same root cause as the markets_cache fix.
	err error
}

// NewCachedAssetsReader wraps `upstream` with a TTL cache. ttl=0
// disables the cache (every call passes through).
//
// The production TTL is **2 minutes**, set at the single call site
// (cmd/stellarindex-api/main.go, `NewCachedAssetsReader(store,
// 2*time.Minute)`) — listings are activity-ranked aggregates that don't
// move materially in that window, and the explorer's react-query layer
// caches client-side on top. Change the call site and this line
// together.
func NewCachedAssetsReader(upstream AssetsReader, ttl time.Duration) *CachedAssetsReader {
	return &CachedAssetsReader{
		logger:     slog.Default(),
		upstream:   upstream,
		ttl:        ttl,
		entries:    map[string]*assetsCacheEntry{},
		swrEntries: map[string]*swrEntry{},
	}
}

// LatestSupplyObservations passes through to the upstream's supply
// reader (used by the /v1/assets market_cap enrichment). Not part of
// AssetsReader — exposed so the handler's type-assert resolves through
// this wrapper instead of skipping enrichment. Uncached: the underlying
// observation lookup is one indexed row per watched asset.
//
// Deliberately NOT cached here on top of that. The freshness bound the
// caller passes is the whole point of the read, and a cache in front of it
// would age the observation by its own TTL behind the bound's back — which
// is a smaller version of exactly the defect that made this method exist.
func (c *CachedAssetsReader) LatestSupplyObservations(
	ctx context.Context, maxAge time.Duration,
) (map[string]timescale.SupplyObservation, error) {
	if sr, ok := c.upstream.(interface {
		LatestSupplyObservations(context.Context, time.Duration) (map[string]timescale.SupplyObservation, error)
	}); ok {
		return sr.LatestSupplyObservations(ctx, maxAge)
	}
	return nil, nil
}

// ListAssetsExt — cached on a key derived from the options struct.
func (c *CachedAssetsReader) ListAssetsExt(ctx context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	if c.ttl <= 0 {
		return c.upstream.ListAssetsExt(ctx, opts)
	}
	// Every ListAssetsOptions dimension that changes the result set
	// MUST appear in the key, or two requests differing only by that
	// dimension collide and one serves the other's rows. Code is a
	// row-narrowing filter, so it is keyed alongside
	// Issuer/Cursor/Q — and so is Type, which reaches the store now
	// that it narrows the spine (classic vs the traded Soroban-native
	// contracts) rather than being folded away by the handler.
	key := newCacheKey("ListAssetsExt").
		int(opts.Limit).str(opts.Issuer).str(opts.Code).str(opts.Type).
		str(opts.Cursor).str(opts.Q).order(int(opts.Order)).build()
	return c.fetchRows(ctx, "list_coins", key, func(ctx context.Context) ([]timescale.AssetRow, error) {
		return c.upstream.ListAssetsExt(ctx, opts)
	})
}

// GetAssetsPriceHistory24hBatch — cached on the asset-id set.
// [cacheKey.strSet] order-normalises the ids so two callers passing
// the same set in a different order share one slot (the result is a
// map keyed by asset_id — order-independent).
func (c *CachedAssetsReader) GetAssetsPriceHistory24hBatch(ctx context.Context, assetIDs []string) (map[string][]timescale.AssetPricePoint, error) {
	if c.ttl <= 0 {
		return c.upstream.GetAssetsPriceHistory24hBatch(ctx, assetIDs)
	}
	key := newCacheKey("Price24hBatch").strSet(assetIDs).build()
	return c.fetchHistoryMap(ctx, "price_history_24h", key, func(ctx context.Context) (map[string][]timescale.AssetPricePoint, error) {
		return c.upstream.GetAssetsPriceHistory24hBatch(ctx, assetIDs)
	})
}

// GetAssetsPriceHistory7dBatch — cached. Same order-normalised keying
// as the 24h batch.
func (c *CachedAssetsReader) GetAssetsPriceHistory7dBatch(ctx context.Context, assetIDs []string) (map[string][]timescale.AssetPricePoint, error) {
	if c.ttl <= 0 {
		return c.upstream.GetAssetsPriceHistory7dBatch(ctx, assetIDs)
	}
	key := newCacheKey("Price7dBatch").strSet(assetIDs).build()
	return c.fetchHistoryMap(ctx, "price_history_7d", key, func(ctx context.Context) (map[string][]timescale.AssetPricePoint, error) {
		return c.upstream.GetAssetsPriceHistory7dBatch(ctx, assetIDs)
	})
}

// GetAssetsATHBatch passes through uncached: no handler serves it on a
// hot path, so a cache would add a fetcher type for no measured win.
func (c *CachedAssetsReader) GetAssetsATHBatch(ctx context.Context, assetIDs []string) (map[string]timescale.AssetATH, error) {
	return c.upstream.GetAssetsATHBatch(ctx, assetIDs)
}

// Per-asset single-value reads — all stale-while-revalidate cached
// via the generic swr[T] helper. These were pass-through on the
// assumption they were "<50ms low-volume single-row" lookups; that
// became false post-backfill: /v1/assets/{id}'s asset-catalogue extension
// fans out ~9 of these per request and GetAssetByAssetID /
// GetNativeAssetRow run the whole-asset-universe listAssetsBaseSelect
// query (~13s under load), with GetAssetTradeCount24h /
// GetAssetMarketsCount adding multi-second trades-OR scans (385d564e8).
// SWR moves all of that off the request path with zero correctness
// loss (serve stale instantly, single-flighted background refresh).

func (c *CachedAssetsReader) GetAssetBySlug(ctx context.Context, slug string) (timescale.AssetRow, error) {
	if c.ttl <= 0 {
		return c.upstream.GetAssetBySlug(ctx, slug)
	}
	return swr(ctx, c, "coin_by_slug", "GetAssetBySlug|"+slug,
		func(ctx context.Context) (timescale.AssetRow, error) {
			return c.upstream.GetAssetBySlug(ctx, slug)
		})
}

func (c *CachedAssetsReader) GetAssetByAssetID(ctx context.Context, assetID string) (timescale.AssetRow, error) {
	if c.ttl <= 0 {
		return c.upstream.GetAssetByAssetID(ctx, assetID)
	}
	return swr(ctx, c, "coin_by_asset", "GetAssetByAssetID|"+assetID,
		func(ctx context.Context) (timescale.AssetRow, error) {
			return c.upstream.GetAssetByAssetID(ctx, assetID)
		})
}

func (c *CachedAssetsReader) GetNativeAssetRow(ctx context.Context) (timescale.AssetRow, error) {
	if c.ttl <= 0 {
		return c.upstream.GetNativeAssetRow(ctx)
	}
	return swr(ctx, c, "native_coin", "GetNativeAssetRow",
		func(ctx context.Context) (timescale.AssetRow, error) {
			return c.upstream.GetNativeAssetRow(ctx)
		})
}

func (c *CachedAssetsReader) GetAssetTopMarkets(ctx context.Context, assetID string, limit int) ([]timescale.AssetTopMarket, error) {
	if c.ttl <= 0 {
		return c.upstream.GetAssetTopMarkets(ctx, assetID, limit)
	}
	return swr(ctx, c, "coin_top_markets", newCacheKey("GetAssetTopMarkets").str(assetID).int(limit).build(),
		func(ctx context.Context) ([]timescale.AssetTopMarket, error) {
			return c.upstream.GetAssetTopMarkets(ctx, assetID, limit)
		})
}

func (c *CachedAssetsReader) GetAssetPriceHistory24h(ctx context.Context, assetID string) ([]timescale.AssetPricePoint, error) {
	if c.ttl <= 0 {
		return c.upstream.GetAssetPriceHistory24h(ctx, assetID)
	}
	return swr(ctx, c, "coin_hist_24h", "GetAssetPriceHistory24h|"+assetID,
		func(ctx context.Context) ([]timescale.AssetPricePoint, error) {
			return c.upstream.GetAssetPriceHistory24h(ctx, assetID)
		})
}

func (c *CachedAssetsReader) GetAssetPriceHistory7d(ctx context.Context, assetID string) ([]timescale.AssetPricePoint, error) {
	if c.ttl <= 0 {
		return c.upstream.GetAssetPriceHistory7d(ctx, assetID)
	}
	return swr(ctx, c, "coin_hist_7d", "GetAssetPriceHistory7d|"+assetID,
		func(ctx context.Context) ([]timescale.AssetPricePoint, error) {
			return c.upstream.GetAssetPriceHistory7d(ctx, assetID)
		})
}

func (c *CachedAssetsReader) GetAssetMarketsCount(ctx context.Context, assetID string) (int64, error) {
	if c.ttl <= 0 {
		return c.upstream.GetAssetMarketsCount(ctx, assetID)
	}
	return swr(ctx, c, "coin_markets_count", "GetAssetMarketsCount|"+assetID,
		func(ctx context.Context) (int64, error) {
			return c.upstream.GetAssetMarketsCount(ctx, assetID)
		})
}

func (c *CachedAssetsReader) GetAssetATH(ctx context.Context, assetID string) (*timescale.AssetATH, error) {
	if c.ttl <= 0 {
		return c.upstream.GetAssetATH(ctx, assetID)
	}
	return swr(ctx, c, "coin_ath", "GetAssetATH|"+assetID,
		func(ctx context.Context) (*timescale.AssetATH, error) {
			return c.upstream.GetAssetATH(ctx, assetID)
		})
}

func (c *CachedAssetsReader) GetAssetTradeCount24h(ctx context.Context, assetID string) (int64, error) {
	if c.ttl <= 0 {
		return c.upstream.GetAssetTradeCount24h(ctx, assetID)
	}
	return swr(ctx, c, "coin_trade_count_24h", "GetAssetTradeCount24h|"+assetID,
		func(ctx context.Context) (int64, error) {
			return c.upstream.GetAssetTradeCount24h(ctx, assetID)
		})
}

// swrEntry backs the generic single-value SWR path. Separate from
// assetsCacheEntry's method-field shape — `val` holds an opaque T;
// its dynamic type is invariant per key because keys are
// method-namespaced (e.g. "GetAssetByAssetID|<id>"), so the
// e.val.(T) assertions never mix types. Guarded by
// CachedAssetsReader.mu (shared with `entries`; distinct map).
// evictOldestSWREntry drops the oldest-filled entry when the map is at
// capacity. Caller must hold the mutex.
//
// In-flight entries (at.IsZero()) are never evicted — dropping one
// would orphan its waiters from the fill about to close their channel.
func evictOldestSWREntry(m map[string]*swrEntry) {
	if len(m) < assetsCacheMaxEntries {
		return
	}
	var (
		oldestKey string
		oldestAt  time.Time
	)
	for k, e := range m {
		if e.at.IsZero() {
			continue
		}
		if oldestKey == "" || e.at.Before(oldestAt) {
			oldestKey, oldestAt = k, e.at
		}
	}
	if oldestKey != "" {
		delete(m, oldestKey)
	}
}

type swrEntry struct {
	at     time.Time
	flight chan struct{}
	val    any
	err    error
}

// swr is the generic single-value stale-while-revalidate fetch: the
// proven, race-clean assetsReader fetchRows logic, made
// type-parametric so every per-asset single-value asset-catalogue method
// shares ONE implementation. Free function — Go methods can't have
// type parameters.
//
//	(A)  fresh hit → return cached
//	(A') expired with a prior success → serve stale IMMEDIATELY +
//	     one single-flighted background refresh (runDetachedFill); never
//	     blocks a request on the slow upstream
//	(B)/(C) cold → join the running fill or start one, then
//	     wait on it OR the caller's ctx; delete-on-error with the
//	     waiter-err-pointer panic-safety
func swr[T any](ctx context.Context, c *CachedAssetsReader, op, key string, upstream func(context.Context) (T, error)) (T, error) {
	var zero T
	c.mu.Lock()
	e, ok := c.swrEntries[key]

	if ok && e.flight == nil && time.Since(e.at) < c.ttl {
		v := e.val
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "hit").Inc()
		return v.(T), nil
	}
	if ok && !e.at.IsZero() {
		v := e.val
		if e.flight == nil {
			done := make(chan struct{})
			e.flight = done
			entry := e
			c.mu.Unlock()
			obs.APICacheOpsTotal.WithLabelValues("coins", op, "stale").Inc()
			//nolint:gosec,contextcheck // G118 / contextcheck:
			// intentional. The SWR background refresh MUST use a
			// fresh context (runDetachedFill), NOT
			// the request ctx, which is cancelled the instant the
			// stale response is written; reusing it would abort
			// every refresh — defeating the entire point of SWR.
			go runDetachedFill(c.logger, "api-assets-catalogue-swr-refresh", assetsRefreshBudget, done, upstream, settleSWR[T](c, op, key, entry))
			return v.(T), nil
		}
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "stale").Inc()
		return v.(T), nil
	}
	entry, leader := e, false
	if !ok || e.flight == nil {
		entry, leader = &swrEntry{flight: make(chan struct{})}, true
		evictOldestSWREntry(c.swrEntries)
		c.swrEntries[key] = entry
		//nolint:gosec,contextcheck // G118 / contextcheck: intentional
		// detached fill — see runDetachedFill.
		go runDetachedFill(c.logger, "api-assets-catalogue-swr-refresh", assetsRefreshBudget, entry.flight, upstream, settleSWR[T](c, op, key, entry))
	}
	ch := entry.flight
	c.mu.Unlock()
	if leader {
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "miss").Inc()
	}
	select {
	case <-ch:
		c.mu.Lock()
		v, err := entry.val, entry.err
		c.mu.Unlock()
		if err != nil {
			if !leader {
				obs.APICacheOpsTotal.WithLabelValues("coins", op, "miss").Inc()
			}
			return zero, err
		}
		if !leader {
			obs.APICacheOpsTotal.WithLabelValues("coins", op, "hit").Inc()
		}
		return v.(T), nil
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// settleSWR applies an swr fill, cold or (A'). On failure a stale value is
// kept (retry next request); a cold entry hands err to its waiters and is
// dropped so errors are never cached.
func settleSWR[T any](c *CachedAssetsReader, op, key string, entry *swrEntry) func(T, error) {
	return func(v T, err error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		entry.flight = nil
		switch {
		case err == nil:
			entry.at = time.Now()
			entry.val = v
		case !entry.at.IsZero():
			obs.APICacheOpsTotal.WithLabelValues("coins", op, "refresh_error").Inc()
		default:
			entry.err = err
			if c.swrEntries[key] == entry {
				delete(c.swrEntries, key)
			}
		}
	}
}

// settleRowsFillLocked applies a finished fill to an assetsCacheEntry:
// apply the value on success; on failure keep a stale value, or for a
// cold entry hand err to its waiters and drop it. Caller holds c.mu.
func (c *CachedAssetsReader) settleRowsFillLocked(op, key string, entry *assetsCacheEntry, err error, apply func()) {
	entry.flight = nil
	switch {
	case err == nil:
		entry.at = time.Now()
		apply()
	case !entry.at.IsZero():
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "refresh_error").Inc()
	default:
		entry.err = err
		if c.entries[key] == entry {
			delete(c.entries, key)
		}
	}
}

// assetsRefreshBudget bounds a stale-while-revalidate background
// refresh. It runs OFF the request path so a generous budget costs
// users nothing (they're already served the stale value); it just
// has to comfortably exceed the listing aggregate's worst case
// (~seconds, contended) so the refresh actually completes and the
// cache moves forward instead of perpetually re-spawning.
const assetsRefreshBudget = 30 * time.Second

// fetchRows returns just the rows. Kept so callers that do not care
// about observation time are not forced to discard a value at every call
// site.
func (c *CachedAssetsReader) fetchRows(
	ctx context.Context,
	op, key string,
	upstream func(context.Context) ([]timescale.AssetRow, error),
) ([]timescale.AssetRow, error) {
	rows, _, err := c.fetchRowsAt(ctx, op, key, upstream)
	return rows, err
}

// fetchRowsAt is fetchRows plus the OBSERVATION TIME of the rows it
// returns, read under the same lock acquisition as the rows themselves.
//
// The pairing is the point. Sampling `e.at` before calling fetchRows
// and pairing that timestamp with whatever rows come back is a race: a
// refresh landing in between returns FRESH rows stamped with the STALE
// entry's time and `stale=true`. A caller would then publish correct
// data under a wrong `as_of` and a wrong freshness flag, which is the
// same honesty class as serving stale data as fresh, just inverted.
//
// A zero `at` means the rows did not come from a cache entry (cache
// disabled, or a cold fill whose entry was not retained), which callers
// read as "answered live".
func (c *CachedAssetsReader) fetchRowsAt(
	ctx context.Context,
	op, key string,
	upstream func(context.Context) ([]timescale.AssetRow, error),
) ([]timescale.AssetRow, time.Time, error) {
	c.mu.Lock()
	e, ok := c.entries[key]

	// (A) Fresh hit.
	if ok && e.flight == nil && time.Since(e.at) < c.ttl {
		out, outAt := e.rows, e.at
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "hit").Inc()
		return out, outAt, nil
	}

	// (A') Stale-while-revalidate. A prior SUCCESSFUL fetch exists
	// (e.at non-zero — failed cold fetches delete the entry, so a
	// present entry with non-zero at always has servable rows) but
	// it's expired. Serve the stale rows IMMEDIATELY and, if no
	// refresh is already running, kick exactly one in the
	// background. Concurrent callers during the refresh also get
	// stale — nobody ever waits on the upstream call. This is the
	// entire fix for ba0374697: the expiry refetch (~seconds on the
	// listing aggregate) must never land on a user request.
	if ok && !e.at.IsZero() {
		stale, staleAt := e.rows, e.at
		if e.flight == nil {
			done := make(chan struct{})
			e.flight = done
			entry := e
			c.mu.Unlock()
			obs.APICacheOpsTotal.WithLabelValues("coins", op, "stale").Inc()
			//nolint:gosec,contextcheck // G118 / contextcheck:
			// intentional. The SWR background refresh MUST use a
			// fresh context (runDetachedFill), NOT
			// the request ctx: the request ctx is cancelled the
			// instant the stale response is written, so reusing it
			// would abort every refresh — defeating the entire point
			// of serving stale while revalidating.
			go runDetachedFill(c.logger, "api-assets-catalogue-rows-refresh", assetsRefreshBudget, done, upstream, c.settleRows(op, key, entry))
			return stale, staleAt, nil
		}
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "stale").Inc()
		return stale, staleAt, nil
	}

	// (B)/(C) Cold: no prior success to serve. Join the running fill or
	// start one, then wait on it OR this caller's ctx. The fill runs
	// detached (runDetachedFill), so one caller's abort cannot fail the others.
	entry, leader := c.joinOrStartColdLocked(e, ok, key)
	if leader {
		//nolint:gosec,contextcheck // G118 / contextcheck: intentional
		// detached fill — see runDetachedFill.
		go runDetachedFill(c.logger, "api-assets-catalogue-rows-refresh", assetsRefreshBudget, entry.flight, upstream, c.settleRows(op, key, entry))
	}
	ch := entry.flight
	c.mu.Unlock()
	if leader {
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "miss").Inc()
	}
	select {
	case <-ch:
		c.mu.Lock()
		rows, at, err := entry.rows, entry.at, entry.err
		c.mu.Unlock()
		if err != nil {
			if !leader {
				obs.APICacheOpsTotal.WithLabelValues("coins", op, "miss").Inc()
			}
			return nil, time.Time{}, err
		}
		if !leader {
			obs.APICacheOpsTotal.WithLabelValues("coins", op, "hit").Inc()
		}
		return rows, at, nil
	case <-ctx.Done():
		return nil, time.Time{}, ctx.Err()
	}
}

// joinOrStartColdLocked returns the in-flight cold entry for key, or
// installs a fresh one (leader=true) whose fill the caller must start.
// Caller holds c.mu.
func (c *CachedAssetsReader) joinOrStartColdLocked(e *assetsCacheEntry, ok bool, key string) (*assetsCacheEntry, bool) {
	if ok && e.flight != nil {
		return e, false
	}
	entry := &assetsCacheEntry{flight: make(chan struct{})}
	evictOldestAssetsEntry(c.entries)
	c.entries[key] = entry
	return entry, true
}

// settleRows applies a fetchRowsAt fill, cold or stale-while-revalidate
// (A'). The upstream call ran through runDetachedFill, never a request ctx:
// for (A') that ctx dies the instant the stale response is written, and for
// a cold fill one waiter's abort must not fail the rest.
func (c *CachedAssetsReader) settleRows(op, key string, entry *assetsCacheEntry) func([]timescale.AssetRow, error) {
	return func(rows []timescale.AssetRow, err error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.settleRowsFillLocked(op, key, entry, err, func() { entry.rows = rows })
	}
}

// fetchHistoryMap is [CachedAssetsReader.fetchRows] for the batched
// price-history maps. The branch structure is deliberately identical,
// (A)/(A')/(B)/(C) in the same order. Without (A') an EXPIRED history
// batch falls straight through to the blocking cold-leader path and
// every caller during the refetch waits on the slow upstream: the
// stampede-on-expiry the listing rows avoid, on the sibling the same
// handler calls in the same request (/v1/assets?include=sparkline fans
// out to both).
func (c *CachedAssetsReader) fetchHistoryMap(
	ctx context.Context,
	op, key string,
	upstream func(context.Context) (map[string][]timescale.AssetPricePoint, error),
) (map[string][]timescale.AssetPricePoint, error) {
	c.mu.Lock()
	e, ok := c.entries[key]

	// (A) Fresh hit.
	if ok && e.flight == nil && time.Since(e.at) < c.ttl {
		out := e.historyByAsset
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "hit").Inc()
		return out, nil
	}

	// (A') Stale-while-revalidate. A prior SUCCESSFUL fetch exists
	// (e.at non-zero — failed cold fetches delete the entry) but has
	// expired. Serve the stale map IMMEDIATELY and kick at most one
	// background refresh. Nobody waits on the upstream call.
	if ok && !e.at.IsZero() {
		stale := e.historyByAsset
		if e.flight == nil {
			done := make(chan struct{})
			e.flight = done
			entry := e
			c.mu.Unlock()
			obs.APICacheOpsTotal.WithLabelValues("coins", op, "stale").Inc()
			//nolint:gosec,contextcheck // G118 / contextcheck:
			// intentional, same as fetchRows — the SWR refresh MUST
			// use a fresh context (runDetachedFill), NOT
			// the request ctx, which is
			// cancelled the instant the stale response is written.
			go runDetachedFill(c.logger, "api-assets-catalogue-history-refresh", assetsRefreshBudget, done, upstream, c.settleHistoryMap(op, key, entry))
			return stale, nil
		}
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "stale").Inc()
		return stale, nil
	}

	// (B)/(C) Cold: join or start a detached fill — see fetchRowsAt.
	entry, leader := c.joinOrStartColdLocked(e, ok, key)
	if leader {
		//nolint:gosec,contextcheck // G118 / contextcheck: intentional
		// detached fill — see runDetachedFill.
		go runDetachedFill(c.logger, "api-assets-catalogue-history-refresh", assetsRefreshBudget, entry.flight, upstream, c.settleHistoryMap(op, key, entry))
	}
	ch := entry.flight
	c.mu.Unlock()
	if leader {
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "miss").Inc()
	}
	select {
	case <-ch:
		c.mu.Lock()
		hist, err := entry.historyByAsset, entry.err
		c.mu.Unlock()
		if err != nil {
			if !leader {
				obs.APICacheOpsTotal.WithLabelValues("coins", op, "miss").Inc()
			}
			return nil, err
		}
		if !leader {
			obs.APICacheOpsTotal.WithLabelValues("coins", op, "hit").Inc()
		}
		return hist, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// settleHistoryMap is [CachedAssetsReader.settleRows] for the history-map
// entries.
func (c *CachedAssetsReader) settleHistoryMap(op, key string, entry *assetsCacheEntry) func(map[string][]timescale.AssetPricePoint, error) {
	return func(hist map[string][]timescale.AssetPricePoint, err error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.settleRowsFillLocked(op, key, entry, err, func() { entry.historyByAsset = hist })
	}
}
