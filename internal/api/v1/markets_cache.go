package v1

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// CachedMarketsReader wraps [MarketsReader] with a per-key TTL cache and single-flight; served row slices are one-level COPIES, so callers may REPLACE a field but never write THROUGH a pointer or slice field.
type CachedMarketsReader struct {
	// logger sinks a panic recovered in a detached refresh goroutine. It is the process default
	// because the reader wrappers are built before the API Server exists; the load-bearing signal
	// is stellarindex_worker_panics_total, which worker.Report moves regardless of the sink.
	logger *slog.Logger

	upstream MarketsReader
	ttl      time.Duration

	mu      sync.Mutex
	entries map[string]*marketsCacheEntry

	// sparks caches the per-listing sparkline batch, which adds ~200 ms to a /v1/markets page on r1.
	sparks *swrCache[map[string][]timescale.PairVolumePoint]
}

// marketsCacheMaxEntries bounds the map: keys carry client-controlled parts (cursor, q, asset ids), so uncapped one caller could exhaust
// memory.
const marketsCacheMaxEntries = 4096

// evictOldestMarketsEntry drops the oldest-filled entry at capacity; caller holds the mutex. In-flight entries are never evicted (waiters
// orphaned).
func evictOldestMarketsEntry(m map[string]*marketsCacheEntry) {
	if len(m) < marketsCacheMaxEntries {
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

type marketsCacheEntry struct {
	at     time.Time
	flight chan struct{}

	pairs  []Market
	pools  []Pool
	cursor string

	// err is set by the leader before close(flight) on a failing upstream call. Waiters hold the
	// SAME entry they joined, so even after the leader removes it from the map (errors are not
	// cached) they read entry.err instead of nil-dereferencing a missing entry.
	err error
}

// NewCachedMarketsReader wraps upstream with a TTL cache (ttl=0 disables); the 30s production default is tighter than sources_stats's 60s because /v1/markets is the front page.
func NewCachedMarketsReader(upstream MarketsReader, ttl time.Duration) *CachedMarketsReader {
	return &CachedMarketsReader{
		logger:   slog.Default(),
		upstream: upstream,
		ttl:      ttl,
		entries:  map[string]*marketsCacheEntry{},
		sparks:   newSWRCache[map[string][]timescale.PairVolumePoint](nil, "markets", "api-markets-sparkline-fill", ttl, marketsSparklineMaxEntries),
	}
}

// marketsSparklineMaxEntries bounds the sparkline cache; keys are the pair sets of served listing pages.
const marketsSparklineMaxEntries = 128

// PairMarket passes through uncached.
func (c *CachedMarketsReader) PairMarket(ctx context.Context, base, quote canonical.Asset) (Market, bool, error) {
	return c.upstream.PairMarket(ctx, base, quote)
}

// GetPairsVolumeHistory24hBatch is GetPairsVolumeHistory24hBatchAt without the staleness bit.
func (c *CachedMarketsReader) GetPairsVolumeHistory24hBatch(ctx context.Context, pairs [][2]string) (map[string][]timescale.PairVolumePoint, error) {
	hist, _, err := c.GetPairsVolumeHistory24hBatchAt(ctx, pairs)
	return hist, err
}

// GetPairsVolumeHistory24hBatchAt caches one batch per pair set and reports whether the served batch is past its TTL. The
// returned map is shared: callers read it, never write it.
func (c *CachedMarketsReader) GetPairsVolumeHistory24hBatchAt(ctx context.Context, pairs [][2]string) (map[string][]timescale.PairVolumePoint, bool, error) {
	if c.ttl <= 0 {
		hist, err := c.upstream.GetPairsVolumeHistory24hBatch(ctx, pairs)
		return hist, false, err
	}
	keys := make([]string, len(pairs))
	for i, p := range pairs {
		keys[i] = p[0] + "|" + p[1]
	}
	sort.Strings(keys)
	hist, _, stale, err := c.sparks.get(ctx, "pairs_volume_24h", strings.Join(keys, ","), func(ctx context.Context) (map[string][]timescale.PairVolumePoint, error) {
		return c.upstream.GetPairsVolumeHistory24hBatch(ctx, pairs)
	})
	return hist, stale, err
}

// FirstTradeBatch passes through uncached: inception is immutable and the call is opt-in via ?include=inception.
func (c *CachedMarketsReader) FirstTradeBatch(ctx context.Context, pairs [][2]string) (map[string]time.Time, error) {
	return c.upstream.FirstTradeBatch(ctx, pairs)
}

// DistinctPairsExt is cached in the plain [MarketsReader] shape, dropping the staleness meta DistinctPairsExtAt surfaces.
func (c *CachedMarketsReader) DistinctPairsExt(ctx context.Context, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, error) {
	rows, next, _, _, err := c.DistinctPairsExtAt(ctx, cursor, limit, order)
	return rows, next, err
}

// DistinctPairsExtAt is DistinctPairsExt plus the rows' observed-at time and staleness so /v1/markets stamps an honest as_of; observedAt is zero when the cache is disabled.
func (c *CachedMarketsReader) DistinctPairsExtAt(ctx context.Context, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, time.Time, bool, error) {
	if c.ttl <= 0 {
		rows, next, err := c.upstream.DistinctPairsExt(ctx, cursor, limit, order)
		return rows, next, time.Time{}, false, err
	}
	fetch := headLimit(cursor, limit)
	key := newCacheKey("DistinctPairsExt").str(cursor).int(fetch).order(int(order)).build()
	rows, next, at, stale, err := c.fetchPairs(ctx, "distinct_pairs", key, func(ctx context.Context) ([]Market, string, error) {
		return c.upstream.DistinctPairsExt(ctx, cursor, fetch, order)
	})
	rows, next = sliceHead(rows, next, limit, func(m Market) string {
		return timescale.PageCursor(order, m.Base+"|"+m.Quote, m.Volume24hUSD)
	})
	return rows, next, at, stale, err
}

// SourceMarkets — cached. See DistinctPairsExt re: dropped staleness meta.
func (c *CachedMarketsReader) SourceMarkets(ctx context.Context, source, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, error) {
	rows, next, _, _, err := c.SourceMarketsAt(ctx, source, cursor, limit, order)
	return rows, next, err
}

// SourceMarketsAt is SourceMarkets plus observed-at and staleness meta; see DistinctPairsExtAt.
func (c *CachedMarketsReader) SourceMarketsAt(ctx context.Context, source, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, time.Time, bool, error) {
	if c.ttl <= 0 {
		rows, next, err := c.upstream.SourceMarkets(ctx, source, cursor, limit, order)
		return rows, next, time.Time{}, false, err
	}
	key := newCacheKey("SourceMarkets").str(source).str(cursor).int(limit).order(int(order)).build()
	return c.fetchPairs(ctx, "source_markets", key, func(ctx context.Context) ([]Market, string, error) {
		return c.upstream.SourceMarkets(ctx, source, cursor, limit, order)
	})
}

// AssetMarkets — cached. See DistinctPairsExt re: dropped staleness meta.
func (c *CachedMarketsReader) AssetMarkets(ctx context.Context, asset, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, error) {
	rows, next, _, _, err := c.AssetMarketsAt(ctx, asset, cursor, limit, order)
	return rows, next, err
}

// AssetMarketsAt is AssetMarkets plus observed-at and staleness meta; see DistinctPairsExtAt.
func (c *CachedMarketsReader) AssetMarketsAt(ctx context.Context, asset, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, time.Time, bool, error) {
	if c.ttl <= 0 {
		rows, next, err := c.upstream.AssetMarkets(ctx, asset, cursor, limit, order)
		return rows, next, time.Time{}, false, err
	}
	key := newCacheKey("AssetMarkets").str(asset).str(cursor).int(limit).order(int(order)).build()
	return c.fetchPairs(ctx, "asset_markets", key, func(ctx context.Context) ([]Market, string, error) {
		return c.upstream.AssetMarkets(ctx, asset, cursor, limit, order)
	})
}

// AllPools is cached; the Sources filter is a SET that [cacheKey.strSet] sorts into the key so prewarm and handler share a slot.
func (c *CachedMarketsReader) AllPools(ctx context.Context, filter timescale.PoolsFilter, cursor string, limit int, order timescale.MarketsOrder) ([]Pool, string, error) {
	rows, next, _, _, err := c.AllPoolsStale(ctx, filter, cursor, limit, order)
	return rows, next, err
}

// AllPoolsStale is AllPools plus the rows' observed-at and whether they came from the stale-while-revalidate branch (see poolsStaleReader).
func (c *CachedMarketsReader) AllPoolsStale(ctx context.Context, filter timescale.PoolsFilter, cursor string, limit int, order timescale.MarketsOrder) ([]Pool, string, time.Time, bool, error) {
	if c.ttl <= 0 {
		rows, next, err := c.upstream.AllPools(ctx, filter, cursor, limit, order)
		return rows, next, time.Time{}, false, err
	}
	fetch := headLimit(cursor, limit)
	key := newCacheKey("AllPools").
		strSet(filter.Sources).
		str(filter.Base).str(filter.Quote).str(filter.Asset).
		str(cursor).int(fetch).order(int(order)).build()
	rows, next, at, stale, err := c.fetchPools(ctx, "all_pools", key, func(ctx context.Context) ([]Pool, string, error) {
		return c.upstream.AllPools(ctx, filter, cursor, fetch, order)
	})
	rows, next = sliceHead(rows, next, limit, func(p Pool) string {
		return timescale.PageCursor(order, p.Source+"|"+p.Base+"|"+p.Quote, p.Volume24hUSD)
	})
	return rows, next, at, stale, err
}

// marketsHeadLimit is the single first-page slot per listing: every first page up to it is sliced from one scan, since the
// scan aggregates the whole 14-day window whatever the LIMIT.
const marketsHeadLimit = 200

// headLimit is the limit to fetch and key on: marketsHeadLimit for a first page within it, else the caller's own.
func headLimit(cursor string, limit int) int {
	if cursor == "" && limit >= 1 && limit <= marketsHeadLimit {
		return marketsHeadLimit
	}
	return limit
}

// sliceHead trims a head-slot page to limit, re-deriving the next cursor from the last row kept.
func sliceHead[T any](rows []T, next string, limit int, cursorOf func(T) string) ([]T, string) {
	if limit < 1 || len(rows) <= limit {
		return rows, next
	}
	return rows[:limit], cursorOf(rows[limit-1])
}

// marketsRefreshBudget bounds the off-request SWR refresh; it must exceed the worst-case contended AllPools/DistinctPairs scan.
const marketsRefreshBudget = 30 * time.Second

// fetchPairs is the TTL + single-flight + stale-while-revalidate loop for pair methods (`op` is the metric label); observedAt/stale keep
// stale serves honest.
// Rows are always cloned, even to the cold leader; the clone runs outside the mutex since stored backing arrays are never rewritten.
func (c *CachedMarketsReader) fetchPairs(
	ctx context.Context,
	op, key string,
	upstream func(context.Context) ([]Market, string, error),
) ([]Market, string, time.Time, bool, error) {
	c.mu.Lock()
	e, ok := c.entries[key]

	// (A) Fresh hit.
	if ok && e.flight == nil && time.Since(e.at) < c.ttl {
		out, next, at := e.pairs, e.cursor, e.at
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("markets", op, "hit").Inc()
		return slices.Clone(out), next, at, false, nil
	}

	// (A') Stale-while-revalidate. A prior SUCCESSFUL fetch exists (e.at non-zero; failed cold
	// fetches delete the entry) but is expired. Serve it immediately and kick exactly one
	// background refresh; nobody waits on upstream. Rows keep their ORIGINAL e.at so failing
	// refreshes ride out on old-but-real data and observedAt/stale=true tell the truth.
	if ok && !e.at.IsZero() {
		out, next, at := e.pairs, e.cursor, e.at
		if e.flight == nil {
			done := make(chan struct{})
			e.flight = done
			entry := e
			c.mu.Unlock()
			obs.APICacheOpsTotal.WithLabelValues("markets", op, "stale").Inc()
			//nolint:gosec,contextcheck // G118 / contextcheck:
			// intentional. The SWR background refresh MUST use a
			// fresh context (runDetachedFill),
			// NOT the request ctx: it is cancelled the instant the
			// stale response is written, so reusing it would abort
			// every refresh — defeating the entire point of SWR.
			go runDetachedFill(c.logger, "api-markets-pairs-refresh", marketsRefreshBudget, done, marketsPageFill(upstream), c.settlePairs(op, key, entry))
			return slices.Clone(out), next, at, true, nil
		}
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("markets", op, "stale").Inc()
		return slices.Clone(out), next, at, true, nil
	}

	// (B)/(C) Cold: no prior success to serve. Join or start the fill, then wait on it OR this
	// caller's ctx. The fill runs detached (runDetachedFill), so one caller's abort cannot fail
	// the others or leave the key unfilled. The entry pointer is captured so result/err is read
	// off the SAME struct we joined, surviving the fill's delete-on-error.
	entry, leader := c.joinOrStartColdLocked(e, ok, key)
	if leader {
		//nolint:gosec,contextcheck // G118 / contextcheck: intentional
		// detached fill — see runDetachedFill.
		go runDetachedFill(c.logger, "api-markets-pairs-refresh", marketsRefreshBudget, entry.flight, marketsPageFill(upstream), c.settlePairs(op, key, entry))
	}
	ch := entry.flight
	c.mu.Unlock()
	if leader {
		obs.APICacheOpsTotal.WithLabelValues("markets", op, "miss").Inc()
	}
	select {
	case <-ch:
		c.mu.Lock()
		rows, cursor, at, err := entry.pairs, entry.cursor, entry.at, entry.err
		c.mu.Unlock()
		if err != nil {
			if !leader {
				obs.APICacheOpsTotal.WithLabelValues("markets", op, "miss").Inc()
			}
			return nil, "", time.Time{}, false, err
		}
		if !leader {
			obs.APICacheOpsTotal.WithLabelValues("markets", op, "hit").Inc()
		}
		return slices.Clone(rows), cursor, at, false, nil
	case <-ctx.Done():
		return nil, "", time.Time{}, false, ctx.Err()
	}
}

// joinOrStartColdLocked returns key's in-flight cold entry or installs a fresh one (leader=true) the caller must fill. Caller holds c.mu.
func (c *CachedMarketsReader) joinOrStartColdLocked(e *marketsCacheEntry, ok bool, key string) (*marketsCacheEntry, bool) {
	if ok && e.flight != nil {
		return e, false
	}
	entry := &marketsCacheEntry{flight: make(chan struct{})}
	evictOldestMarketsEntry(c.entries)
	c.entries[key] = entry
	return entry, true
}

// settleFailedFillLocked keeps serving a prior value on failure (retry next request); cold, it hands err to waiters and drops the entry.
// Caller holds c.mu.
func (c *CachedMarketsReader) settleFailedFillLocked(op, key string, entry *marketsCacheEntry, err error) {
	entry.flight = nil
	if !entry.at.IsZero() {
		obs.APICacheOpsTotal.WithLabelValues("markets", op, "refresh_error").Inc()
		c.logger.Warn("cache refresh failed; serving stale value", "cache", "markets", "op", op, "key", key, "err", err)
		return
	}
	entry.err = err
	if c.entries[key] == entry {
		delete(c.entries, key)
	}
}

// marketsPage carries one upstream page through runDetachedFill.
type marketsPage[T any] struct {
	rows   []T
	cursor string
}

// marketsPageFill adapts a (rows, cursor, err) upstream to runDetachedFill.
func marketsPageFill[T any](upstream func(context.Context) ([]T, string, error)) func(context.Context) (marketsPage[T], error) {
	return func(ctx context.Context) (marketsPage[T], error) {
		rows, cursor, err := upstream(ctx)
		return marketsPage[T]{rows, cursor}, err
	}
}

// settlePairs applies a fetchPairs fill (cold or SWR) to entry.
func (c *CachedMarketsReader) settlePairs(op, key string, entry *marketsCacheEntry) func(marketsPage[Market], error) {
	return func(page marketsPage[Market], err error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			c.settleFailedFillLocked(op, key, entry, err)
			return
		}
		entry.at = time.Now()
		entry.pairs = page.rows
		entry.cursor = page.cursor
		entry.flight = nil
	}
}

// fetchPools mirrors fetchPairs (SWR included) for AllPools; the ~8s per-source scan can't be made cheap, so SWR moves it off the request
// path.
func (c *CachedMarketsReader) fetchPools(
	ctx context.Context,
	op, key string,
	upstream func(context.Context) ([]Pool, string, error),
) ([]Pool, string, time.Time, bool, error) {
	c.mu.Lock()
	e, ok := c.entries[key]

	// (A) Fresh hit.
	if ok && e.flight == nil && time.Since(e.at) < c.ttl {
		out, next, at := e.pools, e.cursor, e.at
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("markets", op, "hit").Inc()
		return slices.Clone(out), next, at, false, nil
	}

	// (A') Stale-while-revalidate.
	if ok && !e.at.IsZero() {
		out, next, at := e.pools, e.cursor, e.at
		if e.flight == nil {
			done := make(chan struct{})
			e.flight = done
			entry := e
			c.mu.Unlock()
			obs.APICacheOpsTotal.WithLabelValues("markets", op, "stale").Inc()
			//nolint:gosec,contextcheck // G118 / contextcheck:
			// intentional — see fetchPairs (A'). The pools refresh
			// MUST outlive the stale response's request ctx.
			go runDetachedFill(c.logger, "api-markets-pools-refresh", marketsRefreshBudget, done, marketsPageFill(upstream), c.settlePools(op, key, entry))
			return slices.Clone(out), next, at, true, nil
		}
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("markets", op, "stale").Inc()
		return slices.Clone(out), next, at, true, nil
	}

	// (B)/(C) Cold: join or start a detached fill — see fetchPairs.
	entry, leader := c.joinOrStartColdLocked(e, ok, key)
	if leader {
		//nolint:gosec,contextcheck // G118 / contextcheck: intentional
		// detached fill — see runDetachedFill.
		go runDetachedFill(c.logger, "api-markets-pools-refresh", marketsRefreshBudget, entry.flight, marketsPageFill(upstream), c.settlePools(op, key, entry))
	}
	ch := entry.flight
	c.mu.Unlock()
	if leader {
		obs.APICacheOpsTotal.WithLabelValues("markets", op, "miss").Inc()
	}
	select {
	case <-ch:
		c.mu.Lock()
		rows, cursor, at, err := entry.pools, entry.cursor, entry.at, entry.err
		c.mu.Unlock()
		if err != nil {
			if !leader {
				obs.APICacheOpsTotal.WithLabelValues("markets", op, "miss").Inc()
			}
			return nil, "", time.Time{}, false, err
		}
		if !leader {
			obs.APICacheOpsTotal.WithLabelValues("markets", op, "hit").Inc()
		}
		return slices.Clone(rows), cursor, at, false, nil
	case <-ctx.Done():
		return nil, "", time.Time{}, false, ctx.Err()
	}
}

// settlePools is settlePairs for the Pool return type.
func (c *CachedMarketsReader) settlePools(op, key string, entry *marketsCacheEntry) func(marketsPage[Pool], error) {
	return func(page marketsPage[Pool], err error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			c.settleFailedFillLocked(op, key, entry, err)
			return
		}
		entry.at = time.Now()
		entry.pools = page.rows
		entry.cursor = page.cursor
		entry.flight = nil
	}
}
