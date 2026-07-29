package explorer

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// This file holds the bounded-TTL + single-flight layer in front of the
// two UNAUTHENTICATED explorer reads whose cost is set by the size of
// the lake rather than by the request: GET /v1/assets/{asset_id}/holders
// and the GET /v1/contracts directory (C3-002 + C3-009,
// audit-2026-07-23).
//
// Both endpoints ran their scans on every request, over the shared
// 8-connection explorer pool, with no credential required — so a single
// client looping either of them could hold every connection and stall
// every lake-backed endpoint behind it, which the per-request
// explorerReadTimeout bounds but does not prevent. The shape of the fix
// is the one already in the tree for the same class of read:
//
//   - a short TTL cache keyed on the query's expensive dimension, with a
//     bounded entry count (accountStateCache, opsDirCache, opTypeStatsCache);
//   - a per-key single-flight so a burst of concurrent misses for the
//     same key launches ONE scan, not N (AccountStateCached);
//   - the `limit` argument deliberately excluded from the cache key —
//     one warm entry holds the maximum page and every request size slices
//     from it, exactly as AccountsByWealthCached documents ("a single
//     warm entry covers all traffic — the limit argument is not a cache
//     key"). The aggregation cost is set by the scan, not by LIMIT.
//
// Refresh model (route-sweep 2026-07-29 — both endpoints were in the
// 8s-budget 503 class, so "the request path may fill the cache" stopped
// being true on the post-D3 part layout): the underlying scans now run
// DETACHED from any request context, on their own bounded budget, and the
// request path only ever (a) serves a fresh entry, (b) serves a STALE
// entry — 200 + flags.stale + the entry's real as_of — while a
// single-flight detached refresh runs, or (c) on a stone-cold key, waits
// for the detached compute up to its own deadline (fast keys fill within
// it; a key whose scan outlives the request 503s THIS request but the
// compute keeps running, so the retry lands warm). A failed refresh keeps
// the previous entry — old-but-real beats blank. PrewarmContractsDirectory
// keeps the directory's default rung permanently warm off the API's
// 5-minute prewarm loop.

// perKeyFlight collapses concurrent work for the same key. Derived from the
// clickhouse-package helper of the same name (this package cannot import an
// unexported type); this copy additionally carries the flight's ERROR so a
// cold-path waiter can serve the compute's real failure (a deadline maps to
// the 503 timeout contract, C-F1) instead of a shapeless sentinel.
type perKeyFlight struct {
	mu      sync.Mutex
	inGoing map[string]*keyFlight
}

// keyFlight is one in-flight compute. done closes when it finishes; err
// carries its failure (nil on success) and is readable only AFTER done
// closes — the close is the happens-before edge.
type keyFlight struct {
	done chan struct{}
	err  error
}

// begin returns the flight for key and whether the caller owns it. Owners
// must call end (with the compute's error); waiters select on done. Zero
// value ready to use.
func (f *perKeyFlight) begin(key string) (*keyFlight, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fl, ok := f.inGoing[key]; ok {
		return fl, false
	}
	if f.inGoing == nil {
		f.inGoing = make(map[string]*keyFlight)
	}
	fl := &keyFlight{done: make(chan struct{})}
	f.inGoing[key] = fl
	return fl, true
}

func (f *perKeyFlight) end(key string, fl *keyFlight, err error) {
	f.mu.Lock()
	delete(f.inGoing, key)
	f.mu.Unlock()
	fl.err = err
	close(fl.done)
}

// assetHoldersTTL bounds how stale a cached holders board may be.
// Trustline balances move continuously, but a "top holders" board
// tolerates a minute of staleness, and the read behind it is TWO FINAL
// scans of the 43.6M-row current-state table. Same trade the 30s
// AccountStateCacheTTL makes on the sibling detail read, doubled because
// this one is twice the scans and its output changes more slowly.
const assetHoldersTTL = 60 * time.Second

// assetHoldersCacheMax bounds resident entries. Holder boards are
// long-tail — a handful of hot assets (USDC, yXLM, the big issuers) plus
// a churn of one-off lookups — so a modest cap holds the hot set while
// capping memory. On overflow the oldest entry is evicted.
const assetHoldersCacheMax = 512

