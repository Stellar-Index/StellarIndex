package v1

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// CachedOracleReader wraps an [OracleReader] with a per-process TTL
// cache + single-flight refetch on the high-traffic
// LatestOracleUpdatesForAssets path. F-0013 audit (2026-05-26)
// measured `/v1/oracle/latest` at p95 ~271 ms — over the 200 ms SLO.
//
// The underlying SQL is a DISTINCT ON (source) scan across
// oracle_updates with `asset = ANY($1)`. The only indexes that help
// the asset filter are (asset, ts DESC) and (asset, quote, ts DESC);
// neither covers the DISTINCT ON (source) keyset so a Sort step is
// unavoidable. Adding a (source, asset, ts DESC) index would help
// but migrations are operator-manual (feedback_migrations_not_auto_deployed)
// and oracle freshness updates on a 10–60 s cadence anyway — a short
// TTL cache captures the same wins without touching the schema.
//
// TTL of 3 s is well below the freshest oracle's publish cadence
// (Redstone ~10 s, Reflector ~30 s, Band ~60 s) so customers never
// see oracle data older than they would have without the cache. The
// pattern mirrors CachedIssuersReader/CachedMarketsReader (the
// delete-on-error / waiter-err-pointer single-flight shape).
//
// LatestOracleUpdatesForAsset delegates to the multi-key variant so
// the cache key space is consolidated. LatestOracleStreams is
// pass-through — its caller is /v1/oracles/streams, which is on the
// explorer's status-tile path (~one call per page load), not the
// SLA-probed /v1/oracle/latest hot path.
type CachedOracleReader struct {
	upstream OracleReader
	ttl      time.Duration

	mu      sync.Mutex
	entries map[string]*oracleCacheEntry
}

type oracleCacheEntry struct {
	at     time.Time
	flight chan struct{}

	updates []canonical.OracleUpdate

	// err is set by the leader before close(flight) on a failing
	// upstream call. Waiters hold a pointer to the SAME entry they
	// joined the flight on — so even if the leader removes the
	// entry from the map (we don't TTL-cache errors), waiters can
	// still read entry.err here and return it instead of nil-
	// derefing the missing entry. Same pattern as
	// CachedMarketsReader / CachedIssuersReader.
	err error
}

// oracleCacheMaxEntries bounds the entry map.
//
// The key is the caller's sorted asset list plus the source filter, and
// `/v1/oracle/latest?asset=` is public and unauthenticated — every
// distinct asset an anonymous caller names mints a new key. Successful
// fills were never evicted (only failures delete their entry, and an
// empty result for an asset no oracle quotes is a SUCCESS), so a walk of
// the asset key space grew the map without limit until the process OOMed.
//
// The cap + oldest-first eviction mirrors the bounded siblings
// (historyCacheMaxEntries, assetDetailCacheMaxEntries) and sits far above
// the real working set: the SLA probe and the explorer poll a handful of
// assets, and a multi-asset request collapses to one key.
const oracleCacheMaxEntries = 4096

// evictIfFullLocked drops the oldest-filled entry when the map is at
// capacity. Caller must hold c.mu, and must only call it when admitting a
// NEW key — replacing an expired entry is size-neutral and would
// otherwise cost an unrelated live key for nothing.
//
// In-flight entries (at.IsZero()) are never evicted — dropping one would
// orphan its waiters from the fill that is about to close their channel.
// The map can therefore sit briefly above the cap when every entry is a
// live fill, but that overshoot is bounded by the number of concurrent
// requests, not by the key space, and reclaims as those fills land.
// Evicting a still-fresh entry costs at most one extra upstream query on
// the next request for it, so correctness is unaffected. With the cap far
// above the working set the scan is cheap and runs only on
// insert-at-capacity.
func (c *CachedOracleReader) evictIfFullLocked(op string) {
	if len(c.entries) < oracleCacheMaxEntries {
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
		// NOTE: `evicted` is NOT a read outcome, so the
		// api_cache_miss_rate_high alert filters it (and
		// `refresh_error`) out of its denominator in both rule trees.
		// Left unfiltered it cancelled the very miss storm it is meant
		// to catch: miss/(miss+evicted) can never exceed 0.5.
		obs.APICacheOpsTotal.WithLabelValues("oracle", op, "evicted").Inc()
	}
}

