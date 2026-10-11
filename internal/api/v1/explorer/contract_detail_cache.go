package explorer

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// This file extends the stale-while-revalidate layer (hot_reads.go) to the
// per-contract detail reads: recent events, interactions, and code-history.
//
// Inline on the request budget these lake scans, whose cost scales with contract activity, time
// out for busy contracts, and a scan that dies with the request can never land warm. One
// (kind, contract, params) string key holding an `any` payload lets the differently-typed reads
// share one bounded cache and flight table.

// contractDetailTTL bounds how stale a served detail payload may be. Five minutes is close enough
// to live for these views, while each recompute is a bloom-probed scan over the
// multi-billion-row contract_events table.
const contractDetailTTL = 5 * time.Minute

// contractDetailCacheMax bounds resident entries across ALL kinds. Detail
// traffic is long-tail like holders boards; 1024 holds the hot set at
// three entries per actively-viewed contract.
const contractDetailCacheMax = 1024

// contractDetailRefreshTimeout bounds one detached recompute. Matches the
// contracts-directory budget: same table, same scan class, and the
// measured worst case there (55.7 s with spill) leaves ample headroom.
const contractDetailRefreshTimeout = 3 * time.Minute

type contractDetailEntry struct {
	v        any
	cachedAt time.Time
}

// contractDetailCache is the shared TTL+bounded cache behind the three
// contract detail reads. Zero value ready to use, matching the siblings.
type contractDetailCache struct {
	mu      sync.Mutex
	entries map[string]contractDetailEntry
	// activity holds the "act:" keys apart, each map bounded on its own:
	// a random account's activity computes in milliseconds, so sharing one
	// bound let cheap account misses evict expensive contract entries.
	activity map[string]contractDetailEntry
	flight   perKeyFlight
}

// accountActivityCacheKey is the key prefix of GET /v1/accounts/{g}/activity.
const accountActivityCacheKey = "act:"

// tableLocked returns (lazily creating) the map holding key. Caller holds c.mu.
func (c *contractDetailCache) tableLocked(key string) map[string]contractDetailEntry {
	if strings.HasPrefix(key, accountActivityCacheKey) {
		if c.activity == nil {
			c.activity = make(map[string]contractDetailEntry)
		}
		return c.activity
	}
	if c.entries == nil {
		c.entries = make(map[string]contractDetailEntry)
	}
	return c.entries
}

// contractCodeHistoryTTL is the "ch:" key class's freshness window. The executable timeline is
// append-only and changes on the order of months, while its read (the key_xdr probe over
// ledger_entry_changes) is the heaviest per-request scan in the explorer, so 24h allows at most
// one recompute per contract per day.
const contractCodeHistoryTTL = 24 * time.Hour

// positionsCacheKey is the key PREFIX (and therefore the TTL + refresh
// class) for GET /v1/accounts/{g}/positions. Named rather than inlined
// because three files have to agree on it: the handler, detailTTLForKey
// below, and the tests that seed an entry.
const positionsCacheKey = "pos:"

// accountPositionsTTL is the "pos:" key class's freshness window. Deliberately shorter than
// contractDetailTTL: positions describe what an address holds right now, and a fresh deposit
// should not be hidden for five minutes. One minute still collapses repeat loads and concurrent
// visitors onto a single six-fold fan-out. Past it the entry is served stale-but-labelled, so
// its age is bounded by the refresh succeeding, not by this constant alone.
const accountPositionsTTL = time.Minute

// detailTTLForKey maps a cache key to its freshness window — 5 minutes
// for the live-ish classes (detail events, interactions, activity), 1
// minute for account positions, 24h for the near-immutable code-history
// timeline.
func detailTTLForKey(key string) time.Duration {
	switch {
	case strings.HasPrefix(key, "ch:"):
		return contractCodeHistoryTTL
	case strings.HasPrefix(key, positionsCacheKey):
		return accountPositionsTTL
	default:
		return contractDetailTTL
	}
}

// get returns the entry for key whenever one exists — INCLUDING past the
// TTL (fresh=false); staleness is the caller's judgment, so a run of
// failed refreshes degrades to old-but-real data instead of 503s.
func (c *contractDetailCache) get(key string) (e contractDetailEntry, ok, fresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok = c.tableLocked(key)[key]
	if !ok {
		return contractDetailEntry{}, false, false
	}
	return e, true, time.Since(e.cachedAt) <= detailTTLForKey(key)
}

func (c *contractDetailCache) put(key string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.tableLocked(key)
	if len(m) >= contractDetailCacheMax {
		evictOneLocked(m)
	}
	m[key] = contractDetailEntry{v: v, cachedAt: time.Now()}
}

