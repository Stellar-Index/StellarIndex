package explorer

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// This file holds the bounded-TTL + single-flight layer in front of the two UNAUTHENTICATED
// explorer reads whose cost is set by the size of the lake, not the request: GET
// /v1/assets/{asset_id}/holders and the GET /v1/contracts directory. A client looping either
// would otherwise hold every connection of the shared explorer pool.
//
// Entries are keyed on the query's expensive dimension, never on `limit`: one warm entry holds
// the maximum page and every request size slices from it. Per-key single-flight means a burst
// of misses launches ONE scan.
//
// Both scans can outlast the 8s request budget, so they run DETACHED from any request context.
// A request serves a fresh entry, or a STALE one (200 + flags.stale + the entry's real as_of)
// while a refresh runs, or on a stone-cold key waits up to its own deadline and then 503s while
// the compute continues, so the retry lands warm. A failed refresh keeps the previous entry.

// perKeyFlight collapses concurrent work for one key. Unlike the clickhouse-package helper it copies, it carries the flight's ERROR,
// so a cold-path waiter serves the compute's real failure (a deadline maps to the 503 contract) instead of a shapeless sentinel.
type perKeyFlight struct {
	mu      sync.Mutex
	inGoing map[string]*keyFlight
}

// keyFlight is one in-flight compute. done closes when it finishes; err (nil on success) is readable only AFTER done closes,
// which is the happens-before edge.
type keyFlight struct {
	done chan struct{}
	err  error
}

// begin returns the flight for key and whether the caller owns it. Owners must call end (with the compute's error); waiters select on done.
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

// assetHoldersTTL bounds how stale a cached holders board may be: a minute of staleness is fine for "top holders", and the read is TWO
// FINAL scans of the large current-state table. Double the 30s AccountStateCacheTTL: twice the scans, slower-changing.
const assetHoldersTTL = 60 * time.Second

// assetHoldersCacheMax bounds resident entries: boards are long-tail (a few hot assets plus
// one-off lookups), so a modest cap holds the hot set. On overflow the oldest entry is evicted.
const assetHoldersCacheMax = 512

// assetHoldersMaxLimit is the page size every cached entry holds: the handler's ParseLimit ceiling, so an entry serves any accepted
// `?limit=` by slicing.
const assetHoldersMaxLimit = 500

// assetHoldersRefreshTimeout bounds one detached holders scan: well above the largest asset's two
// FINAL scans, but finite so a wedged query cannot pin the single-flight forever.
const assetHoldersRefreshTimeout = 90 * time.Second

type assetHoldersEntry struct {
	holders    []clickhouse.AssetHolder
	total      int64
	cachedAt   time.Time
	asOfLedger uint32
}

// snapshotVintage is when a cached snapshot was computed and the lake ledger read just before its
// scan; a handler stamps as_of and as_of_ledger from ONE of these so they cannot disagree.
type snapshotVintage struct {
	at     time.Time
	ledger uint32
}

// vintageLedger reads the lake watermark BEFORE the scan, so it never names a ledger later than
// the data scanned; 0 (as_of_ledger omitted) when no watermark is available.
func (h *Handler) vintageLedger(ctx context.Context) uint32 {
	if h.LakeWatermark == nil {
		return 0
	}
	l, _, ok := h.LakeWatermark(ctx)
	if !ok {
		return 0
	}
	return l
}

// assetHoldersCache is the TTL+bounded cache behind GET /v1/assets/{asset_id}/holders. Zero value ready to use, like opsDirCache.
type assetHoldersCache struct {
	mu      sync.Mutex
	entries map[string]assetHoldersEntry
	flight  perKeyFlight
}

// get returns the entry for asset even past the TTL (fresh=false), so a run of failed refreshes
// degrades to old-but-real data instead of 503s. ok=false only when never computed.
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
		// Evict the oldest entry: an approximate LRU in one pass, only at capacity.
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

// errRefreshFailed is returned by a wait path when the detached compute finished without a
// cacheable entry; the refresh goroutine already logged the cause.
var errRefreshFailed = errors.New("explorer: detached cache refresh produced no entry")

// errRefreshSaturated goes to a COLD-path waiter when the shared detached-refresh gate is full and the refresh was skipped (see
// clickhouse.RefreshGate). Handlers map it to the same retryable 503 as a read timeout: a capacity condition, not a bug.
var errRefreshSaturated = errors.New("explorer: detached refresh capacity saturated; retry shortly")

// errRefreshPanicked is what a COLD-path waiter gets when the detached refresh it joined panicked, distinct from a reader error.
// A STALE entry is unaffected: the panicking flight is ended, not latched.
var errRefreshPanicked = errors.New("explorer: detached refresh panicked")

