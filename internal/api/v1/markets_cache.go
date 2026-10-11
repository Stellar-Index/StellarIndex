package v1

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// CachedMarketsReader wraps a [MarketsReader] with a small per-key TTL cache. The
// five list endpoints it backs (DistinctPairsExt, SourceMarkets, AssetMarkets,
// AllPools, GetPairsVolumeHistory24hBatch) all run the same expensive
// 24h-trades-hypertable scan, hit by the explorer on every /markets, /pools and
// /dexes page load.
//
// Cache key: a stable string derived from the call's args. Single-flight +
// error-not-cached match the SourcesStatsReader wrapper. Per-pair lookups
// (PairMarket) and the sparkline batch are pass-through.
//
// Ownership: the cache owns what it stores, the caller owns what it gets. Every
// serving branch of fetchPairs / fetchPools returns a COPY of the entry's row
// slice, because handleMarkets and handlePools write rows in place (the
// dex-nonstandard-decimals last_price correction is not idempotent: a shared
// array would re-multiply the price on every hit).
//
// The copy is one level deep. Pointer and slice FIELDS (LastPrice,
// Volume24hUSD, VolumeHistory24h, ...) still alias the cached values, so a
// caller may REPLACE a field on its row but must never write THROUGH one.
type CachedMarketsReader struct {
	// logger sinks a panic recovered in a detached refresh goroutine.
	// It is the PROCESS DEFAULT rather than the API Server's logger:
	// these reader wrappers are constructed directly in
	// cmd/stellarindex-api/main.go before the Server exists, and
	// widening the exported constructor would churn every caller for a
	// sink that ends up in the same journal either way. The
	// load-bearing signal is stellarindex_worker_panics_total, which
	// worker.Report moves regardless of the sink.
	logger *slog.Logger

	upstream MarketsReader
	ttl      time.Duration

	mu      sync.Mutex
	entries map[string]*marketsCacheEntry
}

// marketsCacheMaxEntries bounds the entry map.
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
const marketsCacheMaxEntries = 4096

// evictOldestMarketsEntry drops the oldest-filled entry when the map is at
// capacity. Caller must hold the mutex.
//
// In-flight entries (at.IsZero()) are never evicted — dropping one
// would orphan its waiters from the fill about to close their channel.
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

	// err is set by the leader before close(flight) on a failing
	// upstream call. Waiters hold a pointer to the SAME entry they
	// joined the flight on — so even if the leader removes the entry
	// from the map (we don't TTL-cache errors), waiters can still
	// read entry.err here and return it instead of nil-derefing the
	// missing entry. See fetchPairs / fetchPools.
	err error
}

// NewCachedMarketsReader wraps `upstream` with a TTL cache. ttl=0
// disables the cache. 30s is the production default — these are
// trade-volume aggregates that move slowly, but tighter than
// sources_stats's 60s because /v1/markets is more user-visible
// (the front page).
func NewCachedMarketsReader(upstream MarketsReader, ttl time.Duration) *CachedMarketsReader {
	return &CachedMarketsReader{
		logger:   slog.Default(),
		upstream: upstream,
		ttl:      ttl,
		entries:  map[string]*marketsCacheEntry{},
	}
}

// PairMarket and GetPairsVolumeHistory24hBatch are pass-through —
// see type comment for rationale.
func (c *CachedMarketsReader) PairMarket(ctx context.Context, base, quote canonical.Asset) (Market, bool, error) {
	return c.upstream.PairMarket(ctx, base, quote)
}

func (c *CachedMarketsReader) GetPairsVolumeHistory24hBatch(ctx context.Context, pairs [][2]string) (map[string][]timescale.PairVolumePoint, error) {
	return c.upstream.GetPairsVolumeHistory24hBatch(ctx, pairs)
}