// assetHoldersMaxLimit is the page size every cached entry holds. It
// matches the handler's own ParseLimit ceiling, so a cached entry can
// serve any accepted `?limit=` by slicing.
const assetHoldersMaxLimit = 500

// assetHoldersRefreshTimeout bounds one detached holders scan. Well above
// the observed cost for even the largest asset's two FINAL scans, bounded
// so a wedged query can't pin the single-flight forever.
const assetHoldersRefreshTimeout = 90 * time.Second

type assetHoldersEntry struct {
	holders  []clickhouse.AssetHolder
	total    int64
	cachedAt time.Time
}

// assetHoldersCache is the TTL+bounded cache behind
// GET /v1/assets/{asset_id}/holders. Zero value ready to use (map
// lazily created), matching opsDirCache.
type assetHoldersCache struct {
	mu      sync.Mutex
	entries map[string]assetHoldersEntry
	flight  perKeyFlight
}

// get returns the entry for asset whenever one exists — INCLUDING past the
// TTL (fresh=false); staleness is the caller's judgment, so a run of failed
// refreshes degrades to old-but-real data instead of 503s. ok=false only
// when the asset was never computed.
func (c *assetHoldersCache) get(asset string) (e assetHoldersEntry, ok, fresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok = c.entries[asset]
	if !ok {
		return assetHoldersEntry{}, false, false
	}
	return e, true, time.Since(e.cachedAt) <= assetHoldersTTL
}

