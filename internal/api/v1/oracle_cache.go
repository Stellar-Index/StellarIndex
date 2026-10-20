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

// CachedOracleReader adds a per-process TTL cache and single-flight to LatestOracleUpdatesForAssets; the 3s TTL is below every oracle's publish cadence and the SQL must still meet its SLO on a miss.
type CachedOracleReader struct {
	upstream OracleReader
	ttl      time.Duration

	mu      sync.Mutex
	entries map[string]*oracleCacheEntry
}

type oracleCacheEntry struct {
	at     time.Time // fill time: drives the TTL and eviction
	asOf   time.Time // when the served rows were computed; never after at
	flight chan struct{}

	updates []canonical.OracleUpdate

	// err is set by the leader before close(flight) on a failing upstream call. Waiters hold a
	// pointer to the entry they joined, so they can still read err after the leader removes it
	// from the map (errors are not TTL-cached).
	err error
}

// oracleCacheMaxEntries bounds the map: the key is the sorted asset list plus source filter on a public unauthenticated endpoint,
// and only failures delete entries (empty is a success), so an asset-space walk would grow it to OOM. Oldest-first eviction
// mirrors historyCacheMaxEntries.
const oracleCacheMaxEntries = 4096

// evictIfFullLocked drops the oldest-filled entry at capacity; caller holds c.mu and calls it only when admitting a NEW key.
// In-flight entries are never evicted (waiters orphaned), so the map can briefly exceed the cap, bounded by concurrent requests.
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
		// `evicted` is not a read outcome: api_cache_miss_rate_high filters it (and
		// `refresh_error`) out of its denominator, else miss/(miss+evicted) caps at 0.5.
		obs.APICacheOpsTotal.WithLabelValues("oracle", op, "evicted").Inc()
	}
}

// NewCachedOracleReader wraps upstream with a TTL cache (ttl=0 disables); 3s is the production default.
func NewCachedOracleReader(upstream OracleReader, ttl time.Duration) *CachedOracleReader {
	return &CachedOracleReader{
		upstream: upstream,
		ttl:      ttl,
		entries:  map[string]*oracleCacheEntry{},
	}
}

// LatestOracleUpdatesForAsset delegates to the multi-key variant so the cache is keyed consistently.
func (c *CachedOracleReader) LatestOracleUpdatesForAsset(ctx context.Context, asset canonical.Asset, sourceFilter string) ([]canonical.OracleUpdate, error) {
	return c.LatestOracleUpdatesForAssets(ctx, []canonical.Asset{asset}, sourceFilter)
}

// LatestOracleUpdatesForAssets is cached under the sorted asset strings plus sourceFilter, so the same set in any order shares one slot.
func (c *CachedOracleReader) LatestOracleUpdatesForAssets(ctx context.Context, assets []canonical.Asset, sourceFilter string) ([]canonical.OracleUpdate, error) {
	rows, _, err := c.LatestOracleUpdatesForAssetsAt(ctx, assets, sourceFilter)
	return rows, err
}

// oracleUpdatesAtReader is an upstream that knows when its rows were
// computed, e.g. a Redis layer replaying an older DB read.
type oracleUpdatesAtReader interface {
	LatestOracleUpdatesForAssetsAt(ctx context.Context, assets []canonical.Asset, sourceFilter string) ([]canonical.OracleUpdate, time.Time, error)
}

// LatestOracleUpdatesForAssetsAt is LatestOracleUpdatesForAssets plus when the rows were computed; zero on an uncached read.
func (c *CachedOracleReader) LatestOracleUpdatesForAssetsAt(ctx context.Context, assets []canonical.Asset, sourceFilter string) ([]canonical.OracleUpdate, time.Time, error) {
	if c.ttl <= 0 {
		rows, err := c.upstream.LatestOracleUpdatesForAssets(ctx, assets, sourceFilter)
		return rows, time.Time{}, err
	}
	if len(assets) == 0 {
		return nil, time.Time{}, nil
	}
	keys := make([]string, len(assets))
	for i, a := range assets {
		keys[i] = a.String()
	}
	sort.Strings(keys)
	key := strings.Join(keys, "|") + "|" + sourceFilter

	return c.fetch(ctx, "latest_oracle_updates", key, func(ctx context.Context) ([]canonical.OracleUpdate, time.Time, error) {
		if at, ok := c.upstream.(oracleUpdatesAtReader); ok {
			return at.LatestOracleUpdatesForAssetsAt(ctx, assets, sourceFilter)
		}
		computedAt := time.Now()
		rows, err := c.upstream.LatestOracleUpdatesForAssets(ctx, assets, sourceFilter)
		return rows, computedAt, err
	})
}

