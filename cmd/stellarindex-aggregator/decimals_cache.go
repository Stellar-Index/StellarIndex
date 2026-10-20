package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// decimalsCacheRefreshInterval mirrors api/v1.NonstandardDecimalsRefreshInterval (not imported: the aggregator must not depend on api/v1).
const decimalsCacheRefreshInterval = 60 * time.Second

// decimalsAssetReader is the storage seam this cache consults. Satisfied
// by *timescale.Store via LoadNonstandardDecimalsAssets.
type decimalsAssetReader interface {
	LoadNonstandardDecimalsAssets(ctx context.Context) ([]timescale.NonstandardDecimalsAsset, error)
}

// decimalsCache mirrors api/v1.NonstandardDecimalsCache for orchestrator.Config.DecimalsLookup (decimals-normalized VWAP for
// non-7dp legs). Fail-open: a read error or unrefreshed cache reports "nothing flagged", so a Postgres blip delays
// normalization instead of blocking VWAP.
type decimalsCache struct {
	mu     sync.RWMutex
	assets map[string]int
	reader decimalsAssetReader
	logger *slog.Logger
}

func newDecimalsCache(reader decimalsAssetReader, logger *slog.Logger) *decimalsCache {
	return &decimalsCache{reader: reader, logger: logger}
}

// Refresh reloads the snapshot from Postgres and atomically swaps it in; on error the previous snapshot (if any) is retained.
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

// Lookup satisfies aggregate.DecimalsLookup; a nil receiver or cold cache reports found=false (never "everything flagged"), the same fail-open contract as internal/api/v1's cache.
func (c *decimalsCache) Lookup(assetID string) (decimals int, found bool) {
	if c == nil {
		return 0, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	d, ok := c.assets[assetID]
	return d, ok
}

// run refreshes on decimalsCacheRefreshInterval until ctx is cancelled; errors are logged in Refresh and swallowed so a DB blip can't exit
// the process.
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
