package v1

import (
	"context"
	"sync"
	"time"

	"github.com/StellarIndex/stellar-index/internal/obs"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// CachedAssetsReader wraps a [AssetsReader] with a small per-key TTL
// cache for the methods that back high-traffic listing endpoints —
// ListAssetsExt and the batched price-history calls used by
// /v1/assets?limit=200&include=sparkline. The legacy /v1/assetsReader
// route was removed in rc.48; /v1/assets now sources the same data
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
// disables the cache (every call passes through). 30s is the
// production default — listings are activity-ranked aggregates
// that don't move materially in 30s, and the explorer's existing
// react-query layer caches client-side anyway.
func NewCachedAssetsReader(upstream AssetsReader, ttl time.Duration) *CachedAssetsReader {
	return &CachedAssetsReader{
		upstream:   upstream,
		ttl:        ttl,
		entries:    map[string]*assetsCacheEntry{},
		swrEntries: map[string]*swrEntry{},
	}
}

// LatestCirculatingSupply passes through to the upstream's supply
// reader (used by the /v1/assets market_cap enrichment). Not part of
// AssetsReader — exposed so the handler's type-assert resolves through
// this wrapper instead of skipping enrichment. Uncached: the underlying
// supply_1d lookup is a handful of rows.
func (c *CachedAssetsReader) LatestCirculatingSupply(ctx context.Context) (map[string]string, error) {
	if sr, ok := c.upstream.(interface {
		LatestCirculatingSupply(context.Context) (map[string]string, error)
	}); ok {
		return sr.LatestCirculatingSupply(ctx)
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
	// row-narrowing filter (BACKLOG #54), so it is keyed alongside
	// Issuer/Cursor/Q. (Type is NOT here: the handler folds it before
	// reaching the reader — classic_assets is homogeneously classic,
	// so a non-classic type short-circuits to an empty page and a
	// classic/any type is a no-op on this call.)
	key := newCacheKey("ListAssetsExt").
		int(opts.Limit).str(opts.Issuer).str(opts.Code).
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

// GetAssetsATHBatch — cached. ATH can change on every new high but
// per-request cost is the same as the histories so 30s freshness
// is plenty.
func (c *CachedAssetsReader) GetAssetsATHBatch(ctx context.Context, assetIDs []string) (map[string]timescale.AssetATH, error) {
	if c.ttl <= 0 {
		return c.upstream.GetAssetsATHBatch(ctx, assetIDs)
	}
	// Pass-through for now — the upstream is fast enough that the
	// cost / complexity tradeoff doesn't yet justify a third
	// fetcher type. Plumbed here so an operator can flip it on
	// without changing the interface.
	return c.upstream.GetAssetsATHBatch(ctx, assetIDs)
}

// Per-asset single-value reads — all stale-while-revalidate cached
// via the generic swr[T] helper. These were pass-through on the
// assumption they were "<50ms low-volume single-row" lookups; that
// became false post-backfill: /v1/assets/{id}'s asset-catalogue extension
// fans out ~9 of these per request and GetAssetByAssetID /
// GetNativeAssetRow run the whole-asset-universe listAssetsBaseSelect
// query (~13s under load), with GetAssetTradeCount24h /
// GetAssetMarketsCount adding multi-second trades-OR scans (#24).
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
type swrEntry struct {
	at     time.Time
	flight chan struct{}
	val    any
	err    error
}

// swr is the generic single-value stale-while-revalidate fetch: the
// proven, race-clean assetsReader fetchRows/refreshRows logic (#22), made
// type-parametric so every per-asset single-value asset-catalogue method
// shares ONE implementation. Free function — Go methods can't have
// type parameters.
//
//	(A)  fresh hit → return cached
//	(A') expired with a prior success → serve stale IMMEDIATELY +
//	     one single-flighted background refresh (refreshSWR); never
//	     blocks a request on the slow upstream
//	(B)  cold fetch already in flight → join it
//	(C)  cold leader → block inline; delete-on-error with the
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
			// fresh context (refreshSWR -> context.Background), NOT
			// the request ctx, which is cancelled the instant the
			// stale response is written; reusing it would abort
			// every refresh — defeating the entire point of SWR.
			go refreshSWR(c, op, entry, done, upstream)
			return v.(T), nil
		}
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "stale").Inc()
		return v.(T), nil
	}
	if ok && e.flight != nil {
		entry := e
		ch := e.flight
		c.mu.Unlock()
		select {
		case <-ch:
			if entry.err != nil {
				obs.APICacheOpsTotal.WithLabelValues("coins", op, "miss").Inc()
				return zero, entry.err
			}
			obs.APICacheOpsTotal.WithLabelValues("coins", op, "hit").Inc()
			return entry.val.(T), nil
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}

	done := make(chan struct{})
	entry := &swrEntry{flight: done}
	c.swrEntries[key] = entry
	c.mu.Unlock()
	obs.APICacheOpsTotal.WithLabelValues("coins", op, "miss").Inc()

	v, err := upstream(ctx)

	c.mu.Lock()
	if err == nil {
		entry.at = time.Now()
		entry.val = v
		entry.flight = nil
	} else {
		entry.err = err
		delete(c.swrEntries, key) // don't cache the error
	}
	c.mu.Unlock()
	close(done)
	return v, err
}

