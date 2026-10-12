package v1

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// NonstandardDecimalsReader is the storage seam the serving guard consults; *timescale.Store satisfies it.
type NonstandardDecimalsReader interface {
	LoadNonstandardDecimalsAssets(ctx context.Context) ([]timescale.NonstandardDecimalsAsset, error)
}

// NonstandardDecimalsRefreshInterval is main.go's refresh cadence for the READ side of the dex-nonstandard-decimals guard; surfaces normalize with aggregate.AdjustPrice's exact factor rather than decline.
const NonstandardDecimalsRefreshInterval = 60 * time.Second

// NonstandardDecimalsCache is a background-refreshed snapshot that fails open: a read error keeps the PREVIOUS snapshot, and a cold cache means "nothing flagged".
type NonstandardDecimalsCache struct {
	mu        sync.RWMutex
	assets    map[string]timescale.NonstandardDecimalsAsset
	fetchedAt time.Time
	reader    NonstandardDecimalsReader
	logger    Logger
}

// NewNonstandardDecimalsCache constructs an empty cache; call Refresh once at startup, and until then the guard is fail-open (not yet effective).
func NewNonstandardDecimalsCache(reader NonstandardDecimalsReader, logger Logger) *NonstandardDecimalsCache {
	return &NonstandardDecimalsCache{reader: reader, logger: logger}
}

// Refresh swaps in a fresh snapshot on success and keeps the previous one on error so a DB blip cannot blank or invert the guard.
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
	c.warnOnPartialAliasFamily(next)
	c.mu.Lock()
	c.assets = next
	c.fetchedAt = time.Now().UTC()
	c.mu.Unlock()
	return nil
}

// warnOnPartialAliasFamily warns and counts (never drops or fails the refresh) when a flagged asset's alias family is only PARTLY
// flagged.
// [NonstandardDecimalsCache.Lookup] does not alias-fold, so price and supply legs scale by different powers of ten: a WRONG value, not a
// missing one.
// Unreachable on today's data; a bad TABLE row only shows up at refresh, and blanking the guard would turn a scaling error into an
// unnormalised one.
func (c *NonstandardDecimalsCache) warnOnPartialAliasFamily(next map[string]timescale.NonstandardDecimalsAsset) {
	for id := range next {
		asset, err := canonical.ParseAsset(id)
		if err != nil {
			continue // an unparseable id cannot have a computable family
		}
		for _, alias := range assetAliases(asset) {
			aliasID := alias.String()
			if aliasID == id {
				continue
			}
			if _, ok := next[aliasID]; ok {
				continue
			}
			obs.NonstandardDecimalsPartialAliasFamilyTotal.Inc()
			if c.logger != nil {
				c.logger.Warn(
					"nonstandard-decimals: flagged asset has an UNFLAGGED alias — price and supply can diverge by a power of ten",
					"flagged", id, "unflagged_alias", aliasID,
				)
			}
		}
	}
}

// Lookup reports whether assetID is a confirmed non-7-decimal asset and its decimals; a nil or cold cache reports found=false (fail-open).
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

// Snapshot returns the size and fetch time of the current snapshot, for diagnostics only.
func (c *NonstandardDecimalsCache) Snapshot() (count int, fetchedAt time.Time) {
	if c == nil {
		return 0, time.Time{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.assets), c.fetchedAt
}
