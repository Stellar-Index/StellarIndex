// Package marketcap maintains a process-local cache of CoinGecko-
// sourced market_cap snapshots for the verified-currency catalogue's
// crypto + stablecoin entries.
//
// Why a separate package: the catalogue ships fixed `circulating_supply`
// values only for fiat (USD M2 = $21.7T, etc.) — supply for crypto is
// reflexively defined by the network and changes continuously. CoinGecko
// publishes a daily-updated `market_cap` aggregate per coin slug we can
// consume via /simple/price?include_market_cap=true; that's the cheapest
// path to a stable, well-understood market_cap_usd value on the unified
// /v1/assets listing without standing up a per-network supply observer
// for every chain.
//
// Scope: BTC, ETH, SOL, etc. plus stablecoin caps (USDC ~$30B, USDT
// ~$120B). Fiat continues to use the catalogue's M2 × FX path
// (assets_global.go::fiatMarketCapUSD); this cache is the second
// market_cap source the listing consults.
//
// The cache is thread-safe and tolerates poller failure (stale reads
// served indefinitely until the next successful refresh).
package marketcap

import (
	"sync"
	"time"
)

// Snapshot is the per-slug market data returned by /simple/price
// with include_market_cap=true&include_24hr_change=true. Values are
// kept as raw strings (no big.Float / decimal parsing in the cache
// layer) so the wire shape passes through unchanged — handlers
// reformat at projection time.
type Snapshot struct {
	// MarketCapUSD — CG's circulating × spot price in USD,
	// formatted as a fixed-2 decimal string (e.g.
	// "1234567890.12"). Empty when CG returns no value for the
	// slug (free-tier 403 / temporary 5xx / unknown slug).
	MarketCapUSD string

	// PriceUSD — current USD price per CG (sanity cross-check
	// against our aggregator). Optional, fixed-precision string.
	PriceUSD string

	// Change24hPct — 24h percent change from CG, signed string
	// with 2 fractional digits (e.g. "+1.27", "-0.05"). Optional.
	Change24hPct string

	// FetchedAt is the wall-clock time the refresher last replaced
	// this snapshot. Useful for staleness checks at handler time.
	FetchedAt time.Time
}

// Cache is the thread-safe in-memory store. Keyed by catalogue slug
// (lowercase, e.g. "btc", "usdc", "xlm"). One process-local instance
// shared across handlers; the Refresher writes, handlers read.
type Cache struct {
	mu sync.RWMutex
	m  map[string]Snapshot
}

// New returns an empty cache. Used by both production wiring (in
// cmd/stellarindex-api/main.go) and tests.
func New() *Cache {
	return &Cache{m: make(map[string]Snapshot, 32)}
}

// Lookup returns the snapshot for `slug` plus a found bool. The
// snapshot is a copy; mutating the returned struct does not affect
// the cache.
func (c *Cache) Lookup(slug string) (Snapshot, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.m[slug]
	return s, ok
}

// Store writes a snapshot for `slug`. Called by Refresher on each
// successful CG response; safe to call concurrently from a single
// refresh goroutine and reads from multiple handler goroutines.
func (c *Cache) Store(slug string, s Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[slug] = s
}

// All returns a shallow copy of every snapshot in the cache. Exists
// for diagnostic surfaces (a future /v1/diagnostics/marketcap dump)
// and tests; not used on the hot path.
func (c *Cache) All() map[string]Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]Snapshot, len(c.m))
	for k, v := range c.m {
		out[k] = v
	}
	return out
}
