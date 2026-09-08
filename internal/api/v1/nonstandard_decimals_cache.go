package v1

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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
// /v1/ohlc, /v1/price/at and the chart surfaces can NORMALIZE a pair
// touching such an asset without a per-request DB round trip.
//
// The "real normalization" this originally promised has since shipped:
// these surfaces no longer decline, they correct, applying
// aggregate.AdjustPrice's exact 10^(baseDecimals-quoteDecimals) factor
// (see Server.normalizeRawRatioString). 60s keeps the cache tight so an
// operator who corrects a row sees serving follow within one interval
// rather than a process lifetime.
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
	c.warnOnPartialAliasFamily(next)
	c.mu.Lock()
	c.assets = next
	c.fetchedAt = time.Now().UTC()
	c.mu.Unlock()
	return nil
}

// warnOnPartialAliasFamily reports a flagged asset whose alias family is only
// PARTLY flagged. [NonstandardDecimalsCache.Lookup] is a raw map lookup on the
// exact asset-id string, so it does not alias-fold — and an asset that lives
// under several canonical spellings (XLM is `native`, `crypto:XLM` and its SAC
// contract id) would then be normalised under one spelling and not another.
// The consequence is not a missing value but a WRONG one: the price leg is
// scaled per source pair, whose base may be a different spelling of the same
// asset, while the supply leg is divided by the requested base's decimals — so
// price and supply diverge by a power of ten with both legs looking plausible.
//
// This is unreachable on today's data (every flagged row is a bare C-strkey
// with a singleton alias family) and the durable fix is to alias-fold the
// lookup itself, which changes this cache's contract and affects its other
// callers. That is deliberately not done here. What IS done is refusing to let
// the state arrive silently: the dangerous row is added to a TABLE at runtime,
// not to code, so no unit test over fixtures can catch it — only a check at
// refresh time against the rows actually loaded can.
//
// It warns and counts; it does not drop the row or fail the refresh. A partial
// family is a data-entry question for an operator, and blanking the guard
// would turn a scaling error into an unnormalised one across every surface.
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
