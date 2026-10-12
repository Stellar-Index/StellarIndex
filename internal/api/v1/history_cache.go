package v1

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// CachedHistoryReader adds a stale-while-revalidate cache to LatestTradePerSource only (behind /v1/observations); the cold fill runs detached so the handler's 8s ceiling cannot 503 it forever.
type CachedHistoryReader struct {
	// logger sinks a panic recovered in a detached refresh goroutine. It is the process default,
	// not the Server's logger, because the wrappers are built before the Server exists. The
	// load-bearing signal is stellarindex_worker_panics_total, which worker.Report moves anyway.
	logger *slog.Logger

	HistoryReader // embedded: every method pass-through unless overridden below

	ttl time.Duration

	mu      sync.Mutex
	entries map[string]*historyCacheEntry
}

// historyCacheMaxEntries bounds the entry map: the key `base|quote|source` accepts any CRC-valid strkey, a space an anonymous
// caller can walk free, and only failures delete entries, so without a cap one IP mints thousands of permanent entries a
// minute. Eviction is oldest-first as in accountStateCache.
const historyCacheMaxEntries = 4096

type historyCacheEntry struct {
	at     time.Time
	flight chan struct{}

	trades []canonical.Trade

	// err is set by the filler before close(flight) on a failing upstream call. Waiters hold a
	// pointer to the entry they joined, so they can read err after it leaves the map (errors are
	// not TTL-cached).
	err error
}

// NewCachedHistoryReader wraps upstream with a stale-while-revalidate cache for LatestTradePerSource; ttl=0 disables it.
func NewCachedHistoryReader(upstream HistoryReader, ttl time.Duration) *CachedHistoryReader {
	return &CachedHistoryReader{
		logger:        slog.Default(),
		HistoryReader: upstream,
		ttl:           ttl,
		entries:       map[string]*historyCacheEntry{},
	}
}

// historyRefreshBudget bounds a detached cold-fill or stale-while-revalidate refresh, independent of any request ctx (it must
// outlive the handler's 8s ceiling); matches coins/markets budgets.
const historyRefreshBudget = 30 * time.Second

// TradesInRangeAfterFromSource forwards uncached to an upstream that supports the single-source read and errors otherwise, so an unchecked caller never gets unfiltered rows.
func (c *CachedHistoryReader) TradesInRangeAfterFromSource(
	ctx context.Context, pair canonical.Pair, source string,
	from, to, afterTs time.Time, afterLedger uint32,
	afterTxHash, afterSource string, afterOpIndex uint32, limit int,
) ([]canonical.Trade, error) {
	sr, ok := c.HistoryReader.(SourceHistoryReader)
	if !ok {
		return nil, errors.New("history: upstream reader cannot filter by source")
	}
	return sr.TradesInRangeAfterFromSource(ctx, pair, source, from, to,
		afterTs, afterLedger, afterTxHash, afterSource, afterOpIndex, limit)
}

// evictIfFullLocked drops the oldest-filled entry when the map is at capacity. Caller must hold
// c.mu. In-flight entries (at.IsZero()) are never evicted, since that would orphan their waiters.
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

type freshHistoryKey struct{}

// withFreshHistory marks ctx so [CachedHistoryReader.LatestTradePerSource] reads through to the store: a caller stamping its
// own as_of=now (the observations stream) must not re-emit a cached trade as current.
func withFreshHistory(ctx context.Context) context.Context {
	return context.WithValue(ctx, freshHistoryKey{}, true)
}

func (c *CachedHistoryReader) LatestTradePerSource(
	ctx context.Context, pair canonical.Pair, sourceFilter string,
) ([]canonical.Trade, error) {
	if fresh, _ := ctx.Value(freshHistoryKey{}).(bool); fresh || c.ttl <= 0 {
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

	// (B)/(C) Cold: ensure a detached fill is in flight, then wait on it or the request ctx. The
	// caller is bounded by its own ctx; the fill completes on its own budget and warms the cache.
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

// errHistoryFillPanicked is what a cold waiter gets when the fill it joined panicked, distinct from upstream errors so
// "refresher died" differs from "query failed".
var errHistoryFillPanicked = errors.New("history cache: fill panicked")

// settleFailedFill applies failed-fill bookkeeping: keep a stale value and clear in-flight so the next expiry retries, or with
// no prior value hand the error to waiters and drop the entry. Split out so the panic guard runs the same transitions; a guard
// that only reported would latch the entry in flight forever.
func (c *CachedHistoryReader) settleFailedFill(key string, entry *historyCacheEntry, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !entry.at.IsZero() {
		entry.flight = nil
		return
	}
	entry.err = err
	if c.entries[key] == entry {
		delete(c.entries, key)
	}
}

func (c *CachedHistoryReader) fill(
	key string,
	entry *historyCacheEntry,
	done chan struct{},
	upstream func(context.Context) ([]canonical.Trade, error),
) {
	defer close(done)
	// Registered AFTER close(done), so it runs BEFORE it: waiters read entry.err / entry.trades
	// once `done` closes, so the entry must be settled before that happens-before edge fires.
	//
	// Settling on a panic is the point. Otherwise entry.flight stays set and entry.at zero forever,
	// and every waiter reads nil error and nil trades as an EMPTY 200 OK for the life of the
	// process. A permanently-empty cache reporting success is worse than the crash, so recover
	// runs the error path's bookkeeping (see settleFailedFill). The panic source is upstream,
	// which runs before the lock is taken, so settleFailedFill cannot deadlock on c.mu.
	defer func() {
		if rec := recover(); rec != nil {
			worker.Report(c.logger, "api-history-latest-per-source-fill", rec)
			c.settleFailedFill(key, entry, errHistoryFillPanicked)
		}
	}()
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
		// markets_cache.go settleFailedFillLocked.)
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
