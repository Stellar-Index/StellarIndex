package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// decimalsCacheRefreshInterval mirrors internal/api/v1.NonstandardDecimalsRefreshInterval
// (not imported: the aggregator binary must not depend on api/v1). The two caches are
// independent in-process mirrors of the same tiny `nonstandard_decimals_assets` table.
const decimalsCacheRefreshInterval = 60 * time.Second

// decimalsAssetReader is the storage seam this cache consults. Satisfied
// by *timescale.Store via LoadNonstandardDecimalsAssets.
type decimalsAssetReader interface {
	LoadNonstandardDecimalsAssets(ctx context.Context) ([]timescale.NonstandardDecimalsAsset, error)
}

// decimalsCache is the aggregator-binary mirror of
// internal/api/v1.NonstandardDecimalsCache: a periodically refreshed snapshot of
// `nonstandard_decimals_assets`, wired into orchestrator.Config.DecimalsLookup so the published
// VWAP is decimals-normalized for any confirmed non-7-decimals leg
// (docs/operations/runbooks/dex.md).
//
// It is a standalone type because the orchestrator's seams are storage-agnostic interfaces and
// this binary owns the DB-aware wiring that satisfies aggregate.DecimalsLookup structurally.
//
// Fail-open by construction, like the API-side cache: a read error or unrefreshed cache reports
// "nothing flagged", so a Postgres blip delays normalization for a brand-new offender rather
// than blocking VWAP publication.
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