func (c *assetHoldersCache) put(asset string, e assetHoldersEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]assetHoldersEntry)
	}
	if len(c.entries) >= assetHoldersCacheMax {
		// Evict the oldest entry (approximate LRU — cheap: one pass, and
		// only when at capacity). Mirrors accountStateCache.put.
		var oldestKey string
		var oldestAt time.Time
		for k, v := range c.entries {
			if oldestKey == "" || v.cachedAt.Before(oldestAt) {
				oldestKey, oldestAt = k, v.cachedAt
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[asset] = e
}

// errRefreshFailed is the sentinel a stale-while-revalidate wait path
// returns when the detached compute finished without producing a cacheable
// entry (the underlying error was already logged by the refresh goroutine).
var errRefreshFailed = errors.New("explorer: detached cache refresh produced no entry")

// refreshAssetHolders kicks ONE detached holders scan for asset (no-op when
// a flight is already up) and returns the flight to optionally wait on.
// Detached on purpose: bound to a request's 8s deadline the scan for a huge
// asset never completed, so the cache never filled and every request kept
// paying the timeout (same failure shape as site-audit S3's wealth
// ranking).
func (h *Handler) refreshAssetHolders(asset string) *keyFlight {
	fl, owner := h.assetHolders.flight.begin(asset)
	if !owner {
		return fl
	}
	go func() {
		start := time.Now()
		rctx, cancel := context.WithTimeout(context.Background(), assetHoldersRefreshTimeout)
		defer cancel()
		holders, total, err := h.Reader.AssetHolders(rctx, asset, assetHoldersMaxLimit)
		obs.ObserveExplorerSWRRefresh("asset_holders", start, err)
		if err != nil {
			// Keep the previous entry (if any) — old-but-real beats blank.
			h.Logger.Warn("asset holders detached refresh failed", "asset", asset, "err", err)
			h.assetHolders.flight.end(asset, fl, err)
			return
		}
		h.assetHolders.put(asset, assetHoldersEntry{holders: holders, total: total, cachedAt: time.Now()})
		h.assetHolders.flight.end(asset, fl, nil)
	}()
	return fl
}

// assetHoldersCached serves the holders board for `asset` from the cache.
// fresh entry → served as-is; STALE entry → served (degraded=true, with its
// real asOf) while a detached single-flight rescan runs; never-computed →
// wait for the detached scan up to the request deadline (fast assets fill
// well within it; a huge asset 503s THIS request but the scan keeps
// running, so a retry lands warm). The returned slice is capped at `limit`;
// the total holder count is limit-independent.
func (h *Handler) assetHoldersCached(ctx context.Context, asset string, limit int) (holders []clickhouse.AssetHolder, total int64, asOf time.Time, degraded bool, err error) {
	if e, ok, fresh := h.assetHolders.get(asset); ok {
		if !fresh {
			h.refreshAssetHolders(asset) //nolint:contextcheck // intentional detach — the rescan must outlive this request (see refreshAssetHolders)
		}
		return sliceHolders(e.holders, limit), e.total, e.cachedAt, !fresh, nil
	}
	// Stone-cold: wait for the detached compute, bounded by OUR deadline
	// only — the compute itself is not.
	fl := h.refreshAssetHolders(asset) //nolint:contextcheck // intentional detach — a request that times out must not kill the fill (see refreshAssetHolders)
	select {
	case <-fl.done:
		if e, ok, _ := h.assetHolders.get(asset); ok {
			return sliceHolders(e.holders, limit), e.total, e.cachedAt, false, nil
		}
		if fl.err != nil {
			return nil, 0, time.Time{}, false, fl.err
		}
		return nil, 0, time.Time{}, false, errRefreshFailed
	case <-ctx.Done():
		return nil, 0, time.Time{}, false, ctx.Err()
	}
}

func sliceHolders(rows []clickhouse.AssetHolder, limit int) []clickhouse.AssetHolder {
	if limit > 0 && len(rows) > limit {
		return rows[:limit]
	}
	return rows
}

// contractsDirTTL bounds how stale the cached contracts directory may
// be. Deliberately much longer than the 3s opsDirTTL, for the same
// reason opTypeStatsTTL is 5 minutes: this is an aggregate over a
// MULTI-DAY window (30 by default), so second-level precision on it is
// meaningless — and each recompute is a GROUP BY over that window of the
// billions-row contract_events table.
const contractsDirTTL = 5 * time.Minute

// contractsWindowLadder is the set of windows GET /v1/contracts actually
// aggregates over. A caller's `?days=` is rounded UP to the next rung
// (365 is the last), and the served window is echoed back in
// `window_days` / `since_ledger`.
//
// Without the ladder the endpoint accepted 365 distinct window sizes,
// each a separate multi-day GROUP BY — so a cache keyed on the raw value
// bounds a repeat caller but not an adversary, who just walks
// `?days=1..365` and gets 365 full-price scans. Quantising collapses
// that to at most len(ladder) distinct queries, which the TTL cache then
// absorbs. Rounding UP rather than down keeps the response a SUPERSET of
// what the caller asked for: a client asking for 45 days is never shown
// less than 45 days of activity.
var contractsWindowLadder = []int{1, 7, 30, 90, 365}

// contractsWindow rounds a requested day count up to the ladder.
// Callers must have already range-checked days into [1, 365].
func contractsWindow(days int) int {
	for _, rung := range contractsWindowLadder {
		if days <= rung {
			return rung
		}
	}
	return contractsWindowLadder[len(contractsWindowLadder)-1]
}

// contractsDirMaxLimit is the page size every cached entry holds —
// the handler's own ParseLimit ceiling, so one entry serves any
// accepted `?limit=`.
const contractsDirMaxLimit = 500

// contractsDirRefreshTimeout bounds one detached directory aggregation.
// The 365d rung is a GROUP BY over a year of contract_events — minutes-
// class under load; generous so a refresh that would have succeeded isn't
// abandoned, bounded so a wedged one can't pin the single-flight forever.
const contractsDirRefreshTimeout = 3 * time.Minute

type contractsDirEntry struct {
	rows     []clickhouse.ContractDirectoryRow
	since    uint32
	cachedAt time.Time
}

// contractsDirCache is the TTL cache behind the GET /v1/contracts
// directory, keyed on the ladder-quantised window. At most
// len(contractsWindowLadder) entries, so it needs no eviction pass.
// Zero value ready to use.
type contractsDirCache struct {
	mu      sync.Mutex
	entries map[int]contractsDirEntry
	flight  perKeyFlight
}

// get returns the entry for window whenever one exists — including past
// the TTL (fresh=false); same stale-serving contract as assetHoldersCache.
func (c *contractsDirCache) get(window int) (e contractsDirEntry, ok, fresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok = c.entries[window]
	if !ok {
		return contractsDirEntry{}, false, false
	}
	return e, true, time.Since(e.cachedAt) <= contractsDirTTL
}

func (c *contractsDirCache) put(window int, e contractsDirEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[int]contractsDirEntry)
	}
	c.entries[window] = e
}