// NewCachedOracleReader wraps `upstream` with a TTL cache. ttl=0
// disables caching (every call passes through). 3 s is the
// production default — well below every oracle's publish cadence.
func NewCachedOracleReader(upstream OracleReader, ttl time.Duration) *CachedOracleReader {
	return &CachedOracleReader{
		upstream: upstream,
		ttl:      ttl,
		entries:  map[string]*oracleCacheEntry{},
	}
}

// LatestOracleUpdatesForAsset delegates to the multi-key variant so
// the cache is keyed consistently regardless of which entry point
// the caller used.
func (c *CachedOracleReader) LatestOracleUpdatesForAsset(ctx context.Context, asset canonical.Asset, sourceFilter string) ([]canonical.OracleUpdate, error) {
	return c.LatestOracleUpdatesForAssets(ctx, []canonical.Asset{asset}, sourceFilter)
}

// LatestOracleUpdatesForAssets is cached. The key is the sorted
// asset-string list joined by `|`, then `|` + sourceFilter. Sort is
// stable across callers that pass the same set in a different order
// (e.g. [native, crypto:XLM] and [crypto:XLM, native] both share the
// same cache slot).
func (c *CachedOracleReader) LatestOracleUpdatesForAssets(ctx context.Context, assets []canonical.Asset, sourceFilter string) ([]canonical.OracleUpdate, error) {
	if c.ttl <= 0 {
		return c.upstream.LatestOracleUpdatesForAssets(ctx, assets, sourceFilter)
	}
	if len(assets) == 0 {
		return nil, nil
	}
	keys := make([]string, len(assets))
	for i, a := range assets {
		keys[i] = a.String()
	}
	sort.Strings(keys)
	key := strings.Join(keys, "|") + "|" + sourceFilter

	return c.fetch(ctx, "latest_oracle_updates", key, func(ctx context.Context) ([]canonical.OracleUpdate, error) {
		return c.upstream.LatestOracleUpdatesForAssets(ctx, assets, sourceFilter)
	})
}

// LatestOracleStreams — cached like its siblings (#332 F5, 2026-09-02).
//
// This was the reader's one pass-through, on the theory that the endpoint
// was low-frequency enough not to be worth a cache slot. Measurement
// refuted it: /v1/oracle/streams is fetched by BOTH /oracles and the home
// page, and every hit re-ran the oracle_updates scan and rebuilt ~34 KB —
// 0.43–0.46 s per request WARM on production. (The superseded reasoning is
// recorded rather than deleted because "wrapping it would scatter the
// working set" is the right instinct in general; what made it wrong here
// is that this call has no per-request dimensions, so it occupies exactly
// one slot.)
func (c *CachedOracleReader) LatestOracleStreams(ctx context.Context) ([]canonical.OracleUpdate, error) {
	// A single key under the same 3 s TTL + single-flight the other reads
	// already use: one scan per TTL window, concurrent callers coalesce,
	// and errors are never cached (fetch drops the entry on failure).
	return c.fetch(ctx, "latest_oracle_streams", "", func(ctx context.Context) ([]canonical.OracleUpdate, error) {
		return c.upstream.LatestOracleStreams(ctx)
	})
}

// oracleFetchBudget bounds a fill goroutine's own upstream call —
// independent of any single caller's ctx. See fetch's fill comment:
// this is what stops one caller's abort from propagating to every
// caller single-flighted onto the same fill. Mirrors
// CachedHistoryReader's historyRefreshBudget.
const oracleFetchBudget = 30 * time.Second