// FirstTradeBatch passes through uncached — inception is immutable
// once set and the call is opt-in via ?include=inception.
func (c *CachedMarketsReader) FirstTradeBatch(ctx context.Context, pairs [][2]string) (map[string]time.Time, error) {
	return c.upstream.FirstTradeBatch(ctx, pairs)
}

// DistinctPairsExt — cached. Plain [MarketsReader] shape; drops the
// staleness meta DistinctPairsExtAt surfaces (prewarm + non-/v1/markets
// callers don't need it).
func (c *CachedMarketsReader) DistinctPairsExt(ctx context.Context, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, error) {
	rows, next, _, _, err := c.DistinctPairsExtAt(ctx, cursor, limit, order)
	return rows, next, err
}

// DistinctPairsExtAt is DistinctPairsExt plus the served row-set's
// observed-at timestamp and staleness flag. /v1/markets uses it to stamp
// an honest as_of (the served data's real observation time) and
// flags.stale, instead of the SWR stale-serve path silently asserting
// stale:false / as_of=now over arbitrarily-old rows when refreshes keep
// failing (the same posture as /v1/contracts).
//
// observedAt is the zero time (→ caller stamps as_of=now, not stale) when
// the cache is disabled: an uncached read comes straight from upstream and
// is live-fresh.
func (c *CachedMarketsReader) DistinctPairsExtAt(ctx context.Context, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, time.Time, bool, error) {
	if c.ttl <= 0 {
		rows, next, err := c.upstream.DistinctPairsExt(ctx, cursor, limit, order)
		return rows, next, time.Time{}, false, err
	}
	key := newCacheKey("DistinctPairsExt").str(cursor).int(limit).order(int(order)).build()
	return c.fetchPairs(ctx, "distinct_pairs", key, func(ctx context.Context) ([]Market, string, error) {
		return c.upstream.DistinctPairsExt(ctx, cursor, limit, order)
	})
}

// SourceMarkets — cached. See DistinctPairsExt re: dropped staleness meta.
func (c *CachedMarketsReader) SourceMarkets(ctx context.Context, source, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, error) {
	rows, next, _, _, err := c.SourceMarketsAt(ctx, source, cursor, limit, order)
	return rows, next, err
}

// SourceMarketsAt is SourceMarkets plus the observed-at + staleness meta —
// see DistinctPairsExtAt.
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

// AssetMarketsAt is AssetMarkets plus the observed-at + staleness meta —
// see DistinctPairsExtAt.
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

// AllPools — cached. The Sources filter is a SET; [cacheKey.strSet]
// sorts it into the key so the prewarm goroutine and the handler
// (which build the DEX-source list from the same registry but must
// not have to agree on element ORDER) always hit the same slot —
// removing the fragile "handlers upstream sort sources so order is
// stable" convention the pre-typed key relied on.
func (c *CachedMarketsReader) AllPools(ctx context.Context, filter timescale.PoolsFilter, cursor string, limit int, order timescale.MarketsOrder) ([]Pool, string, error) {
	rows, next, _, _, err := c.AllPoolsStale(ctx, filter, cursor, limit, order)
	return rows, next, err
}

// AllPoolsStale is AllPools plus the served rows' observed-at and whether
// they came from the stale-while-revalidate branch (past the TTL) — see
// poolsStaleReader.
func (c *CachedMarketsReader) AllPoolsStale(ctx context.Context, filter timescale.PoolsFilter, cursor string, limit int, order timescale.MarketsOrder) ([]Pool, string, time.Time, bool, error) {
	if c.ttl <= 0 {
		rows, next, err := c.upstream.AllPools(ctx, filter, cursor, limit, order)
		return rows, next, time.Time{}, false, err
	}
	key := newCacheKey("AllPools").
		strSet(filter.Sources).
		str(filter.Base).str(filter.Quote).str(filter.Asset).
		str(cursor).int(limit).order(int(order)).build()
	return c.fetchPools(ctx, "all_pools", key, func(ctx context.Context) ([]Pool, string, error) {
		return c.upstream.AllPools(ctx, filter, cursor, limit, order)
	})
}