// refreshSWR runs the upstream call OFF the request path for swr's
// (A') branch — fresh background context (the request ctx dies when
// the stale response is written); on success swaps val+at under the
// lock; on failure keeps the stale value and only clears the
// in-flight marker (retry next request); single-flighted via
// done/entry.flight. Mirrors asset_catalogue_cache.go refreshRows.
func refreshSWR[T any](c *CachedAssetsReader, op string, entry *swrEntry, done chan struct{}, upstream func(context.Context) (T, error)) {
	defer close(done)
	ctx, cancel := context.WithTimeout(context.Background(), assetsRefreshBudget)
	defer cancel()

	v, err := upstream(ctx)

	c.mu.Lock()
	if err == nil {
		entry.at = time.Now()
		entry.val = v
	}
	entry.flight = nil
	c.mu.Unlock()

	if err != nil {
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "refresh_error").Inc()
	}
}

// assetsRefreshBudget bounds a stale-while-revalidate background
// refresh. It runs OFF the request path so a generous budget costs
// users nothing (they're already served the stale value); it just
// has to comfortably exceed the listing aggregate's worst case
// (~seconds, contended) so the refresh actually completes and the
// cache moves forward instead of perpetually re-spawning.
const assetsRefreshBudget = 30 * time.Second

func (c *CachedAssetsReader) fetchRows(
	ctx context.Context,
	op, key string,
	upstream func(context.Context) ([]timescale.AssetRow, error),
) ([]timescale.AssetRow, error) {
	c.mu.Lock()
	e, ok := c.entries[key]

	// (A) Fresh hit.
	if ok && e.flight == nil && time.Since(e.at) < c.ttl {
		out := e.rows
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "hit").Inc()
		return out, nil
	}

	// (A') Stale-while-revalidate. A prior SUCCESSFUL fetch exists
	// (e.at non-zero — failed cold fetches delete the entry, so a
	// present entry with non-zero at always has servable rows) but
	// it's expired. Serve the stale rows IMMEDIATELY and, if no
	// refresh is already running, kick exactly one in the
	// background. Concurrent callers during the refresh also get
	// stale — nobody ever waits on the upstream call. This is the
	// entire fix for #22: the expiry refetch (~seconds on the
	// listing aggregate) must never land on a user request.
	if ok && !e.at.IsZero() {
		stale := e.rows
		if e.flight == nil {
			done := make(chan struct{})
			e.flight = done
			entry := e
			c.mu.Unlock()
			obs.APICacheOpsTotal.WithLabelValues("coins", op, "stale").Inc()
			//nolint:gosec,contextcheck // G118 / contextcheck:
			// intentional. The SWR background refresh MUST use a
			// fresh context (refreshRows -> context.Background), NOT
			// the request ctx: the request ctx is cancelled the
			// instant the stale response is written, so reusing it
			// would abort every refresh — defeating the entire point
			// of serving stale while revalidating.
			go c.refreshRows(op, entry, done, upstream)
			return stale, nil
		}
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "stale").Inc()
		return stale, nil
	}

	// (B) Cold fetch already in flight (no prior success to serve) —
	// join it rather than stampede upstream.
	if ok && e.flight != nil {
		entry := e
		ch := e.flight
		c.mu.Unlock()
		select {
		case <-ch:
			if entry.err != nil {
				obs.APICacheOpsTotal.WithLabelValues("coins", op, "miss").Inc()
				return nil, entry.err
			}
			obs.APICacheOpsTotal.WithLabelValues("coins", op, "hit").Inc()
			return entry.rows, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// (C) Cold leader: no entry (or a prior failed cold fetch left
	// none). Block inline — there is nothing stale to serve.
	done := make(chan struct{})
	entry := &assetsCacheEntry{flight: done}
	c.entries[key] = entry
	c.mu.Unlock()
	obs.APICacheOpsTotal.WithLabelValues("coins", op, "miss").Inc()

	rows, err := upstream(ctx)

	c.mu.Lock()
	if err == nil {
		entry.at = time.Now()
		entry.rows = rows
		entry.flight = nil
	} else {
		entry.err = err
		delete(c.entries, key) // don't cache the error for new callers
	}
	c.mu.Unlock()
	close(done)
	return rows, err
}

// refreshRows runs the upstream call OFF the request path for the
// stale-while-revalidate (A') branch of fetchRows.
//
//   - It uses a fresh background context, NOT the triggering
//     request's ctx: that ctx is cancelled the instant the stale
//     response is written, which would abort every refresh.
//   - On success it swaps the entry's rows + timestamp under the
//     lock → subsequent callers get a fresh hit.
//   - On failure it KEEPS the existing stale value (does not delete,
//     does not touch e.at) so we keep serving stale and simply
//     retry on the next request; only the in-flight marker clears.
//   - Single-flighted via `done`/`entry.flight`: while it runs,
//     fetchRows' (A') sees e.flight != nil and serves stale without
//     spawning a second refresh. Nothing waits on `done` (the SWR
//     path never blocks), but closing it is harmless and keeps the
//     channel lifecycle symmetric with the cold path.
func (c *CachedAssetsReader) refreshRows(
	op string,
	entry *assetsCacheEntry,
	done chan struct{},
	upstream func(context.Context) ([]timescale.AssetRow, error),
) {
	defer close(done)
	ctx, cancel := context.WithTimeout(context.Background(), assetsRefreshBudget)
	defer cancel()

	rows, err := upstream(ctx)

	c.mu.Lock()
	if err == nil {
		entry.at = time.Now()
		entry.rows = rows
	}
	entry.flight = nil
	c.mu.Unlock()

	if err != nil {
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "refresh_error").Inc()
	}
}

