package v1

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// CachedAssetsReader wraps [AssetsReader] with a per-key TTL cache and single-flight for the slow listing and price-history calls; other methods pass through.
type CachedAssetsReader struct {
	// logger sinks a panic recovered in a detached refresh goroutine. It is the process
	// default because the reader wrappers are built before the API Server exists; the
	// load-bearing signal is stellarindex_worker_panics_total, which worker.Report moves
	// regardless of the sink.
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
// Cache keys carry client-controlled components (`cursor` is validated for shape only, plus
// free-text `q` and asset ids). Without eviction one anonymous caller could mint a permanent
// full-page entry per distinct cursor and walk the process to its MemoryMax. The cap sits far
// above the real working set, which is a handful of prewarmed shapes and bounded pagination.
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

	// err is set by the leader before close(flight) on a failing upstream call. Waiters hold
	// the SAME entry they joined, so they can read err even after the leader deletes it from
	// the map (errors are not cached); re-looking it up by key would nil-deref and panic.
	err error
}

// NewCachedAssetsReader wraps upstream with a TTL cache (ttl=0 disables); the 2-minute production TTL is set in cmd/stellarindex-api/main.go.
func NewCachedAssetsReader(upstream AssetsReader, ttl time.Duration) *CachedAssetsReader {
	return &CachedAssetsReader{
		logger:     slog.Default(),
		upstream:   upstream,
		ttl:        ttl,
		entries:    map[string]*assetsCacheEntry{},
		swrEntries: map[string]*swrEntry{},
	}
}

// LatestSupplyObservations passes through uncached to the upstream supply reader so the handler's type-assert resolves through this wrapper.
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
	// Every ListAssetsOptions dimension that changes the result set MUST appear in the key,
	// or two requests differing only by that dimension would serve each other's rows. That
	// includes Code and Type (classic vs traded Soroban-native), alongside Issuer/Cursor/Q.
	key := newCacheKey("ListAssetsExt").
		int(opts.Limit).str(opts.Issuer).str(opts.Code).str(opts.Type).
		str(opts.Cursor).str(opts.Q).order(int(opts.Order)).build()
	return c.fetchRows(ctx, "list_coins", key, func(ctx context.Context) ([]timescale.AssetRow, error) {
		return c.upstream.ListAssetsExt(ctx, opts)
	})
}

// GetAssetsPriceHistory24hBatch is cached on the order-normalised asset-id set, so the same set in any order shares one slot.
func (c *CachedAssetsReader) GetAssetsPriceHistory24hBatch(ctx context.Context, assetIDs []string) (map[string][]timescale.AssetPricePoint, error) {
	if c.ttl <= 0 {
		return c.upstream.GetAssetsPriceHistory24hBatch(ctx, assetIDs)
	}
	key := newCacheKey("Price24hBatch").strSet(assetIDs).build()
	return c.fetchHistoryMap(ctx, "price_history_24h", key, func(ctx context.Context) (map[string][]timescale.AssetPricePoint, error) {
		return c.upstream.GetAssetsPriceHistory24hBatch(ctx, assetIDs)
	})
}

// GetAssetsPriceHistory7dBatch is cached with the same order-normalised keying as the 24h batch.
func (c *CachedAssetsReader) GetAssetsPriceHistory7dBatch(ctx context.Context, assetIDs []string) (map[string][]timescale.AssetPricePoint, error) {
	if c.ttl <= 0 {
		return c.upstream.GetAssetsPriceHistory7dBatch(ctx, assetIDs)
	}
	key := newCacheKey("Price7dBatch").strSet(assetIDs).build()
	return c.fetchHistoryMap(ctx, "price_history_7d", key, func(ctx context.Context) (map[string][]timescale.AssetPricePoint, error) {
		return c.upstream.GetAssetsPriceHistory7dBatch(ctx, assetIDs)
	})
}

// GetAssetsATHBatch passes through uncached: no hot-path handler serves it.
func (c *CachedAssetsReader) GetAssetsATHBatch(ctx context.Context, assetIDs []string) (map[string]timescale.AssetATH, error) {
	return c.upstream.GetAssetsATHBatch(ctx, assetIDs)
}

// Per-asset single-value reads are stale-while-revalidate cached via the generic swr[T] helper.
// They are not cheap single-row lookups: /v1/assets/{id} fans out ~9 of them per request,
// GetAssetByAssetID and GetNativeAssetRow run the whole-asset-universe listAssetsBaseSelect
// query, and the trade-count and markets-count reads scan trades. SWR moves that off the
// request path (stale served instantly, one single-flighted background refresh).

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

// swrEntry backs the generic single-value SWR path. `val` holds an opaque T whose dynamic type
// is invariant per key because keys are method-namespaced (e.g. "GetAssetByAssetID|<id>"), so
// the e.val.(T) assertions never mix types. Guarded by CachedAssetsReader.mu.
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

// swr is the generic single-value stale-while-revalidate fetch: the fetchRows logic made
// type-parametric so every per-asset method shares ONE implementation. It is a free function
// because Go methods cannot have type parameters.
//
//	(A)  fresh hit → return cached
//	(A') expired with a prior success → serve stale IMMEDIATELY + one single-flighted
//	     background refresh (runDetachedFill); never blocks a request on the slow upstream
//	(B)/(C) cold → join the running fill or start one, then wait on it OR the caller's ctx;
//	     delete-on-error, with waiters holding the entry pointer so they read its err safely
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

// assetsRefreshBudget bounds a stale-while-revalidate background refresh. It runs off the
// request path, so a generous budget costs users nothing; it must comfortably exceed the
// listing aggregate's contended worst case so the refresh completes and the cache advances
// instead of perpetually re-spawning.
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
// The pairing is the point: sampling `e.at` before fetchRows races with a refresh, which would
// publish fresh rows under a stale `as_of` and `stale=true`. A zero `at` means the rows did not
// come from a cache entry (cache disabled, or a cold fill not retained); callers read it as
// "answered live".
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

	// (A') Stale-while-revalidate. A prior SUCCESSFUL fetch exists (failed cold fetches delete
	// the entry, so non-zero e.at always means servable rows) but it has expired. Serve the
	// stale rows IMMEDIATELY and kick at most one background refresh; concurrent callers also
	// get stale, so the slow expiry refetch never lands on a user request.
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

// fetchHistoryMap is [CachedAssetsReader.fetchRows] for the batched price-history maps, with
// the same (A)/(A')/(B)/(C) branch order. Without (A') an expired history batch would block
// every caller on the slow upstream, a stampede-on-expiry that the listing rows avoid and that
// /v1/assets?include=sparkline would hit on its sibling call in the same request.
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
