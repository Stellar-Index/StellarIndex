package clickhouse

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TTLVerdictCacheTTL bounds how old a served archived-pair verdict set may
// be before a background re-classification is kicked. The verdict answers
// "has this pool's instance entry's TTL lapsed" — state that moves on
// day/week scales (an archival or a restore, not per-ledger churn) — so
// tens of minutes of staleness is materially indistinguishable from live,
// while each recompute is a scan of the ~586M-row ttl prefix computing
// base64Decode per row (route-sweep 2026-07-29: running it INLINE per
// request is what held GET /v1/pools/reserves in the 8s-budget 503 class;
// the durable schema fix is the planned slim ttl_live_until projection
// table, v0.21.4 — this cache is the serving-layer fix that works today).
const TTLVerdictCacheTTL = 30 * time.Minute

// ttlVerdictRefreshTimeout bounds one detached re-classification. The scan
// measured minutes-class under load; generous so a refresh that would have
// succeeded isn't abandoned, bounded so a wedged one can't pin the
// single-flight forever.
const ttlVerdictRefreshTimeout = 5 * time.Minute

// ttlLivenessCache is a stale-while-revalidate snapshot of per-key TTL
// liveness verdicts, fronting [ClassifyTTLLiveness] for the request-path
// reader (SoroswapPairReserves).
//
// States:
//
//   - fresh snapshot: verdicts served from memory.
//   - stale snapshot (or new keys appeared): served anyway — missing keys
//     read as TTLUnknown, which callers KEEP per the ClassifyTTLLiveness
//     contract (fail-open: a brand-new pair is almost certainly live) —
//     while ONE detached recompute runs.
//   - never filled: the same DETACHED compute is kicked and the caller
//     waits for it bounded by its OWN deadline only — the compute runs on
//     the cache's ttlVerdictRefreshTimeout budget and outlives any caller
//     that gives up, so the fill lands regardless and the retry (or the
//     DEXTVLCache background refresher, which calls the same reader at
//     startup and every 10 min) serves warm.
//
// compute is injected so the cache logic is unit-testable without a
// ClickHouse server; the production wiring closes over the reader's conn.
type ttlLivenessCache struct {
	mu        sync.Mutex
	verdicts  map[string]TTLLiveness
	fetchedAt time.Time
	flight    *ttlFlight

	compute func(ctx context.Context, keys []string) (map[string]TTLLiveness, error)
	// onErr, when set, observes detached-refresh failures (a persistently
	// failing refresh pins the snapshot stale — same visibility rationale
	// as wealthRefreshErr). Optional.
	onErr func(error)
}

// ttlFlight is one in-flight classification. done closes when it finishes;
// err carries its failure (nil on success), readable only AFTER done closes
// (the close is the happens-before edge).
type ttlFlight struct {
	done chan struct{}
	err  error
}

func newTTLLivenessCache(compute func(ctx context.Context, keys []string) (map[string]TTLLiveness, error)) *ttlLivenessCache {
	return &ttlLivenessCache{compute: compute}
}

// resolve returns the liveness verdict for every requested key. Nil-safe
// (a zero-value reader in tests degrades to computing inline... which a
// nil compute skips by reporting everything unknown).
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

// fromSnapshot serves the requested keys from a filled snapshot.
// filled=false when nothing was ever stored; needsRefresh=true when the
// snapshot is past its TTL or lacks some requested key (the CALLER kicks
// the detached recompute — kept out of this read-only helper so the
// intentional-detach point stays a single annotated call site).
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
// CALLER's deadline only — the compute itself runs on the cache's own
// budget and outlives a caller that gives up, so the fill still lands and
// a retry serves warm.
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

// kickRefresh starts ONE detached recompute for the given key snapshot
// (returning the existing flight while one is up). The key set is the
// caller's full current registry-derived set, so the refreshed snapshot
// covers exactly the keys being served.
func (c *ttlLivenessCache) kickRefresh(keys []string) *ttlFlight {
	c.mu.Lock()
	if c.flight != nil {
		fl := c.flight
		c.mu.Unlock()
		return fl
	}
	fl := &ttlFlight{done: make(chan struct{})}
	c.flight = fl
	c.mu.Unlock()

	snapshot := make([]string, len(keys))
	copy(snapshot, keys)
	go func() {
		start := time.Now()
		rctx, cancel := context.WithTimeout(context.Background(), ttlVerdictRefreshTimeout)
		defer cancel()
		verdicts, err := c.compute(rctx, snapshot)
		obs.ObserveExplorerSWRRefresh("ttl_liveness", start, err)
		if err == nil {
			c.store(verdicts)
		} else if c.onErr != nil {
			// Keep the previous snapshot — stale verdicts of real state beat
			// none (and beat fail-open serving of possibly-archived pairs).
			c.onErr(err)
		}
		c.mu.Lock()
		c.flight = nil
		c.mu.Unlock()
		fl.err = err
		close(fl.done)
	}()
	return fl
}
