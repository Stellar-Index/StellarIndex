package explorer

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// This file extends the stale-while-revalidate layer (hot_reads.go) to the
// per-contract detail reads: recent events (the /v1/contracts/{id} first
// page), interactions, and code-history.
//
// Why (route-sweep 2026-07-30): all three ran INLINE on the request's 8s
// budget over lake scans whose cost is set by the contract's activity, so a
// busy contract (the sweep's Phoenix SAC among them) timed out on EVERY
// request — and because the scan died WITH the request, no retry could ever
// land warm. That is exactly the failure shape assetHoldersCached fixed for
// the holders board; this is the same mechanism generalised over a
// (kind, contract, params) string key holding an `any` payload, so three
// differently-typed reads share one bounded cache + flight table instead of
// three copies of it.

// contractDetailTTL bounds how stale a served detail payload may be. Five
// minutes on a per-contract activity/interaction/code view is materially
// indistinguishable from live (code upgrades are rare events; the events
// first page moves per ledger but tolerates minutes exactly like the
// opTypeStats panel it shadows), while each recompute is a bloom-probed
// scan over the multi-billion-row contract_events table.
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
	flight  perKeyFlight
}

// contractCodeHistoryTTL is the "ch:" key class's freshness window
// (inventory #3 / sub-second goal 2026-08-08). A contract's executable
// timeline is append-only and changes on the order of MONTHS (an
// in-place upgrade), while its backing read is the heaviest per-request
// scan in the explorer (the key_xdr probe over ledger_entry_changes,
// 8s class) — recomputing it every 5 minutes per visited contract was
// pure background burn. 24h keeps at most one recompute per contract
// per day; a brand-new upgrade appears within a day, which matches the
// page's actual freshness need.
const contractCodeHistoryTTL = 24 * time.Hour

// detailTTLForKey maps a cache key to its freshness window — 5 minutes
// for the live-ish classes (detail events, interactions, activity), 24h
// for the near-immutable code-history timeline.
func detailTTLForKey(key string) time.Duration {
	if strings.HasPrefix(key, "ch:") {
		return contractCodeHistoryTTL
	}
	return contractDetailTTL
}

// get returns the entry for key whenever one exists — INCLUDING past the
// TTL (fresh=false); staleness is the caller's judgment, so a run of
// failed refreshes degrades to old-but-real data instead of 503s.
func (c *contractDetailCache) get(key string) (e contractDetailEntry, ok, fresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok = c.entries[key]
	if !ok {
		return contractDetailEntry{}, false, false
	}
	return e, true, time.Since(e.cachedAt) <= detailTTLForKey(key)
}

func (c *contractDetailCache) put(key string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]contractDetailEntry)
	}
	if len(c.entries) >= contractDetailCacheMax {
		var oldestKey string
		var oldestAt time.Time
		for k, e := range c.entries {
			if oldestKey == "" || e.cachedAt.Before(oldestAt) {
				oldestKey, oldestAt = k, e.cachedAt
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[key] = contractDetailEntry{v: v, cachedAt: time.Now()}
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
	if !gate.TryAcquire() {
		h.contractDetail.flight.end(key, fl, errRefreshSaturated)
		return fl
	}
	go func() {
		defer gate.Release()
		start := time.Now()
		rctx, cancel := context.WithTimeout(context.Background(), contractDetailRefreshTimeout)
		defer cancel()
		v, err := compute(rctx)
		obs.ObserveExplorerSWRRefresh("contract_detail", start, err)
		if err != nil {
			// Keep the previous entry (if any) — old-but-real beats blank.
			h.Logger.Warn("contract detail detached refresh failed", "key", key, "err", err)
			h.contractDetail.flight.end(key, fl, err)
			return
		}
		h.contractDetail.put(key, v)
		h.contractDetail.flight.end(key, fl, nil)
	}()
	return fl
}

// contractDetailCached serves key from the cache: fresh entry as-is; STALE
// entry served (degraded=true, with its real asOf) while a detached
// single-flight recompute runs; a never-computed key waits for the
// detached compute up to the REQUEST deadline only (the compute itself
// outlives it, so a heavy contract 503s this one request and the retry
// lands warm).
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
