package clickhouse

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// TTLVerdictCacheTTL bounds how stale a served archived-pair verdict set may
// be before a background re-classification starts. TTL lapse moves on
// day/week scales, while each recompute scans the ~586M-row ttl prefix;
// running it inline per request held /v1/pools/reserves in the 503 class.
const TTLVerdictCacheTTL = 30 * time.Minute

// ttlVerdictRefreshTimeout bounds one detached re-classification (minutes
// under load): generous so it isn't abandoned, bounded so it can't pin the flight.
const ttlVerdictRefreshTimeout = 5 * time.Minute

// ttlLivenessCache is a stale-while-revalidate snapshot of per-key TTL
// liveness verdicts, fronting [ClassifyTTLLiveness] for SoroswapPairReserves.
//
// Fresh: served from memory. Stale or new keys: served anyway (missing keys
// read as TTLUnknown, which callers KEEP: fail-open) while ONE detached
// recompute runs. Never filled: the caller waits on that detached compute,
// bounded by its OWN deadline; the compute outlives a caller that gives up,
// so the retry serves warm.
//
// compute is injected so the cache is unit-testable without ClickHouse.
type ttlLivenessCache struct {
	mu        sync.Mutex
	verdicts  map[string]TTLLiveness
	fetchedAt time.Time
	flight    *ttlFlight

	compute func(ctx context.Context, keys []string) (map[string]TTLLiveness, error)
	// onErr, when set, observes detached-refresh failures (a failing refresh
	// pins the snapshot stale). Optional.
	onErr func(error)
}

// ttlFlight is one in-flight classification. done closes when it finishes;
// err carries its failure (nil on success), readable only AFTER done closes
// (the close is the happens-before edge).
type ttlFlight struct {
	done chan struct{}
	err  error
}

// errTTLRefreshPanicked is what waiters on a flight whose recompute panicked
// receive: a real error, so a panic reads as a failed refresh rather than a
// silent success serving an empty verdict set.
var errTTLRefreshPanicked = errors.New(
	"clickhouse: ttl-liveness detached refresh panicked; verdict snapshot unchanged")

// endFlight clears the in-flight marker and publishes `err` to every waiter.
// It must run from a DEFER in the refresh goroutine: containing a panic
// without releasing here would block coldFill's waiters forever.
func (c *ttlLivenessCache) endFlight(fl *ttlFlight, err error) {
	c.mu.Lock()
	c.flight = nil
	c.mu.Unlock()
	fl.err = err
	close(fl.done)
}

func newTTLLivenessCache(compute func(ctx context.Context, keys []string) (map[string]TTLLiveness, error)) *ttlLivenessCache {
	return &ttlLivenessCache{compute: compute}
}

// resolve returns the liveness verdict for every requested key. Nil-safe: a
// nil compute reports everything unknown.
func (c *ttlLivenessCache) resolve(ctx context.Context, keys []string) (map[string]TTLLiveness, error) {
	if c == nil || c.compute == nil {
		out := make(map[string]TTLLiveness, len(keys))
		for _, k := range keys {
			out[k] = TTLUnknown
		}
		return out, nil
	}

	if out, filled, needsRefresh := c.fromSnapshot(keys); filled {
		if needsRefresh {
			c.kickRefresh(keys) //nolint:contextcheck // intentional detach — the recompute must outlive any one caller (see the type doc)
		}
		return out, nil
	}
	return c.coldFill(ctx, keys)
}

// fromSnapshot serves the requested keys from a filled snapshot. filled=false
// when nothing was ever stored; needsRefresh=true when stale or a key is
// missing (the CALLER kicks the recompute, keeping the detach a single call site).
func (c *ttlLivenessCache) fromSnapshot(keys []string) (out map[string]TTLLiveness, filled, needsRefresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetchedAt.IsZero() {
		return nil, false, false
	}
	out = make(map[string]TTLLiveness, len(keys))
	missing := false
	for _, k := range keys {
		v, ok := c.verdicts[k]
		if !ok {
			missing = true
			v = TTLUnknown // fail-open: never drop what we merely haven't classified
		}
		out[k] = v
	}
	stale := time.Since(c.fetchedAt) > TTLVerdictCacheTTL
	return out, true, stale || missing
}

// coldFill kicks the detached compute and waits for it, bounded by the
// CALLER's deadline only; the fill still lands if the caller gives up.
func (c *ttlLivenessCache) coldFill(ctx context.Context, keys []string) (map[string]TTLLiveness, error) {
	fl := c.kickRefresh(keys) //nolint:contextcheck // intentional detach — a caller that times out must not kill the fill (see the type doc)
	select {
	case <-fl.done:
		if out, filled, _ := c.fromSnapshot(keys); filled {
			return out, nil
		}
		return nil, fl.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// store swaps in a freshly computed verdict set.
func (c *ttlLivenessCache) store(verdicts map[string]TTLLiveness) {
	c.mu.Lock()
	c.verdicts = verdicts
	c.fetchedAt = time.Now()
	c.mu.Unlock()
}

// kickRefresh starts ONE detached recompute (returning the existing flight
// while one is up). It computes the UNION of the caller's keys and the
// snapshot's: store() replaces the whole map, so a subset (?pool=) would evict
// other pairs' verdicts and re-open the fail-open TTLUnknown->keep path.
// Keys that leave the registry linger until restart; the registry is grow-only.
func (c *ttlLivenessCache) kickRefresh(keys []string) *ttlFlight {
	c.mu.Lock()
	if c.flight != nil {
		fl := c.flight
		c.mu.Unlock()
		return fl
	}
	fl := &ttlFlight{done: make(chan struct{})}
	c.flight = fl
	snapshot := make([]string, 0, len(keys)+len(c.verdicts))
	seen := make(map[string]struct{}, len(keys)+len(c.verdicts))
	for _, k := range keys {
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		snapshot = append(snapshot, k)
	}
	for k := range c.verdicts {
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		snapshot = append(snapshot, k)
	}
	c.mu.Unlock()
	go func() {
		var err error
		// End the flight from a defer so a panic in compute or onErr cannot wedge
		// the cache (see endFlight); recovery and release must land together.
		defer func() {
			if rec := recover(); rec != nil {
				// nil logger: worker.Report falls back to slog.Default().
				worker.Report(nil, "explorer-ttl-liveness-refresh", rec)
				err = errTTLRefreshPanicked
			}
			c.endFlight(fl, err)
		}()
		start := time.Now()
		rctx, cancel := context.WithTimeout(context.Background(), ttlVerdictRefreshTimeout)
		defer cancel()
		var verdicts map[string]TTLLiveness
		verdicts, err = c.compute(rctx, snapshot)
		obs.ObserveExplorerSWRRefresh("ttl_liveness", start, err)
		if err == nil {
			c.store(verdicts)
		} else if c.onErr != nil {
			// Keep the previous snapshot — stale verdicts of real state beat
			// none (and beat fail-open serving of possibly-archived pairs).
			c.onErr(err)
		}
	}()
	return fl
}