// LatestOracleStreams is cached in one slot because /oracles and the home page both fetch it and the oracle_updates scan is slow.
func (c *CachedOracleReader) LatestOracleStreams(ctx context.Context) ([]canonical.OracleUpdate, error) {
	rows, _, err := c.LatestOracleStreamsAt(ctx)
	return rows, err
}

// LatestOracleStreamsAt is LatestOracleStreams plus the served entry's fill time.
func (c *CachedOracleReader) LatestOracleStreamsAt(ctx context.Context) ([]canonical.OracleUpdate, time.Time, error) {
	// A single key under the same 3 s TTL + single-flight the other reads
	// already use: one scan per TTL window, concurrent callers coalesce,
	// and errors are never cached (fetch drops the entry on failure).
	return c.fetch(ctx, "latest_oracle_streams", "", func(ctx context.Context) ([]canonical.OracleUpdate, time.Time, error) {
		rows, err := c.upstream.LatestOracleStreams(ctx)
		return rows, time.Time{}, err
	})
}

// oracleFetchBudget bounds a fill goroutine's upstream call independently of any caller's ctx, so
// one caller's abort cannot propagate to every caller single-flighted onto the same fill.
const oracleFetchBudget = 30 * time.Second

// errOracleFillPanicked goes to waiters whose fill goroutine panicked, instead of a nil-ish "no error, no data" (see fill). Mirrors
// CachedHistoryReader.errHistoryFillPanicked.
var errOracleFillPanicked = errors.New("oracle cache: fill panicked")

// fetch is the TTL + single-flight loop, as in CachedIssuersReader.fetchList, except a fill is DETACHED on oracleFetchBudget: a
// caller cancelling only stops that caller waiting and must not abort the fetch for others single-flighted onto the key.
func (c *CachedOracleReader) fetch(
	ctx context.Context,
	op, key string,
	upstream func(context.Context) ([]canonical.OracleUpdate, time.Time, error),
) ([]canonical.OracleUpdate, time.Time, error) {
	c.mu.Lock()
	e, ok := c.entries[key]

	// (A) Fresh hit.
	if ok && e.flight == nil && time.Since(e.at) < c.ttl {
		out, asOf := e.updates, e.asOf
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("oracle", op, "hit").Inc()
		return out, asOf, nil
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
		// see fetch's and fill's doc comments.
		go c.fill(key, entry, done, upstream)
	}
	flight := entry.flight
	c.mu.Unlock()
	obs.APICacheOpsTotal.WithLabelValues("oracle", op, "miss").Inc()

	select {
	case <-flight:
		if entry.err != nil {
			return nil, time.Time{}, entry.err
		}
		return entry.updates, entry.asOf, nil
	case <-ctx.Done():
		return nil, time.Time{}, ctx.Err()
	}
}

// fill runs the upstream call on oracleFetchBudget (not the caller's ctx) and settles entry for every waiter. The panic guard is
// needed because entry starts zero-valued: an unsettled panic would still close(done) and waiters would read an empty success.
func (c *CachedOracleReader) fill(
	key string,
	entry *oracleCacheEntry,
	done chan struct{},
	upstream func(context.Context) ([]canonical.OracleUpdate, time.Time, error),
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

	rows, asOf, err := upstream(ctx)

	c.mu.Lock()
	if err == nil {
		entry.at = time.Now()
		// A zero asOf (an upstream with no compute time) keeps the old
		// fill-time stamp; a later-than-fill one is clamped.
		if asOf.IsZero() || asOf.After(entry.at) {
			asOf = entry.at
		}
		entry.asOf = asOf
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

// settleFailedFill hands a failure to every waiter on entry and drops it (errors are never TTL-cached) unless a concurrent fresh fill
// replaced it.
func (c *CachedOracleReader) settleFailedFill(key string, entry *oracleCacheEntry, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.err = err
	if c.entries[key] == entry {
		delete(c.entries, key)
	}
}
