package v1

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// NonstandardDecimalsReader is the storage seam the read-time serving guard
// consults. *timescale.Store satisfies it via LoadNonstandardDecimalsAssets.
// Kept as an interface so [NonstandardDecimalsCache] is unit-testable
// without a database.
type NonstandardDecimalsReader interface {
	LoadNonstandardDecimalsAssets(ctx context.Context) ([]timescale.NonstandardDecimalsAsset, error)
}

// NonstandardDecimalsRefreshInterval is the cadence the background
// goroutine in main.go calls Refresh at. This is the READ side of the
// dex-nonstandard-decimals guard
// (docs/operations/runbooks/dex-nonstandard-decimals.md): the aggregator's
// decimals-guard sweep (internal/decimalsguard) writes confirmed non-7-
// decimal assets into `nonstandard_decimals_assets` (migration 0093); this
// cache mirrors that table in-process so /v1/price, /v1/vwap, /v1/history,
// /v1/ohlc can decline serving a pair touching an offending asset without a
// per-request DB round trip. 60s keeps the "self-clearing once real
// normalization ships" promise tight — an operator who removes the row
// sees the decline disappear within one interval, not a stale
// process-lifetime cache.
const NonstandardDecimalsRefreshInterval = 60 * time.Second

// NonstandardDecimalsCache wraps a [NonstandardDecimalsReader] with an
// in-process snapshot, refreshed on a background schedule
// (NewNonstandardDecimalsCache + Refresh wired in
// cmd/stellarindex-api/main.go, mirroring [CoverageCache]).
//
// Fail-open by design: a read error leaves the PREVIOUS snapshot in place
// (logged at WARN + obs.NonstandardDecimalsCacheRefreshFailuresTotal
// incremented) rather than clearing it — availability wins over the guard
// for infra errors. A Postgres blip must not turn into a blanket price
// outage across every pair; the guard itself stays effective for the
// known-offender case because the last-good snapshot is retained, and a
// cold cache (nothing fetched yet) is treated as "nothing flagged", never
// as "everything flagged" — see [NonstandardDecimalsCache.Lookup].
type NonstandardDecimalsCache struct {
	mu        sync.RWMutex
	assets    map[string]timescale.NonstandardDecimalsAsset
	fetchedAt time.Time
	reader    NonstandardDecimalsReader
	logger    Logger
}

// NewNonstandardDecimalsCache constructs an empty cache. Call Refresh once
// at startup before serving requests; subsequent refreshes happen on the
// background goroutine's schedule. An unrefreshed cache is nil-safe and
// fail-open (Lookup always reports "not flagged"), so a slow first refresh
// degrades to "guard not yet effective", never to "everything declined".
func NewNonstandardDecimalsCache(reader NonstandardDecimalsReader, logger Logger) *NonstandardDecimalsCache {
	return &NonstandardDecimalsCache{reader: reader, logger: logger}
}

// Refresh runs the underlying query and atomically swaps the cached
// snapshot on success. On error the previous snapshot is kept — a
// transient DB hiccup must not blank (or worse, invert) the guard.
func (c *NonstandardDecimalsCache) Refresh(ctx context.Context) error {
	rows, err := c.reader.LoadNonstandardDecimalsAssets(ctx)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("nonstandard-decimals cache refresh failed — serving last-good snapshot", "err", err)
		}
		obs.NonstandardDecimalsCacheRefreshFailuresTotal.Inc()
		return err
	}
	next := make(map[string]timescale.NonstandardDecimalsAsset, len(rows))
	for _, row := range rows {
		next[row.Asset] = row
	}
	c.mu.Lock()
	c.assets = next
	c.fetchedAt = time.Now().UTC()
	c.mu.Unlock()
	return nil
}

// Lookup reports whether assetID is a confirmed non-7-decimal asset and, if
// so, its declared decimals. A nil cache (guard not wired in this
// deployment) and a cold/never-refreshed cache both report found=false —
// the guard is opt-in via server wiring and fails open, never closed.
func (c *NonstandardDecimalsCache) Lookup(assetID string) (decimals int, found bool) {
	if c == nil {
		return 0, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	row, ok := c.assets[assetID]
	if !ok {
		return 0, false
	}
	return row.Decimals, true
}

// Snapshot returns the size + fetch time of the current snapshot —
// diagnostic use only (not consulted by the enforcement path).
func (c *NonstandardDecimalsCache) Snapshot() (count int, fetchedAt time.Time) {
	if c == nil {
		return 0, time.Time{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.assets), c.fetchedAt
}