// evictOneLocked drops one entry to make room for a new key. It prefers
// the oldest entry that has ALREADY EXPIRED under its own key-class TTL,
// falling back to the map's global-oldest cachedAt only when nothing has
// expired yet.
//
// Pure global-oldest eviction starves the "ch:" (code-history) class: with a 24h TTL its cachedAt
// is touched once a day while "ev:"/"ix:"/"pos:" entries keep resetting theirs, so a hot
// code-history entry is always the oldest and evicted first. Attacker-mintable "pos:" keys (any
// checksum-valid strkey is a new cold key) make that churn trivial. Preferring an expired entry
// makes the short-TTL classes that cause the pressure absorb it. Caller holds c.mu.
func evictOneLocked(entries map[string]contractDetailEntry) {
	now := time.Now()
	var oldestExpiredKey, oldestKey string
	var oldestExpiredAt, oldestAt time.Time
	for k, e := range entries {
		if oldestKey == "" || e.cachedAt.Before(oldestAt) {
			oldestKey, oldestAt = k, e.cachedAt
		}
		if now.Sub(e.cachedAt) <= detailTTLForKey(k) {
			continue
		}
		if oldestExpiredKey == "" || e.cachedAt.Before(oldestExpiredAt) {
			oldestExpiredKey, oldestExpiredAt = k, e.cachedAt
		}
	}
	if oldestExpiredKey != "" {
		delete(entries, oldestExpiredKey)
		return
	}
	delete(entries, oldestKey)
}

// detachedClassForKey maps a cache key to its refresh-gate CLASS, using
// the key's kind prefix ("ev:", "ix:", "ch:", "act:", "inst:").
//
// The per-class cap stops one class starving the others, but a single shared class (capped at
// half the global limit) makes a contract page starve itself: it fires detail, interactions and
// code-history concurrently, so three or four refreshes contend for two slots and the losers 503.
// Keying per panel keeps one panel's burst from monopolising the gate without a page's own
// fan-out competing with itself.
func detachedClassForKey(key string) string {
	prefix, _, ok := strings.Cut(key, ":")
	if !ok || prefix == "" {
		return "contract_detail"
	}
	return "contract_detail_" + prefix
}

// refreshContractDetail kicks ONE detached compute for key (no-op while a
// flight is up) and returns the flight to optionally wait on. Detached for
// the same reason as every sibling: bound to a request deadline, the scan
// for a busy contract never completed and the cache never filled.
func (h *Handler) refreshContractDetail(key string, compute func(context.Context) (any, error)) *keyFlight {
	fl, owner := h.contractDetail.flight.begin(key)
	if !owner {
		return fl
	}
	// Bounded globally across keys — contract ids are attacker-mintable
	// (any shape-valid C-address is a distinct cold key); on saturation
	// skip, don't queue (see detachedGate).
	gate := h.detachedGate()
	class := detachedClassForKey(key)
	if !gate.TryAcquireClass(class) {
		h.contractDetail.flight.end(key, fl, errRefreshSaturated)
		return fl
	}
	go func() {
		defer gate.ReleaseClass(class)
		// End the flight from this defer on every path, panic included: an unended flight wedges
		// the key permanently (begin() keeps handing out the dead flight), and end() cannot be
		// called twice because it closes fl.done.
		var err error
		defer func() {
			if rec := recover(); rec != nil {
				worker.Report(h.Logger, "explorer-contract-detail-refresh", rec)
				err = errRefreshPanicked
			}
			h.contractDetail.flight.end(key, fl, err)
		}()
		start := time.Now()
		rctx, cancel := context.WithTimeout(context.Background(), contractDetailRefreshTimeout)
		defer cancel()
		v, err := compute(rctx)
		obs.ObserveExplorerSWRRefresh("contract_detail", start, err)
		if err != nil {
			// Keep the previous entry (if any) — old-but-real beats blank.
			h.Logger.Warn("contract detail detached refresh failed", "key", key, "err", err)
			return
		}
		h.contractDetail.put(key, v)
	}()
	return fl
}

// contractDetailCached serves key from the cache: a stale entry is served (degraded=true, real
// asOf) while a detached single-flight recompute runs; a never-computed key waits only to the
// request deadline, and the compute outlives it so the retry lands warm.
func (h *Handler) contractDetailCached(ctx context.Context, key string, compute func(context.Context) (any, error)) (v any, asOf time.Time, degraded bool, err error) {
	if e, ok, fresh := h.contractDetail.get(key); ok {
		if !fresh {
			h.refreshContractDetail(key, compute) //nolint:contextcheck // intentional detach — see refreshContractDetail
		}
		return e.v, e.cachedAt, !fresh, nil
	}
	fl := h.refreshContractDetail(key, compute) //nolint:contextcheck // intentional detach — a request that times out must not kill the fill
	select {
	case <-fl.done:
		if e, ok, _ := h.contractDetail.get(key); ok {
			return e.v, e.cachedAt, false, nil
		}
		if fl.err != nil {
			return nil, time.Time{}, false, fl.err
		}
		return nil, time.Time{}, false, errRefreshFailed
	case <-ctx.Done():
		return nil, time.Time{}, false, ctx.Err()
	}
}