// refreshContractsDir kicks ONE detached directory aggregation for the
// ladder rung `window` (no-op when a flight is already up) and returns the
// flight channel to optionally wait on. Detached for the same reason as
// refreshAssetHolders: the multi-day GROUP BY cannot be relied on to fit a
// request deadline, and dying with the request left the cache cold forever.
func (h *Handler) refreshContractsDir(window int) *keyFlight {
	key := strconv.Itoa(window)
	fl, owner := h.contractsDir.flight.begin(key)
	if !owner {
		return fl
	}
	go func() {
		start := time.Now()
		rctx, cancel := context.WithTimeout(context.Background(), contractsDirRefreshTimeout)
		defer cancel()
		since := h.windowFloorLedger(rctx, window)
		rows, err := h.Reader.RecentContracts(rctx, contractsDirMaxLimit, since)
		obs.ObserveExplorerSWRRefresh("contracts_dir", start, err)
		if err != nil {
			// Keep the previous entry (if any) — old-but-real beats blank.
			h.Logger.Warn("contracts directory detached refresh failed", "window_days", window, "err", err)
			h.contractsDir.flight.end(key, fl, err)
			return
		}
		h.contractsDir.put(window, contractsDirEntry{rows: rows, since: since, cachedAt: time.Now()})
		h.contractsDir.flight.end(key, fl, nil)
	}()
	return fl
}

// recentContractsCached serves the contracts directory for the
// ladder-quantised `window`: fresh entry as-is; STALE entry served
// (degraded=true, with its real asOf) while a detached single-flight
// re-aggregation runs; a never-computed rung waits for the detached
// compute up to the request deadline (the prewarm loop keeps the default
// rung permanently warm, so this is cold-start + non-default rungs only).
// Returns the rows (capped at `limit`) plus the ledger floor the cached
// aggregate used, so the response's `since_ledger` describes the data
// actually served rather than a floor recomputed after the fact.
func (h *Handler) recentContractsCached(ctx context.Context, window, limit int) (rows []clickhouse.ContractDirectoryRow, since uint32, asOf time.Time, degraded bool, err error) {
	if e, ok, fresh := h.contractsDir.get(window); ok {
		if !fresh {
			h.refreshContractsDir(window) //nolint:contextcheck // intentional detach — see refreshContractsDir
		}
		return sliceContracts(e.rows, limit), e.since, e.cachedAt, !fresh, nil
	}
	fl := h.refreshContractsDir(window) //nolint:contextcheck // intentional detach — see refreshContractsDir
	select {
	case <-fl.done:
		if e, ok, _ := h.contractsDir.get(window); ok {
			return sliceContracts(e.rows, limit), e.since, e.cachedAt, false, nil
		}
		if fl.err != nil {
			return nil, 0, time.Time{}, false, fl.err
		}
		return nil, 0, time.Time{}, false, errRefreshFailed
	case <-ctx.Done():
		return nil, 0, time.Time{}, false, ctx.Err()
	}
}

// PrewarmContractsDirectory primes the /v1/contracts directory's DEFAULT
// ladder rung (30d — what the explorer UI requests) so no user meets the
// cold state. Called from the API's 5-minute prewarm loop
// (cmd/stellarindex-api/main.go), which matches contractsDirTTL, so the
// rung stays permanently fresh. Kicks the same detached single-flight
// refresh the request path uses and returns immediately when it is already
// warm; ctx only gates whether the kick happens at shutdown.
func (h *Handler) PrewarmContractsDirectory(ctx context.Context) {
	if h.Reader == nil || ctx.Err() != nil {
		return
	}
	if _, _, fresh := h.contractsDir.get(contractsWindow(30)); fresh {
		return
	}
	h.refreshContractsDir(contractsWindow(30)) //nolint:contextcheck // intentional detach — prewarm kicks the same background compute
}

func sliceContracts(rows []clickhouse.ContractDirectoryRow, limit int) []clickhouse.ContractDirectoryRow {
	if limit > 0 && len(rows) > limit {
		return rows[:limit]
	}
	return rows
}