// refreshAssetHolders kicks ONE detached holders scan for asset (no-op when a flight is up) and returns the flight. Detached on purpose:
// bound to an 8s request deadline, a huge asset's scan never completes and the cache never fills.
func (h *Handler) refreshAssetHolders(asset string) *keyFlight {
	fl, owner := h.assetHolders.flight.begin(asset)
	if !owner {
		return fl
	}
	// Bounded globally across keys because the asset space is attacker-mintable (any
	// CODE-ISSUER parses); on saturation skip, don't queue (see detachedGate).
	gate := h.detachedGate()
	if !gate.TryAcquireClass("asset_holders") {
		h.assetHolders.flight.end(asset, fl, errRefreshSaturated)
		return fl
	}
	go func() {
		defer gate.ReleaseClass("asset_holders")
		// End the flight from a defer so a panic cannot wedge this board forever.
		var err error
		defer func() {
			if rec := recover(); rec != nil {
				worker.Report(h.Logger, "explorer-asset-holders-refresh", rec)
				err = errRefreshPanicked
			}
			h.assetHolders.flight.end(asset, fl, err)
		}()
		start := time.Now()
		rctx, cancel := context.WithTimeout(context.Background(), assetHoldersRefreshTimeout)
		defer cancel()
		ledger := h.vintageLedger(rctx)
		holders, total, err := h.Reader.AssetHolders(rctx, asset, assetHoldersMaxLimit)
		obs.ObserveExplorerSWRRefresh("asset_holders", start, err)
		if err != nil {
			// Keep the previous entry (if any) — old-but-real beats blank.
			h.Logger.Warn("asset holders detached refresh failed", "asset", asset, "err", err)
			return
		}
		h.assetHolders.put(asset, assetHoldersEntry{holders: holders, total: total, cachedAt: time.Now(), asOfLedger: ledger})
	}()
	return fl
}

// assetHoldersCached serves the holders board: fresh as-is; STALE (degraded=true, real asOf) while a detached rescan runs; never-computed
// waits to the deadline, so a huge asset 503s THIS request but a retry lands warm. The total is limit-independent.
func (h *Handler) assetHoldersCached(ctx context.Context, asset string, limit int) (holders []clickhouse.AssetHolder, total int64, asOf snapshotVintage, degraded bool, err error) {
	if e, ok, fresh := h.assetHolders.get(asset); ok {
		if !fresh {
			h.refreshAssetHolders(asset) //nolint:contextcheck // intentional detach — the rescan must outlive this request (see refreshAssetHolders)
		}
		return sliceHolders(e.holders, limit), e.total, snapshotVintage{e.cachedAt, e.asOfLedger}, !fresh, nil
	}
	// Stone-cold: wait for the detached compute, bounded by OUR deadline
	// only — the compute itself is not.
	fl := h.refreshAssetHolders(asset) //nolint:contextcheck // intentional detach — a request that times out must not kill the fill (see refreshAssetHolders)
	select {
	case <-fl.done:
		if e, ok, _ := h.assetHolders.get(asset); ok {
			return sliceHolders(e.holders, limit), e.total, snapshotVintage{e.cachedAt, e.asOfLedger}, false, nil
		}
		if fl.err != nil {
			return nil, 0, snapshotVintage{}, false, fl.err
		}
		return nil, 0, snapshotVintage{}, false, errRefreshFailed
	case <-ctx.Done():
		return nil, 0, snapshotVintage{}, false, ctx.Err()
	}
}

func sliceHolders(rows []clickhouse.AssetHolder, limit int) []clickhouse.AssetHolder {
	if limit > 0 && len(rows) > limit {
		return rows[:limit]
	}
	return rows
}

// contractsDirTTL bounds how stale the cached contracts directory may be. Much longer than the 3s opsDirTTL: it aggregates a MULTI-DAY
// window, and each recompute is a GROUP BY over the billions-row contract_events table.
const contractsDirTTL = 5 * time.Minute

// contractsWindowLadder is the windows GET /v1/contracts aggregates over; `?days=` rounds UP (a SUPERSET), echoed in `window_days`.
// Quantising caps `?days=1..365` walks at len(ladder) distinct multi-day GROUP BYs.
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

// contractsDirMaxLimit is the page size every cached entry holds, the handler's ParseLimit ceiling, so one entry serves any `?limit=`.
const contractsDirMaxLimit = 500

// contractsDirRefreshTimeout bounds one detached aggregation. The 365d rung is a GROUP BY over a
// year of contract_events (minutes under load), so it is generous but finite.
const contractsDirRefreshTimeout = 3 * time.Minute