// marketsRefreshBudget bounds a stale-while-revalidate background
// refresh. It runs OFF the request path (users already have the
// stale value) so a generous budget is free; it just has to exceed
// the worst-case AllPools / DistinctPairs scan (~seconds, contended)
// so the refresh completes and the cache moves forward. Mirrors
// assetsRefreshBudget.
const marketsRefreshBudget = 30 * time.Second

// fetchPairs is the shared TTL + single-flight + stale-while-revalidate loop for
// the pair-returning methods. `op` is the metric label (`distinct_pairs` /
// `source_markets` / `asset_markets`). SWR semantics match asset_catalogue_cache.go's
// fetchRows: an expired entry serves its stale rows IMMEDIATELY and a single
// background refresh runs off the request path, because the AllPools/DistinctPairs
// scan cannot be made cheap (no per-source pre-aggregate exists).
//
// Returns the served rows + next cursor, observedAt (when the served rows were
// fetched upstream; zero on a cold miss/error) and stale (true whenever the
// served bytes are past the TTL). /v1/markets stamps these into as_of and
// flags.stale so a stale-serve (refresh failing) is never asserted as fresh.
//
// The rows returned are always a copy (slices.Clone), including to the cold
// LEADER, whose upstream result is the slice the entry keeps. The clone runs
// outside the mutex: a stored backing array is never written again (a refresh
// swaps in a new slice), so reading it unlocked is safe. See the type comment.
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

	// (A') Stale-while-revalidate. A prior SUCCESSFUL fetch exists
	// (e.at non-zero — failed cold fetches delete the entry) but is
	// expired. Serve the stale rows immediately; kick exactly one
	// background refresh if none is running. Concurrent callers
	// during the refresh also get stale — nobody waits on upstream.
	//
	// The served rows carry their ORIGINAL e.at (never re-stamped to
	// now): a run of failing refreshes rides out on old-but-real data,
	// and the returned observedAt/stale=true let the handler tell the
	// truth about it rather than claiming freshness now() can't back.
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

	// (B)/(C) Cold: no prior success to serve. Join the running fill or
	// start one, then wait on it OR this caller's ctx. The fill runs
	// detached (runDetachedFill), so one caller's abort cannot fail the others
	// or leave the key unfilled. The entry pointer is captured so the
	// result/err is read off the SAME struct we joined, surviving the
	// fill's delete-on-error.
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

// joinOrStartColdLocked returns the in-flight cold entry for key, or
// installs a fresh one (leader=true) whose fill the caller must start.
// Caller holds c.mu.
func (c *CachedMarketsReader) joinOrStartColdLocked(e *marketsCacheEntry, ok bool, key string) (*marketsCacheEntry, bool) {
	if ok && e.flight != nil {
		return e, false
	}
	entry := &marketsCacheEntry{flight: make(chan struct{})}
	evictOldestMarketsEntry(c.entries)
	c.entries[key] = entry
	return entry, true
}

// settleFailedFillLocked applies a failed fill to entry. With a prior
// success it keeps serving that stale value (retry next request); cold,
// it hands err to the waiters and drops the entry so errors are never
// cached. Caller holds c.mu.
func (c *CachedMarketsReader) settleFailedFillLocked(op, key string, entry *marketsCacheEntry, err error) {
	entry.flight = nil
	if !entry.at.IsZero() {
		obs.APICacheOpsTotal.WithLabelValues("markets", op, "refresh_error").Inc()
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

// fetchPools mirrors fetchPairs (SWR included) for AllPools' return
// type. The ~8s per-source pools scan cannot
// be made cheap (no complete per-(source,base,quote) pre-aggregate
// exists — prices_* collapse source, price_source_contributions is
// curated/sparse), so SWR moves it off the request path entirely
// with zero correctness loss.
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
