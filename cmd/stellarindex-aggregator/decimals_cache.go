package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// decimalsCacheRefreshInterval mirrors internal/api/v1.NonstandardDecimalsRefreshInterval
// (not imported directly — the aggregator binary has no dependency on the
// api/v1 package, per D8 layering; the two caches are independent mirrors
// of the same tiny `nonstandard_decimals_assets` table, matching the
// existing pattern of the API process running its own mirror rather than
// sharing one in-process cache across binaries).
const decimalsCacheRefreshInterval = 60 * time.Second

// decimalsAssetReader is the storage seam this cache consults. Satisfied
// by *timescale.Store via LoadNonstandardDecimalsAssets.
type decimalsAssetReader interface {
	LoadNonstandardDecimalsAssets(ctx context.Context) ([]timescale.NonstandardDecimalsAsset, error)
}

// decimalsCache is the aggregator-binary mirror of
// internal/api/v1.NonstandardDecimalsCache — an in-process, periodically-
// refreshed snapshot of `nonstandard_decimals_assets` (migration 0093),
// wired into orchestrator.Config.DecimalsLookup so the orchestrator's
// published VWAP is decimals-normalized for any confirmed non-7-decimals
// leg (docs/operations/runbooks/dex-nonstandard-decimals.md).
//
// Kept as a small standalone type here (not a shared package with
// internal/api/v1's cache) deliberately: internal/aggregate/orchestrator's
// existing Store/FXStore/Cache seams are all storage-agnostic local
// interfaces, and this binary is the natural place to own the DB-aware
// wiring that satisfies aggregate.DecimalsLookup structurally — exactly
// how FXStore: store and Baselines: baselineLookupAdapter{...} are wired
// a few lines below in run().
//
// Fail-open by construction, same as the API-side cache: a read error or
// an unrefreshed cache leaves Lookup reporting "nothing flagged" (via the
// zero-value map), so a Postgres blip degrades to "normalization not yet
// applied for a brand-new offender" rather than blocking VWAP publication.
type decimalsCache struct {
	mu     sync.RWMutex
	assets map[string]int
	reader decimalsAssetReader
	logger *slog.Logger
}

func newDecimalsCache(reader decimalsAssetReader, logger *slog.Logger) *decimalsCache {
	return &decimalsCache{reader: reader, logger: logger}
}

// Refresh reloads the snapshot from Postgres, atomically swapping it on
// success. On error the previous snapshot (if any) is retained.
func (c *decimalsCache) Refresh(ctx context.Context) error {
	rows, err := c.reader.LoadNonstandardDecimalsAssets(ctx)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("decimals-cache: refresh failed — serving last-good snapshot for VWAP normalization", "err", err)
		}
		return err
	}
	next := make(map[string]int, len(rows))
	for _, row := range rows {
		next[row.Asset] = row.Decimals
	}
	c.mu.Lock()
	c.assets = next
	c.mu.Unlock()
	return nil
}

// Lookup satisfies aggregate.DecimalsLookup. A nil receiver or a cold/
// never-refreshed cache both report found=false (never "everything
// flagged") — same fail-open contract as internal/api/v1's cache.
func (c *decimalsCache) Lookup(assetID string) (decimals int, found bool) {
	if c == nil {
		return 0, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	d, ok := c.assets[assetID]
	return d, ok
}

// run refreshes on decimalsCacheRefreshInterval until ctx is cancelled.
// Errors are logged (inside Refresh) and swallowed — a transient DB blip
// must not exit the aggregator process.
func (c *decimalsCache) run(ctx context.Context) {
	ticker := time.NewTicker(decimalsCacheRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = c.Refresh(ctx)
		}
	}
}