// errOracleFillPanicked is handed to waiters when the fill goroutine
// they joined panicked, rather than a nil-ish "no error, no data" —
// see fill's comment. Mirrors CachedHistoryReader.errHistoryFillPanicked.
var errOracleFillPanicked = errors.New("oracle cache: fill panicked")

// fetch is the TTL + single-flight loop, in the same shape as
// CachedIssuersReader.fetchList EXCEPT for who runs the upstream
// call: a fill is always DETACHED into its own goroutine on
// oracleFetchBudget, never bound to whichever caller happened to
// trigger it. A caller that cancels its own ctx (client disconnect,
// its own deadline) only stops that caller from waiting — it must
// not abort the in-flight fetch out from under every other caller
// single-flighted onto the same key. Mirrors CachedHistoryReader's
// cold path (LatestTradePerSource), the proven fix for this shape.
func (c *CachedOracleReader) fetch(
	ctx context.Context,
	op, key string,
	upstream func(context.Context) ([]canonical.OracleUpdate, error),
) ([]canonical.OracleUpdate, error) {
	c.mu.Lock()
	e, ok := c.entries[key]

	// (A) Fresh hit.
	if ok && e.flight == nil && time.Since(e.at) < c.ttl {
		out := e.updates
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("oracle", op, "hit").Inc()
		return out, nil
	}

	// (B)/(C) No usable value: join the running fill, or start one.
	// Either way this caller only ever WAITS on it — the fill's own
	// upstream call runs on its own detached context (see fill).
	var entry *oracleCacheEntry
	if ok && e.flight != nil {
		entry = e
	} else {
		done := make(chan struct{})
		entry = &oracleCacheEntry{flight: done}
		if !ok {
			c.evictIfFullLocked(op)
		}
		c.entries[key] = entry
		//nolint:gosec,contextcheck // G118 / contextcheck:
		// intentional detached fill — the leader's own ctx must not
		// abort a fill every other single-flighted waiter depends on;
		// see fetch's and fill's doc comments (RLT-439).
		go c.fill(key, entry, done, upstream)
	}
	flight := entry.flight
	c.mu.Unlock()
	obs.APICacheOpsTotal.WithLabelValues("oracle", op, "miss").Inc()

	select {
	case <-flight:
		if entry.err != nil {
			return nil, entry.err
		}
		return entry.updates, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fill runs the upstream call on oracleFetchBudget — NOT the ctx of
// whichever caller triggered it — and settles entry for every waiter
// holding its pointer. This is what fetch's (B)/(C) branch depends
// on: the fill must outlive any one caller's cancellation.
//
// The panic guard exists because entry starts zero-valued (no error,
// no updates): a fill that panicked without settling would still
// close(done), and every waiter would read that as a legitimate empty
// success rather than a failure. Mirrors CachedHistoryReader.fill.
func (c *CachedOracleReader) fill(
	key string,
	entry *oracleCacheEntry,
	done chan struct{},
	upstream func(context.Context) ([]canonical.OracleUpdate, error),
) {
	defer close(done)
	defer func() {
		if rec := recover(); rec != nil {
			worker.Report(nil, "api-oracle-cache-fill", rec)
			c.settleFailedFill(key, entry, errOracleFillPanicked)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), oracleFetchBudget)
	defer cancel()

	rows, err := upstream(ctx)

	c.mu.Lock()
	if err == nil {
		entry.at = time.Now()
		entry.updates = rows
		entry.flight = nil
	} else {
		entry.err = err
		if c.entries[key] == entry {
			delete(c.entries, key)
		}
	}
	c.mu.Unlock()
}

// settleFailedFill hands a fill failure to every waiter holding
// entry's pointer and drops it from the map — errors are never
// TTL-cached — unless a concurrent fresh fill already replaced it.
func (c *CachedOracleReader) settleFailedFill(key string, entry *oracleCacheEntry, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.err = err
	if c.entries[key] == entry {
		delete(c.entries, key)
	}
}
