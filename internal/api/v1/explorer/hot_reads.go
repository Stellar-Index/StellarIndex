package explorer

import (
	"context"
	"strconv"
	"sync"
	"time"

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
// What is NOT here: a background refresher. Both endpoints degrade to
// latency on a cold read, not to an error, so the request path may fill
// the cache (the AccountsByWealth case needed a refresher only because
// its scan cannot fit any request deadline). That is the same reasoning
// 39b244b6 recorded when it deliberately left idx_lec_account_id /
// idx_lec_asset alone: readers that "degrade to latency, not errors" get
// the cheap fix, not the expensive one.

// perKeyFlight collapses concurrent work for the same key. A copy of the
// clickhouse-package helper of the same name (this package cannot import
// an unexported type); keep the two in sync.
type perKeyFlight struct {
	mu      sync.Mutex
	inGoing map[string]chan struct{}
}

// begin returns the in-flight channel for key and whether the caller
// owns the flight. Owners must call end; waiters select on the channel.
// Zero value ready to use.
func (f *perKeyFlight) begin(key string) (chan struct{}, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.inGoing[key]; ok {
		return ch, false
	}
	if f.inGoing == nil {
		f.inGoing = make(map[string]chan struct{})
	}
	ch := make(chan struct{})
	f.inGoing[key] = ch
	return ch, true
}

func (f *perKeyFlight) end(key string, ch chan struct{}) {
	f.mu.Lock()
	delete(f.inGoing, key)
	f.mu.Unlock()
	close(ch)
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

func (c *assetHoldersCache) get(asset string) (assetHoldersEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[asset]
	if !ok || time.Since(e.cachedAt) > assetHoldersTTL {
		return assetHoldersEntry{}, false
	}
	return e, true
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

// assetHoldersCached serves the holders board for `asset` from the TTL
// cache, collapsing a concurrent burst for the same asset into one scan
// and falling through to a live read on a miss. The returned slice is
// capped at `limit`; the total holder count is limit-independent.
func (h *Handler) assetHoldersCached(ctx context.Context, asset string, limit int) ([]clickhouse.AssetHolder, int64, error) {
	if e, ok := h.assetHolders.get(asset); ok {
		return sliceHolders(e.holders, limit), e.total, nil
	}
	ch, owner := h.assetHolders.flight.begin(asset)
	if owner {
		defer h.assetHolders.flight.end(asset, ch)
	} else {
		// Another request is already scanning this asset. Wait for it
		// rather than launch a duplicate pair of FINAL scans.
		select {
		case <-ch:
			if e, ok := h.assetHolders.get(asset); ok {
				return sliceHolders(e.holders, limit), e.total, nil
			}
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
		// The waited-for refresh produced nothing cacheable (it errored);
		// fall through to our own read.
	}
	holders, total, err := h.Reader.AssetHolders(ctx, asset, assetHoldersMaxLimit)
	if err != nil {
		return nil, 0, err
	}
	h.assetHolders.put(asset, assetHoldersEntry{holders: holders, total: total, cachedAt: time.Now()})
	return sliceHolders(holders, limit), total, nil
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

func (c *contractsDirCache) get(window int) (contractsDirEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[window]
	if !ok || time.Since(e.cachedAt) > contractsDirTTL {
		return contractsDirEntry{}, false
	}
	return e, true
}

func (c *contractsDirCache) put(window int, e contractsDirEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[int]contractsDirEntry)
	}
	c.entries[window] = e
}

// recentContractsCached serves the contracts directory for the
// ladder-quantised `window` from the TTL cache, collapsing a concurrent
// burst for the same window into one aggregation. Returns the rows
// (capped at `limit`) plus the ledger floor the cached aggregate used,
// so the response's `since_ledger` describes the data actually served
// rather than a floor recomputed after the fact.
func (h *Handler) recentContractsCached(ctx context.Context, window, limit int) ([]clickhouse.ContractDirectoryRow, uint32, error) {
	key := strconv.Itoa(window)
	if e, ok := h.contractsDir.get(window); ok {
		return sliceContracts(e.rows, limit), e.since, nil
	}
	ch, owner := h.contractsDir.flight.begin(key)
	if owner {
		defer h.contractsDir.flight.end(key, ch)
	} else {
		select {
		case <-ch:
			if e, ok := h.contractsDir.get(window); ok {
				return sliceContracts(e.rows, limit), e.since, nil
			}
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
	since := h.windowFloorLedger(ctx, window)
	rows, err := h.Reader.RecentContracts(ctx, contractsDirMaxLimit, since)
	if err != nil {
		return nil, 0, err
	}
	h.contractsDir.put(window, contractsDirEntry{rows: rows, since: since, cachedAt: time.Now()})
	return sliceContracts(rows, limit), since, nil
}

func sliceContracts(rows []clickhouse.ContractDirectoryRow, limit int) []clickhouse.ContractDirectoryRow {
	if limit > 0 && len(rows) > limit {
		return rows[:limit]
	}
	return rows
}
