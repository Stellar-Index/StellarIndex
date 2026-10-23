package v1

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// swrCache is a keyed TTL cache with single-flight fills and stale-while-revalidate: once a key has filled, an expired value is
// served (dated by its fill time, flagged stale) while one detached fill replaces it, and a failed refresh keeps the old value.
type swrCache[V any] struct {
	logger     *slog.Logger // sinks a failed refresh and a panic recovered in a detached fill
	cache      string       // APICacheOpsTotal cache label
	fillName   string
	ttl        time.Duration
	maxEntries int // 0 = unbounded; set it whenever keys carry client-controlled parts

	mu      sync.Mutex
	entries map[string]*swrEntry[V]
}

type swrEntry[V any] struct {
	at     time.Time
	flight chan struct{}
	val    V
	// err is set before close(flight) on a failed cold fill; waiters hold the same entry, so they read it after it is deleted.
	err error
}

func newSWRCache[V any](logger *slog.Logger, cache, fillName string, ttl time.Duration, maxEntries int) *swrCache[V] {
	if logger == nil {
		logger = slog.Default()
	}
	return &swrCache[V]{
		logger: logger, cache: cache, fillName: fillName, ttl: ttl, maxEntries: maxEntries,
		entries: map[string]*swrEntry[V]{},
	}
}

func (c *swrCache[V]) count(op, result string) {
	obs.APICacheOpsTotal.WithLabelValues(c.cache, op, result).Inc()
}

// get returns key's value, its fill time and whether it is past the TTL. Only a key with no successful fill makes the caller wait,
// and then only on the fill or its own ctx: fills run detached (runDetachedFill), so one caller's abort cannot fail the others.
func (c *swrCache[V]) get(ctx context.Context, op, key string, upstream func(context.Context) (V, error)) (V, time.Time, bool, error) {
	var zero V
	r := c.lookup(op, key, upstream)
	if r.entry == nil {
		return r.val, r.at, r.stale, nil
	}
	select {
	case <-r.flight:
		c.mu.Lock()
		val, at, err := r.entry.val, r.entry.at, r.entry.err
		c.mu.Unlock()
		if err != nil {
			if !r.leader {
				c.count(op, "miss")
			}
			return zero, time.Time{}, false, err
		}
		if !r.leader {
			c.count(op, "hit")
		}
		return val, at, false, nil
	case <-ctx.Done():
		return zero, time.Time{}, false, ctx.Err()
	}
}

// peek is get without the wait: where get would block on a first fill (starting it if none is in flight), peek reports a miss.
func (c *swrCache[V]) peek(op, key string, upstream func(context.Context) (V, error)) (V, time.Time, bool, bool) {
	r := c.lookup(op, key, upstream)
	if r.entry != nil {
		var zero V
		return zero, time.Time{}, false, false
	}
	return r.val, r.at, r.stale, true
}

// filled reports whether key holds a value (fresh or stale), without starting a fill.
func (c *swrCache[V]) filled(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	return ok && !e.at.IsZero()
}

// lookupResult is a filled value (entry nil) or the cold-fill entry to wait on.
type lookupResult[V any] struct {
	val    V
	at     time.Time
	stale  bool
	entry  *swrEntry[V]
	flight chan struct{}
	leader bool
}

// lookup is get's locked step: a fresh or stale value returns at once (a stale one starts at most one refresh); otherwise it joins
// the first fill in flight or starts it.
func (c *swrCache[V]) lookup(op, key string, upstream func(context.Context) (V, error)) lookupResult[V] {
	c.mu.Lock()
	e, ok := c.entries[key]

	if ok && e.flight == nil && time.Since(e.at) < c.ttl {
		out, at := e.val, e.at
		c.mu.Unlock()
		c.count(op, "hit")
		return lookupResult[V]{val: out, at: at}
	}

	if ok && !e.at.IsZero() {
		out, at := e.val, e.at
		if e.flight == nil {
			e.flight = make(chan struct{})
			//nolint:gosec,contextcheck // G118 / contextcheck: intentional detached fill — see runDetachedFill.
			go runDetachedFill(c.logger, c.fillName, cacheFillBudget, e.flight, upstream, c.settle(op, key, e))
		}
		c.mu.Unlock()
		c.count(op, "stale")
		return lookupResult[V]{val: out, at: at, stale: true}
	}

	entry, leader := e, false
	if !ok || e.flight == nil {
		c.evictOldest(op)
		entry, leader = &swrEntry[V]{flight: make(chan struct{})}, true
		c.entries[key] = entry
		//nolint:gosec,contextcheck // G118 / contextcheck: intentional detached fill — see runDetachedFill.
		go runDetachedFill(c.logger, c.fillName, cacheFillBudget, entry.flight, upstream, c.settle(op, key, entry))
	}
	ch := entry.flight
	c.mu.Unlock()
	if leader {
		c.count(op, "miss")
	}
	return lookupResult[V]{entry: entry, flight: ch, leader: leader}
}

// settle applies a fill: cache on success; a failed refresh keeps the old value, a failed cold fill hands err to its waiters and
// drops the entry, so errors are never cached.
func (c *swrCache[V]) settle(op, key string, entry *swrEntry[V]) func(V, error) {
	return func(val V, err error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			if !entry.at.IsZero() {
				entry.flight = nil
				c.count(op, "refresh_error")
				c.logger.Warn("cache refresh failed; serving stale value", "cache", c.cache, "op", op, "key", key, "err", err)
				return
			}
			entry.err = err
			if c.entries[key] == entry {
				delete(c.entries, key)
			}
			return
		}
		entry.at = time.Now()
		entry.val = val
		entry.flight = nil
	}
}

// evictOldest drops the oldest-filled entry at capacity; caller holds mu. Cold fills in flight are never evicted (waiters orphaned).
func (c *swrCache[V]) evictOldest(op string) {
	if c.maxEntries <= 0 || len(c.entries) < c.maxEntries {
		return
	}
	var oldestKey string
	var oldestAt time.Time
	for k, e := range c.entries {
		if e.at.IsZero() {
			continue
		}
		if oldestKey == "" || e.at.Before(oldestAt) {
			oldestKey, oldestAt = k, e.at
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
		c.count(op, "evicted")
	}
}
