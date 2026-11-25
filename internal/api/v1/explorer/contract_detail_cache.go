package explorer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// This file extends the stale-while-revalidate layer (hot_reads.go) to the
// per-contract detail reads: recent events, interactions, and code-history.
//
// Inline on the request budget these lake scans, whose cost scales with contract activity, time
// out for busy contracts, and a scan that dies with the request can never land warm. One
// (kind, contract, params) string key holding an `any` payload lets the differently-typed reads
// share one bounded cache and flight table.

// contractDetailTTL is when a served detail payload is refreshed (its staleness bound is [detailStaleAfterForKey]): five minutes
// is near-live for these views,
// while each recompute is a bloom-probed scan over the multi-billion-row contract_events table.
const contractDetailTTL = 5 * time.Minute

// contractDetailCacheMax bounds resident entries across ALL kinds; 1024 holds the hot set at three
// entries per actively-viewed contract.
const contractDetailCacheMax = 1024

// contractDetailRefreshTimeout bounds one detached recompute; it matches the contracts-directory
// budget because the table and scan class are the same.
const contractDetailRefreshTimeout = 3 * time.Minute

type contractDetailEntry struct {
	v        any
	cachedAt time.Time
	// refreshFailed: the last refresh of this entry failed or was skipped; only a put clears it.
	refreshFailed bool
}