type contractsDirEntry struct {
	rows     []clickhouse.ContractDirectoryRow
	types    map[string]bool // contract id -> isSAC; nil when the lookup failed
	since    uint32
	cachedAt time.Time
}

// contractsDirCache is the TTL cache behind the GET /v1/contracts directory, keyed on the ladder-quantised window.
// At most len(contractsWindowLadder) entries, so no eviction pass. Zero value ready to use.
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

// refreshContractsDir kicks ONE detached aggregation for ladder rung `window` (no-op while a flight is up). Detached as in
// refreshAssetHolders: the multi-day GROUP BY cannot fit a request deadline and would leave the cache cold forever.
func (h *Handler) refreshContractsDir(window int) *keyFlight {
	key := strconv.Itoa(window)
	fl, owner := h.contractsDir.flight.begin(key)
	if !owner {
		return fl
	}
	// Same global gate as the sibling refreshers: the ladder caps this key space at 5, but the
	// scans still contend on the shared pool. The prewarm loop re-kicks within one TTL if skipped.
	gate := h.detachedGate()
	if !gate.TryAcquireClass("contracts_dir") {
		h.contractsDir.flight.end(key, fl, errRefreshSaturated)
		return fl
	}
	go func() {
		defer gate.ReleaseClass("contracts_dir")
		// End the flight from a defer so a panic cannot wedge this window forever.
		var err error
		defer func() {
			if rec := recover(); rec != nil {
				worker.Report(h.Logger, "explorer-contracts-dir-refresh", rec)
				err = errRefreshPanicked
			}
			h.contractsDir.flight.end(key, fl, err)
		}()
		start := time.Now()
		rctx, cancel := context.WithTimeout(context.Background(), contractsDirRefreshTimeout)
		defer cancel()
		since, err := h.windowFloorLedger(rctx, window)
		if err != nil {
			// Keep the previous entry (if any) — old-but-real beats blank,
			// and a failed tip read must not fall back to a genesis-wide
			// scan.
			h.Logger.Warn("contracts directory detached refresh failed (tip read)", "window_days", window, "err", err)
			return
		}
		rows, err := h.Reader.RecentContracts(rctx, contractsDirMaxLimit, since)
		obs.ObserveExplorerSWRRefresh("contracts_dir", start, err)
		if err != nil {
			// Keep the previous entry (if any) — old-but-real beats blank.
			h.Logger.Warn("contracts directory detached refresh failed", "window_days", window, "err", err)
			return
		}
		ids := make([]string, len(rows))
		for i, row := range rows {
			ids[i] = row.ContractID
		}
		types, terr := h.Reader.ContractTypes(rctx, ids)
		if terr != nil {
			h.Logger.Warn("contracts directory type lookup failed; serving without types", "err", terr)
			types = nil
		}
		h.contractsDir.put(window, contractsDirEntry{rows: rows, types: types, since: since, cachedAt: time.Now()})
	}()
	return fl
}

// recentContractsCached serves the directory for the quantised window: fresh as-is; STALE (degraded=true, real asOf) while a detached
// rerun goes; never-computed waits to the deadline. Returns the ledger floor used so `since_ledger` describes the data served.
func (h *Handler) recentContractsCached(ctx context.Context, window, limit int) (rows []clickhouse.ContractDirectoryRow, types map[string]bool, since uint32, asOf time.Time, degraded bool, err error) {
	if e, ok, fresh := h.contractsDir.get(window); ok {
		if !fresh {
			h.refreshContractsDir(window) //nolint:contextcheck // intentional detach — see refreshContractsDir
		}
		return sliceContracts(e.rows, limit), e.types, e.since, e.cachedAt, !fresh, nil
	}
	fl := h.refreshContractsDir(window) //nolint:contextcheck // intentional detach — see refreshContractsDir
	select {
	case <-fl.done:
		if e, ok, _ := h.contractsDir.get(window); ok {
			return sliceContracts(e.rows, limit), e.types, e.since, e.cachedAt, false, nil
		}
		if fl.err != nil {
			return nil, nil, 0, time.Time{}, false, fl.err
		}
		return nil, nil, 0, time.Time{}, false, errRefreshFailed
	case <-ctx.Done():
		return nil, nil, 0, time.Time{}, false, ctx.Err()
	}
}

// PrewarmContractsDirectory primes the directory's DEFAULT ladder rung (30d); the 5-minute prewarm loop matches contractsDirTTL and it
// only kicks the detached refresh, ctx gating the kick at shutdown.
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
