package v1

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
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
	// single backs the per-asset single-row methods through swr[T]; each key holds one T.
	single *swrCache[any]
	// series backs the history batches per (op, asset), so a page whose asset set shifts reads only the assets new to it.
	series     map[string]*seriesEntry
	seriesFill singleflight.Group
}

// assetsCacheMaxEntries bounds the entry map: keys carry client-controlled parts (cursor, free-text q), so without eviction one
// anonymous caller could grow it to MemoryMax. Far above the real working set.
const assetsCacheMaxEntries = 4096

// evictOldestAssetsEntry drops the oldest-filled entry at capacity; caller holds the mutex. In-flight entries (at.IsZero()) are
// never evicted, which would orphan their waiters.
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

	rows []timescale.AssetRow

	// err is set by the leader before close(flight) on a failing upstream call. Waiters hold
	// the SAME entry they joined, so they can read err even after the leader deletes it from
	// the map (errors are not cached); re-looking it up by key would nil-deref and panic.
	err error
}

// NewCachedAssetsReader wraps upstream with a TTL cache (ttl=0 disables); the 2-minute production TTL is set in cmd/stellarindex-api/main.go.
func NewCachedAssetsReader(upstream AssetsReader, ttl time.Duration) *CachedAssetsReader {
	return &CachedAssetsReader{
		logger:   slog.Default(),
		upstream: upstream,
		ttl:      ttl,
		entries:  map[string]*assetsCacheEntry{},
		single:   newSWRCache[any](slog.Default(), "coins", "api-assets-catalogue-swr-refresh", ttl, assetsCacheMaxEntries),
		series:   map[string]*seriesEntry{},
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

// GetAssetsPriceHistory24hBatch is cached per asset (see [CachedAssetsReader.historyBatch]).
func (c *CachedAssetsReader) GetAssetsPriceHistory24hBatch(ctx context.Context, assetIDs []string) (map[string][]timescale.AssetPricePoint, error) {
	if c.ttl <= 0 {
		return c.upstream.GetAssetsPriceHistory24hBatch(ctx, assetIDs)
	}
	return c.historyBatch(ctx, "price_history_24h", assetIDs, c.upstream.GetAssetsPriceHistory24hBatch)
}

// GetAssetsPriceHistory7dBatch is cached per asset (see [CachedAssetsReader.historyBatch]).
func (c *CachedAssetsReader) GetAssetsPriceHistory7dBatch(ctx context.Context, assetIDs []string) (map[string][]timescale.AssetPricePoint, error) {
	if c.ttl <= 0 {
		return c.upstream.GetAssetsPriceHistory7dBatch(ctx, assetIDs)
	}
	return c.historyBatch(ctx, "price_history_7d", assetIDs, c.upstream.GetAssetsPriceHistory7dBatch)
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

// swr is the per-asset single-value read through c.single (a free function: methods cannot take type parameters).
func swr[T any](ctx context.Context, c *CachedAssetsReader, op, key string, upstream func(context.Context) (T, error)) (T, error) {
	v, _, _, err := c.single.get(ctx, op, key, func(ctx context.Context) (any, error) { return upstream(ctx) })
	if err != nil {
		var zero T
		return zero, err
	}
	return v.(T), nil
}

// settleRowsFillLocked applies a finished fill: on failure keep a stale value, or hand err to a cold entry's waiters and drop
// it. Caller holds c.mu.
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

// assetsRefreshBudget bounds a background refresh; it must comfortably exceed the listing aggregate's contended worst case so
// the cache advances instead of perpetually re-spawning.
const assetsRefreshBudget = 30 * time.Second

// fetchRows returns just the rows, for callers that do not need observation time.
func (c *CachedAssetsReader) fetchRows(
	ctx context.Context,
	op, key string,
	upstream func(context.Context) ([]timescale.AssetRow, error),
) ([]timescale.AssetRow, error) {
	rows, _, err := c.fetchRowsAt(ctx, op, key, upstream)
	return rows, err
}

// fetchRowsAt is fetchRows plus the rows' OBSERVATION TIME, read under the same lock: sampling `e.at` separately races a
// refresh and would publish fresh rows under a stale `as_of`. A zero `at` means "answered live" (no cache entry).
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

// joinOrStartColdLocked returns the in-flight cold entry for key or installs one (leader=true) whose fill the caller must
// start. Caller holds c.mu.
func (c *CachedAssetsReader) joinOrStartColdLocked(e *assetsCacheEntry, ok bool, key string) (*assetsCacheEntry, bool) {
	if ok && e.flight != nil {
		return e, false
	}
	entry := &assetsCacheEntry{flight: make(chan struct{})}
	evictOldestAssetsEntry(c.entries)
	c.entries[key] = entry
	return entry, true
}

// settleRows applies a fetchRowsAt fill. The upstream call runs via runDetachedFill, never a request ctx: it dies when the
// stale response is written, and one waiter's abort must not fail the rest.
func (c *CachedAssetsReader) settleRows(op, key string, entry *assetsCacheEntry) func([]timescale.AssetRow, error) {
	return func(rows []timescale.AssetRow, err error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.settleRowsFillLocked(op, key, entry, err, func() { entry.rows = rows })
	}
}

// seriesMaxEntries bounds the per-asset series map; ids come from listing pages, so this sits far above the asset universe.
const seriesMaxEntries = 16384

type seriesEntry struct {
	at         time.Time
	pts        []timescale.AssetPricePoint
	found      bool // the upstream map had the asset; an absent asset is cached as absent
	refreshing bool
}

type historyBatchFunc func(context.Context, []string) (map[string][]timescale.AssetPricePoint, error)

// historyBatch serves each asset from its own slot: fresh and stale slots answer at once (stale ones refresh in one detached
// batch), and only assets with no slot are read before returning, single-flighted on that set. Keying the whole set instead
// made every change in a volume-ranked page a cold read of all of it.
func (c *CachedAssetsReader) historyBatch(ctx context.Context, op string, ids []string, upstream historyBatchFunc) (map[string][]timescale.AssetPricePoint, error) {
	out, missing, stale := c.seriesSlots(op, ids)

	switch {
	case len(missing) > 0:
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "miss").Inc()
	case len(stale) > 0:
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "stale").Inc()
	default:
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "hit").Inc()
	}
	if len(stale) > 0 {
		//nolint:contextcheck // the refresh outlives the request that found the slots stale.
		go func() {
			defer worker.Recover(c.logger, "api-assets-series-refresh")
			_, _ = c.fillSeries(op, stale, upstream)
		}()
	}
	if len(missing) == 0 {
		return out, nil
	}
	sort.Strings(missing)
	//nolint:contextcheck // detached fill: one waiter's abort must not fail the others sharing it.
	ch := c.seriesFill.DoChan(op+"|"+strings.Join(missing, ","), func() (any, error) {
		return c.fillSeries(op, missing, upstream)
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		got, _ := r.Val.(map[string][]timescale.AssetPricePoint)
		for _, id := range missing {
			if pts, ok := got[id]; ok {
				out[id] = pts
			}
		}
		return out, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// seriesSlots answers ids from their slots and names the ids with no slot and the stale ones, marking those refreshing.
func (c *CachedAssetsReader) seriesSlots(op string, ids []string) (out map[string][]timescale.AssetPricePoint, missing, stale []string) {
	out = make(map[string][]timescale.AssetPricePoint, len(ids))
	seen := make(map[string]struct{}, len(ids))
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		e, ok := c.series[op+"|"+id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		if e.found {
			out[id] = e.pts
		}
		if time.Since(e.at) >= c.ttl && !e.refreshing {
			e.refreshing = true
			stale = append(stale, id)
		}
	}
	return out, missing, stale
}

// fillSeries reads ids under a detached budget and stores one slot per id; on error it keeps any stale slots and clears their
// refresh mark so the next request retries.
func (c *CachedAssetsReader) fillSeries(op string, ids []string, upstream historyBatchFunc) (map[string][]timescale.AssetPricePoint, error) {
	var (
		got map[string][]timescale.AssetPricePoint
		err error
	)
	runDetachedFill(c.logger, "api-assets-catalogue-history-refresh", assetsRefreshBudget, make(chan struct{}),
		func(ctx context.Context) (map[string][]timescale.AssetPricePoint, error) { return upstream(ctx, ids) },
		func(v map[string][]timescale.AssetPricePoint, e error) { got, err = v, e })
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for _, id := range ids {
		key := op + "|" + id
		if err != nil {
			if e, ok := c.series[key]; ok {
				e.refreshing = false
			}
			continue
		}
		if _, ok := c.series[key]; !ok {
			evictOldestSeriesEntry(c.series)
		}
		pts, found := got[id]
		c.series[key] = &seriesEntry{at: now, pts: pts, found: found}
	}
	return got, err
}

// evictOldestSeriesEntry drops the oldest slot at capacity; caller holds the mutex.
func evictOldestSeriesEntry(m map[string]*seriesEntry) {
	if len(m) < seriesMaxEntries {
		return
	}
	var (
		oldestKey string
		oldestAt  time.Time
	)
	for k, e := range m {
		if oldestKey == "" || e.at.Before(oldestAt) {
			oldestKey, oldestAt = k, e.at
		}
	}
	delete(m, oldestKey)
}
