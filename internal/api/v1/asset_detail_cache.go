package v1

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// assetDetailEntry holds a fully-rendered /v1/assets/{id} response
// for serving warm requests inside the cache TTL.
//
// The cached payload is the full JSON wire bytes (flags already
// encoded into the envelope by renderAssetDetailEnvelope) plus the
// time it was cached. Storing post-render avoids // chain + asset-catalogue overlay + verified-currency overlay on every hit.
type assetDetailEntry struct {
	body     []byte
	cachedAt time.Time
}

// assetDetailResponseCache is the response-level cache for
// /v1/assets/{id}. Keyed by the normalised asset_id path segment
// (post-`normaliseAssetIDInput`); value is the full pre-rendered
// JSON bytes so the warm path can replay the wire body without
// re-running the handler chain.
//
// Why a response cache instead of per-reader caches: the underlying
// readers fan out wide (Volume24hUSDForAsset, supply.LatestSupply,
// lookupUSDPrice × 2, applyAssetExtensionFields 7 readers,
// applySep1Overlay metadata fetch, ...). Wrapping every reader is
// 5+ new wrapper types. The handler-level cache is one type and
// covers every cost.
//
// Drift-safe: the cached entry IS what the handler would produce
// because the handler IS what populates the cache. New fields,
// new overlays, new readers all flow through without any
// per-reader plumbing.
//
// TTL (120s in production, set in server.New) MUST exceed the
// selfPrewarmAssetEndpoints cadence so the prewarm pass always
// refreshes an entry before it expires — otherwise the cache is
// cold for the gap between TTL expiry and the next prewarm, and
// every request in that window pays the full handler cost (#52).
// The underlying data updates per-minute (closed-bucket prices_1m)
// and per-tx (volume / supply); 120s staleness is well inside the
// "closed-bucket only" API contract per ADR-0015.
type assetDetailResponseCache struct {
	mu      sync.RWMutex
	entries map[string]*assetDetailEntry
	ttl     time.Duration
}

// newAssetDetailResponseCache constructs a cache with the given
// TTL. ttl=0 disables caching (the lookup helpers return false on
// every probe). Useful for tests that want to bypass the cache.
func newAssetDetailResponseCache(ttl time.Duration) *assetDetailResponseCache {
	return &assetDetailResponseCache{
		entries: make(map[string]*assetDetailEntry),
		ttl:     ttl,
	}
}

// get returns the cached entry for assetID if present AND fresh
// (cachedAt + ttl > now). Returns (nil, false) on miss / stale /
// disabled cache.
func (c *assetDetailResponseCache) get(assetID string) (*assetDetailEntry, bool) {
	if c == nil || c.ttl <= 0 {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[assetID]
	if !ok {
		return nil, false
	}
	if time.Since(e.cachedAt) > c.ttl {
		return nil, false
	}
	return e, true
}

// assetDetailCacheMaxEntries hard-bounds the response cache. Fabricated
// asset_ids 404 before put (handleAssetGet), so growth is driven by the
// REAL-asset universe — but that universe is large (every classic asset
// with any trade/oracle observation, ~10^5+). Without a cap a crawler or
// an attacker walking known asset_ids would add a permanent ~1-4 KB entry
// per distinct id — TTL-expiry only checks freshness at READ time and
// never removes anything — climbing resident memory to hundreds of MB / a
// slow OOM on a long-lived API instance (audit W6-perf-1). Every sibling
// response cache is bounded (accountStateCacheMax=4096, assetHoldersCacheMax
// =512); this makes the asset-detail cache match. The verified-currency set
// + native + the realistic exotic-classic tail sits well under the cap; it
// only engages under adversarial/crawler id churn.
const assetDetailCacheMaxEntries = 4096

// put stores a freshly-rendered response under assetID. Existing
// entries are replaced (last-write-wins; concurrent requests for
// the same asset may both compute, both write — accepted as a
// stampede cost rather than adding a single-flight layer here).
//
// Callers should pass an already-marshalled body (flags already
// encoded into it by renderAssetDetailEnvelope); this avoids holding
// the lock during JSON encoding.
func (c *assetDetailResponseCache) put(assetID string, body []byte) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[assetID]; !exists && len(c.entries) >= assetDetailCacheMaxEntries {
		c.evictLocked()
	}
	c.entries[assetID] = &assetDetailEntry{
		body:     body,
		cachedAt: time.Now(),
	}
}

// evictLocked reclaims room when the cache is at capacity. It first drops
// every expired entry (cheap, TTL-based — the common case, since the prewarm
// cadence keeps the hot set fresh and everything else ages out); only if the
// whole map is still fresh does it evict the single oldest entry so a new one
// can be admitted. Both passes are O(n) but n is capped at
// assetDetailCacheMaxEntries and eviction runs only at capacity, so it is off
// every hot path. Caller must hold c.mu.
func (c *assetDetailResponseCache) evictLocked() {
	before := len(c.entries)
	now := time.Now()
	for id, e := range c.entries {
		if now.Sub(e.cachedAt) > c.ttl {
			delete(c.entries, id)
		}
	}
	if len(c.entries) < before {
		return // expired-purge freed space
	}
	// All entries fresh — evict the oldest to keep the map bounded.
	var oldestID string
	var oldestAt time.Time
	for id, e := range c.entries {
		if oldestID == "" || e.cachedAt.Before(oldestAt) {
			oldestID, oldestAt = id, e.cachedAt
		}
	}
	if oldestID != "" {
		delete(c.entries, oldestID)
	}
}

// renderAssetDetailEnvelope builds the wire bytes for an AssetDetail
// response and returns them. Mirrors writeJSON / writeEnvelope so
// the cached body matches the live writeJSON output byte-for-byte.
//
// Used by the handleAssetGet cache path:
//   - On cache miss: render to bytes, cache them, write to client.
//   - On cache hit: replay the cached bytes verbatim. The AsOf
//     timestamp is frozen at render time and served up to ttl stale
//     — there is no AsOf re-splice (that would require re-parsing the
//     body); ttl staleness sits well inside the closed-bucket
//     contract (ADR-0015).
//
// Returns the raw envelope bytes (the `Data` field is the
// AssetDetail).
func renderAssetDetailEnvelope(detail AssetDetail, flags Flags) ([]byte, error) {
	env := Envelope{
		Data:  detail,
		AsOf:  time.Now().UTC(),
		Flags: flags,
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(env); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeCachedAssetDetail writes a cached entry to the response.
// Mirrors writeJSON's header set (Content-Type: application/json,
// implicit 200). The body bytes carry an AsOf that's up to ttl
// stale; clients reading AsOf know the data was computed at-most
// `ttl` ago. Per ADR-0015's closed-bucket-only contract that's
// well within the allowed staleness envelope.
func writeCachedAssetDetail(w http.ResponseWriter, entry *assetDetailEntry) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Stellarindex-Cache", "HIT")
	_, _ = w.Write(entry.body)
}