func (c *CachedAssetsReader) fetchHistoryMap(
	ctx context.Context,
	op, key string,
	upstream func(context.Context) (map[string][]timescale.AssetPricePoint, error),
) (map[string][]timescale.AssetPricePoint, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && e.flight == nil && time.Since(e.at) < c.ttl {
		out := e.historyByAsset
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("coins", op, "hit").Inc()
		return out, nil
	}
	if e, ok := c.entries[key]; ok && e.flight != nil {
		entry := e
		ch := e.flight
		c.mu.Unlock()
		select {
		case <-ch:
			if entry.err != nil {
				obs.APICacheOpsTotal.WithLabelValues("coins", op, "miss").Inc()
				return nil, entry.err
			}
			obs.APICacheOpsTotal.WithLabelValues("coins", op, "hit").Inc()
			return entry.historyByAsset, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	done := make(chan struct{})
	entry := &assetsCacheEntry{flight: done}
	c.entries[key] = entry
	c.mu.Unlock()
	obs.APICacheOpsTotal.WithLabelValues("coins", op, "miss").Inc()

	hist, err := upstream(ctx)

	c.mu.Lock()
	if err == nil {
		entry.at = time.Now()
		entry.historyByAsset = hist
		entry.flight = nil
	} else {
		entry.err = err
		delete(c.entries, key)
	}
	c.mu.Unlock()
	close(done)
	return hist, err
}
