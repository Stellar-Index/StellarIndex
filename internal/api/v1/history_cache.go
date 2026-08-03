package v1

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// CachedHistoryReader wraps a [HistoryReader], adding a small
// stale-while-revalidate cache to **`LatestTradePerSource` only** —
// the storage primitive behind `/v1/observations` (ADR-0018
// Surface 3). Every other HistoryReader method is pure pass-through
// (they're already range-bounded / CAGG-backed and fast).
//
// Why this one method: `LatestTradePerSource` is a
// `DISTINCT ON (source) … WHERE base=$1 AND quote=$2
// ORDER BY source, ts DESC` over the `trades` hypertable with no
// time bound. NOTE (re-measured on r1 2026-08-03): this comment used
// to claim TimescaleDB "cannot do chunk exclusion and probes every
// chunk — multiple seconds". That is FALSE — migration 0037's
// `trades_pair_source_ts_idx` exists and the plan is a Merge Append
// of per-chunk SkipScans, measured at 49 ms (native/fiat:USD),
// 289 ms (heaviest pair) and 47 ms to prove a novel pair empty. The
// cache still earns its place by keeping the status page's poll off
// the database, but do not size risk decisions on the old number —
// it was off by roughly 1000x. The handler caps
// it at 8s and returns 503 on overrun (#29). The real
// query-cheapening fix is the documented-but-missing
// `(base_asset, quote_asset, source, ts DESC)` index — see the note
// on [HistoryReader.LatestTradePerSource] and the durable follow-up.
//
// The status page polls one fixed key (`native|fiat:USD|`) every
// ~2 min, so SWR gives a ~100% hit rate after warm-up with zero
// correctness loss (the exact query result — including a legitimate
// empty slice — is cached).
//
// SWR shape mirrors the proven #22/#23 pattern
// (asset_catalogue_cache.go / markets_cache.go) with one deliberate change:
// the cold fill runs in a **detached** goroutine on its own budget,
// not the request ctx. The handler's hard 8s ceiling would
// otherwise fail every cold call before it could populate the cache
// — the endpoint would 503 forever and never warm. Decoupling the
// fill lets the first caller(s) 503 (bounded by their own ctx)
// while the fill completes out-of-band and warms the cache for the
// next poll.
type CachedHistoryReader struct {
	HistoryReader // embedded: every method pass-through unless overridden below

	ttl time.Duration

	mu      sync.Mutex
	entries map[string]*historyCacheEntry
}

// historyCacheMaxEntries bounds the entry map.
//
// The key is `base|quote|source`, and both asset components accept any
// CRC-valid strkey — a key space an anonymous caller can walk for free.
// Successful fills were never evicted (only failures deleted their
// entry, and an empty result for a nonexistent pair is a SUCCESS), so
// one IP at the 6000/min anon budget minted ~6000 permanent entries a
// minute — roughly 1.7 GB/day of unreclaimable memory on a host that
// also runs Postgres, ClickHouse, MinIO and galexie's captive core,
// with no metric on the map size (cold audit 2026-08-03).
//
// The cap + oldest-first eviction mirrors accountStateCache's
// established shape (internal/storage/clickhouse/account_state_cache.go).
// It is far above the real working set: the status page polls a handful
// of pairs, and every legitimate consumer is bounded by the configured
// source registry.
const historyCacheMaxEntries = 4096

type historyCacheEntry struct {
	at     time.Time
	flight chan struct{}

	trades []canonical.Trade

	// err is set by the filler before close(flight) on a failing
	// upstream call. Waiters hold a pointer to the SAME entry they
	// joined on, so they can read err even though we delete the
	// entry from the map (errors are not TTL-cached). Mirrors
	// markets_cache.go's marketsCacheEntry.err.
	err error
}

// NewCachedHistoryReader wraps `upstream` so `LatestTradePerSource`
// is stale-while-revalidate cached. ttl=0 disables the cache
// (pure pass-through). Production wires 2m (mirrors the markets /
// coins cache TTL — observations move with trades but the status
// page tolerates 2m staleness on a "latest per source" surface).
func NewCachedHistoryReader(upstream HistoryReader, ttl time.Duration) *CachedHistoryReader {
	return &CachedHistoryReader{
		HistoryReader: upstream,
		ttl:           ttl,
		entries:       map[string]*historyCacheEntry{},
	}
}

// historyRefreshBudget bounds a detached cold-fill or
// stale-while-revalidate background refresh — independent of any
// request ctx (the whole point: outlive the handler's 8s ceiling).
// Matches coins/markets refresh budgets (the proven #22 pattern).
const historyRefreshBudget = 30 * time.Second

