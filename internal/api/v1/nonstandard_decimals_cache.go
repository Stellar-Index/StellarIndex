package v1

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// NonstandardDecimalsReader is the storage seam the serving guard consults;
// *timescale.Store satisfies it, and the interface keeps the cache testable without a database.
type NonstandardDecimalsReader interface {
	LoadNonstandardDecimalsAssets(ctx context.Context) ([]timescale.NonstandardDecimalsAsset, error)
}

// NonstandardDecimalsRefreshInterval is the cadence at which main.go refreshes the cache. This is
// the READ side of the dex-nonstandard-decimals guard (docs/operations/runbooks/dex.md): the
// decimals-guard sweep writes confirmed non-7-decimal assets into `nonstandard_decimals_assets`
// and the cache mirrors it so price surfaces can NORMALIZE a pair touching such an asset without
// a per-request DB round trip.
//
// Surfaces correct rather than decline, applying aggregate.AdjustPrice's exact
// 10^(baseDecimals-quoteDecimals) factor (see Server.normalizeRawRatioString). 60s lets an
// operator's correction reach serving within one interval.
const NonstandardDecimalsRefreshInterval = 60 * time.Second

// NonstandardDecimalsCache wraps a [NonstandardDecimalsReader] with an in-process snapshot
// refreshed on a background schedule (mirroring [CoverageCache]).
//
// Fail-open by design: a read error keeps the PREVIOUS snapshot (WARN + failure counter) rather
// than clearing it, so a Postgres blip is not a blanket price outage while known offenders stay
// guarded. A cold cache means "nothing flagged", never "everything flagged"; see
// [NonstandardDecimalsCache.Lookup].
type NonstandardDecimalsCache struct {
	mu        sync.RWMutex
	assets    map[string]timescale.NonstandardDecimalsAsset
	fetchedAt time.Time
	reader    NonstandardDecimalsReader
	logger    Logger
}

// NewNonstandardDecimalsCache constructs an empty cache; call Refresh once at startup. An
// unrefreshed cache is fail-open (Lookup reports "not flagged"), so a slow first refresh means
// "guard not yet effective", never "everything declined".
func NewNonstandardDecimalsCache(reader NonstandardDecimalsReader, logger Logger) *NonstandardDecimalsCache {
	return &NonstandardDecimalsCache{reader: reader, logger: logger}
}

// Refresh swaps in a fresh snapshot on success; on error the previous one is kept so a
// transient DB hiccup cannot blank (or invert) the guard.
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

// warnOnPartialAliasFamily reports a flagged asset whose alias family is only PARTLY flagged.
// [NonstandardDecimalsCache.Lookup] does not alias-fold, so an asset with several spellings (XLM
// is `native`, `crypto:XLM` and its SAC) would be normalised under one and not another. The
// result is a WRONG value, not a missing one: the price leg scales per source pair while the
// supply leg divides by the requested base's decimals, so they diverge by a power of ten.
//
// Unreachable on today's data (every flagged row is a bare C-strkey with a singleton family);
// the durable fix is alias-folding the lookup, which changes the contract for other callers. The
// bad row arrives via a TABLE at runtime, so only a refresh-time check can catch it.
//
// It warns and counts but neither drops the row nor fails the refresh: blanking the guard would
// turn a scaling error into an unnormalised one across every surface.
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

// Lookup reports whether assetID is a confirmed non-7-decimal asset and its decimals. A nil or
// cold cache reports found=false: the guard is opt-in and fails open, never closed.
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