// contractDetailCache is the shared TTL+bounded cache behind the three
// contract detail reads. Zero value ready to use, matching the siblings.
type contractDetailCache struct {
	mu      sync.Mutex
	entries map[string]contractDetailEntry
	// activity holds the "act:" keys apart, each map bounded on its own: account activity
	// computes in milliseconds, so a shared bound let cheap misses evict expensive contract entries.
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

// contractCodeHistoryTTL is the "ch:" key class's freshness window. The executable timeline is append-only and changes over months,
// while its key_xdr probe over ledger_entry_changes is the explorer's heaviest scan, so 24h allows one recompute per contract per day.
const contractCodeHistoryTTL = 24 * time.Hour

// positionsCacheKey is the key PREFIX (so the TTL and refresh class) for GET
// /v1/accounts/{g}/positions; the handler, detailTTLForKey and the tests must agree on it.
const positionsCacheKey = "pos:"

// accountPositionsTTL is the "pos:" class window, shorter than contractDetailTTL because positions are what an address holds now.
// One minute still collapses repeat loads onto one fan-out; past it the entry is served, labelled stale per [detailStaleAfterForKey].
const accountPositionsTTL = time.Minute

// detailTTLForKey maps a cache key to its freshness window by key-class prefix.
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

// detailStaleAfterForKey is how old a served entry may be before it is stale: its class TTL plus the refresh that the first
// expired read starts (at most one more TTL window, capped at the refresh budget). "pos:" balances are held now, so their
// grace is half a TTL (30s); no measured fan-out time justifies a longer one.
func detailStaleAfterForKey(key string) time.Duration {
	ttl := detailTTLForKey(key)
	if strings.HasPrefix(key, positionsCacheKey) {
		return ttl + min(ttl/2, contractDetailRefreshTimeout)
	}
	return ttl + min(ttl, contractDetailRefreshTimeout)
}

// servedStale reports whether e, served for key, is stale: past its bound, its last refresh failed, or it declared itself
// stale on arrival. An expired entry is clean only inside the bound while no refresh of it has failed.
func (e contractDetailEntry) servedStale(key string) bool {
	if p, ok := e.v.(provisionalPayload); ok && p.provisional() {
		return true
	}
	return e.refreshFailed || time.Since(e.cachedAt) > detailStaleAfterForKey(key)
}

// freshness is e's verdict served for key past its TTL ([servedFreshness]): a provisional payload, like a failed or overrun
// refresh, is degraded; age past [detailStaleAfterForKey] alone is only stale.
func (c *contractDetailCache) freshness(key string, e contractDetailEntry) servedFreshness {
	fr := expiredFreshness(e.servedStale(key), e.refreshFailed, c.flight.overran(key, contractDetailRefreshTimeout))
	if p, ok := e.v.(provisionalPayload); ok && p.provisional() {
		fr.degraded = true
	}
	return fr
}

// markRefreshFailed records on key's entry, if any, that its refresh failed or was skipped.
func (c *contractDetailCache) markRefreshFailed(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.tableLocked(key)
	if e, ok := m[key]; ok {
		e.refreshFailed = true
		m[key] = e
	}
}

// endContractDetailRefresh ends key's flight, first recording a failure on its entry so the next serve is flagged.
func (h *Handler) endContractDetailRefresh(key string, fl *keyFlight, err error) {
	if err != nil {
		h.contractDetail.markRefreshFailed(key)
	}
	h.contractDetail.flight.end(key, fl, err)
}

// get returns the entry for key even past the TTL (fresh=false), so a run of failed refreshes
// degrades to old-but-real data instead of 503s.
func (c *contractDetailCache) get(key string) (e contractDetailEntry, ok, fresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok = c.tableLocked(key)[key]
	if !ok {
		return contractDetailEntry{}, false, false
	}
	p, provisional := e.v.(provisionalPayload)
	return e, true, time.Since(e.cachedAt) <= detailTTLForKey(key) && (!provisional || !p.provisional())
}

// provisionalPayload is a cached payload that can declare itself stale on arrival (served, then refreshed on the next read).
type provisionalPayload interface{ provisional() bool }

func (c *contractDetailCache) put(key string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.tableLocked(key)
	if len(m) >= contractDetailCacheMax {
		evictOneLocked(m)
	}
	m[key] = contractDetailEntry{v: v, cachedAt: time.Now()}
}

// evictOneLocked drops the oldest entry already EXPIRED under its own key-class TTL, else the global-oldest: pure global-oldest starves
// the 24h "ch:" class, and attacker-mintable "pos:" keys make that trivial. Caller holds c.mu.
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

// detachedClassForKey maps a cache key to its refresh-gate CLASS by kind prefix. One shared class would starve a contract page, which fires
// detail, interactions and code-history concurrently against two slots; per-panel classes stop one burst monopolising the gate.
func detachedClassForKey(key string) string {
	prefix, _, ok := strings.Cut(key, ":")
	if !ok || prefix == "" {
		return "contract_detail"
	}
	return "contract_detail_" + prefix
}

// coldSlotWait caps how long a cold read waits for a refresh-gate slot, leaving an admitted compute half the read budget.
const coldSlotWait = explorerReadTimeout / 2

// slotAdmit claims a flight owner's refresh-gate slot; a non-nil error ends the flight with it, so it is what joiners see.
type slotAdmit func(*clickhouse.RefreshGate, string) error

// tryAdmit is the stale path's admission: an entry is served either way, so skip rather than wait.
func tryAdmit(g *clickhouse.RefreshGate, class string) error {
	if g.TryAcquireClass(class) {
		return nil
	}
	return errRefreshSaturated
}

// coldSlotAdmit is the cold-path admission shared by the cached reads. Called only by a flight's owner, it first bills the request
// [ratelimit.ColdFillCost] (refused: no slot claimed, refused holds why, the flight ends errOwnerRefused), then waits (bounded, see
// AcquireClassWait) for a slot until ctx or coldSlotWait ends.
type coldSlotAdmit struct {
	ctx     context.Context
	refused error
}

func (a *coldSlotAdmit) admit(g *clickhouse.RefreshGate, class string) error {
	if err := ratelimit.ChargeColdFill(a.ctx); err != nil {
		a.refused = err
		return errOwnerRefused
	}
	wctx, cancel := context.WithTimeout(a.ctx, coldSlotWait)
	defer cancel()
	if !g.AcquireClassWait(wctx, class) {
		return errRefreshSaturated
	}
	return nil
}

// coldFill starts or joins a cold fill through start and waits for it. A joined flight whose owner was refused its charge says
// nothing about this request's budget, so it is retried once with this request as candidate owner paying its own charge; a second
// refused owner answers errRefreshSaturated, so the retry cannot loop.
func coldFill(ctx context.Context, start func(slotAdmit) *keyFlight) (*keyFlight, error) {
	fl, err := coldFillOnce(ctx, start)
	if err != nil || !errors.Is(fl.err, errOwnerRefused) {
		return fl, err
	}
	if fl, err = coldFillOnce(ctx, start); err == nil && errors.Is(fl.err, errOwnerRefused) {
		return nil, errRefreshSaturated
	}
	return fl, err
}

func coldFillOnce(ctx context.Context, start func(slotAdmit) *keyFlight) (*keyFlight, error) {
	cold := &coldSlotAdmit{ctx: ctx}
	fl := start(cold.admit)
	if cold.refused != nil {
		return nil, cold.refused
	}
	select {
	case <-fl.done:
		return fl, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// refreshContractDetail kicks ONE detached compute for key (no-op while a flight is up) and returns the flight. Detached because
// a scan bound to a request deadline never completes for a busy contract, so the cache would never fill. admit claims the gate slot.
func (h *Handler) refreshContractDetail(key string, compute func(context.Context) (any, error), admit slotAdmit) *keyFlight {
	fl, owner := h.contractDetail.flight.begin(key)
	if !owner {
		return fl
	}
	// Bounded globally across keys because any shape-valid C-address is a distinct cold key (see detachedGate).
	gate := h.detachedGate()
	class := detachedClassForKey(key)
	if err := admit(gate, class); err != nil {
		h.endContractDetailRefresh(key, fl, err)
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
			h.endContractDetailRefresh(key, fl, err)
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

// contractDetailCached serves key from cache: an expired entry is served with its real asOf while a detached single-flight
// recompute runs, judged per [contractDetailCache.freshness]; a never-computed key waits only to the request deadline,
// and the compute outlives it so the retry lands warm.
func (h *Handler) contractDetailCached(ctx context.Context, key string, compute func(context.Context) (any, error)) (v any, asOf time.Time, fr servedFreshness, err error) {
	if e, ok, fresh := h.contractDetail.get(key); ok {
		if !fresh {
			// Stale path: an entry is served either way, so skip rather than wait.
			h.refreshContractDetail(key, compute, tryAdmit) //nolint:contextcheck // intentional detach — see refreshContractDetail
			if cur, ok, _ := h.contractDetail.get(key); ok {
				e = cur // a refresh skipped just now is already a known failure
			}
			fr = h.contractDetail.freshness(key, e)
		}
		return e.v, e.cachedAt, fr, nil
	}
	// Cold path: nothing to serve, so wait (bounded, see AcquireClassWait) for a slot rather than shed the page.
	fl, err := coldFill(ctx, func(admit slotAdmit) *keyFlight { //nolint:contextcheck // intentional detach — a request that times out must not kill the fill
		return h.refreshContractDetail(key, compute, admit)
	})
	if err != nil {
		return nil, time.Time{}, servedFreshness{}, err
	}
	if e, ok, _ := h.contractDetail.get(key); ok {
		return e.v, e.cachedAt, servedFreshness{}, nil
	}
	if fl.err != nil {
		return nil, time.Time{}, servedFreshness{}, fl.err
	}
	return nil, time.Time{}, servedFreshness{}, errRefreshFailed
}