// LatestTradePerSource is the one cached method. See type doc.
// evictIfFullLocked drops the oldest-filled entry when the map is at
// capacity. Caller must hold c.mu.
//
// In-flight entries (at.IsZero()) are never evicted — dropping one
// would orphan its waiters from the fill that is about to close their
// channel. With the cap far above the working set, a scan is cheap and
// runs only on insert-at-capacity.
func (c *CachedHistoryReader) evictIfFullLocked() {
	if len(c.entries) < historyCacheMaxEntries {
		return
	}
	var (
		oldestKey string
		oldestAt  time.Time
	)
	for k, e := range c.entries {
		if e.at.IsZero() {
			continue // fill in flight — waiters depend on it
		}
		if oldestKey == "" || e.at.Before(oldestAt) {
			oldestKey, oldestAt = k, e.at
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
		obs.APICacheOpsTotal.WithLabelValues("observations", "latest_per_source", "evicted").Inc()
	}
}

func (c *CachedHistoryReader) LatestTradePerSource(
	ctx context.Context, pair canonical.Pair, sourceFilter string,
) ([]canonical.Trade, error) {
	if c.ttl <= 0 {
		return c.HistoryReader.LatestTradePerSource(ctx, pair, sourceFilter)
	}
	key := pair.Base.String() + "|" + pair.Quote.String() + "|" + sourceFilter
	upstream := func(uctx context.Context) ([]canonical.Trade, error) {
		return c.HistoryReader.LatestTradePerSource(uctx, pair, sourceFilter)
	}

	c.mu.Lock()
	e, ok := c.entries[key]

	// (A) Fresh hit.
	if ok && e.flight == nil && time.Since(e.at) < c.ttl {
		out := e.trades
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("observations", "latest_per_source", "hit").Inc()
		return out, nil
	}

	// (A') Stale-while-revalidate: have a value, serve it now, kick a
	// single-flighted detached refresh.
	if ok && !e.at.IsZero() {
		out := e.trades
		if e.flight == nil {
			done := make(chan struct{})
			e.flight = done
			entry := e
			c.mu.Unlock()
			obs.APICacheOpsTotal.WithLabelValues("observations", "latest_per_source", "stale").Inc()
			//nolint:gosec,contextcheck // G118 / contextcheck:
			// intentional — the refresh MUST outlive the stale
			// response's request ctx (same as markets_cache.go
			// fetchPools (A')).
			go c.fill(key, entry, done, upstream)
			return out, nil
		}
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("observations", "latest_per_source", "stale").Inc()
		return out, nil
	}

	// (B)/(C) Cold: no usable value. Ensure a detached fill is in
	// flight, then wait on it OR the request ctx — whichever first.
	// The caller is bounded by its own ctx (handler 503s at 8s); the
	// fill always runs to completion on its own budget and warms the
	// cache for the next poll.
	var entry *historyCacheEntry
	if ok && e.flight != nil {
		entry = e
	} else {
		done := make(chan struct{})
		entry = &historyCacheEntry{flight: done}
		c.evictIfFullLocked()
		c.entries[key] = entry
		//nolint:gosec,contextcheck // G118 / contextcheck:
		// intentional detached fill — see type doc.
		go c.fill(key, entry, done, upstream)
	}
	flight := entry.flight
	c.mu.Unlock()
	obs.APICacheOpsTotal.WithLabelValues("observations", "latest_per_source", "miss").Inc()

	select {
	case <-flight:
		if entry.err != nil {
			return nil, entry.err
		}
		return entry.trades, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fill runs the upstream query on a detached budget and updates the
// entry. On success it caches (and clears flight so the entry is
// reusable as a fresh/stale value). On error it does NOT cache —
// the entry is removed from the map; waiters still read entry.err
// via their retained pointer. Mirrors markets_cache.go refreshPools
// + the cold-leader error path, unified.
func (c *CachedHistoryReader) fill(
	key string,
	entry *historyCacheEntry,
	done chan struct{},
	upstream func(context.Context) ([]canonical.Trade, error),
) {
	defer close(done)
	ctx, cancel := context.WithTimeout(context.Background(), historyRefreshBudget)
	defer cancel()

	rows, err := upstream(ctx)

	c.mu.Lock()
	switch {
	case err == nil:
		entry.at = time.Now()
		entry.trades = rows
		entry.flight = nil
	case !entry.at.IsZero():
		// Stale-while-revalidate refresh failed: KEEP serving the
		// existing stale value. Don't set entry.err, don't delete —
		// just clear flight so the next expiry retries. (Mirrors
		// markets_cache.go refreshPools.)
		entry.flight = nil
	default:
		// Cold fill failed (no prior value): propagate the error to
		// waiters via their retained entry pointer and don't
		// TTL-cache it. Only delete if still the mapped entry (a
		// concurrent fresh fill may have replaced it).
		entry.err = err
		if c.entries[key] == entry {
			delete(c.entries, key)
		}
	}
	c.mu.Unlock()

	if err != nil {
		obs.APICacheOpsTotal.WithLabelValues("observations", "latest_per_source", "refresh_error").Inc()
	}
}
