package v1

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// assetDetailEntry holds a fully-rendered /v1/assets/{id} response for warm requests inside the
// TTL: the JSON wire bytes (flags already encoded by renderAssetDetailEnvelope) plus the cache
// time. Storing post-render skips the handler chain and both overlays on every hit.
type assetDetailEntry struct {
	body     []byte
	cachedAt time.Time
	// degraded: the body was built over a failed or slow read, so a replay
	// must not carry the route's shared-cache band.
	degraded bool
}

// assetDetailResponseCache is the response-level cache for /v1/assets/{id}, keyed by the
// normalised asset_id (post-`normaliseAssetIDInput`) and holding the pre-rendered JSON.
//
// It caches the whole response because the handler fans out to many readers; wrapping each
// would be many new types. It is drift-safe: the entry IS what the handler produces, so new
// fields and overlays flow through with no per-reader plumbing.
//
// TTL (set in server.New) MUST exceed the selfPrewarmAssetEndpoints cadence, or the cache is
// cold between expiry and the next prewarm and every request in the gap pays full handler cost.
// The staleness is inside the closed-bucket contract (ADR-0015).
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

// assetDetailCacheMaxEntries hard-bounds the response cache. Fabricated ids 404 before put, but
// the real-asset universe is large (~10^5+), and TTL expiry only checks freshness at READ time
// and never removes anything, so a crawler walking known ids would slowly OOM the instance. The
// realistic working set sits well under the cap; it engages only under id churn.
const assetDetailCacheMaxEntries = 4096

// put stores a freshly-rendered response under assetID, last-write-wins: concurrent requests for
// one asset may both compute and write, accepted as a stampede cost rather than adding a
// single-flight layer. Callers pass an already-marshalled body so the lock is not held during
// JSON encoding.
func (c *assetDetailResponseCache) put(assetID string, body []byte, degraded bool) {
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
		degraded: degraded,
	}
}

// evictLocked reclaims room at capacity: it first drops every expired entry, and only if the
// whole map is still fresh evicts the single oldest. Both passes are O(n) with n capped and run
// only at capacity, so they are off the hot path. Caller must hold c.mu.
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

// renderAssetDetailEnvelope builds the wire bytes for an AssetDetail response, mirroring
// writeJSON / writeEnvelope so the cached body matches live output byte-for-byte. On a hit the
// bytes are replayed verbatim: AsOf is frozen at render time and up to ttl stale, with no
// re-splice (that would need re-parsing the body), inside the closed-bucket contract (ADR-0015).
func renderAssetDetailEnvelope(detail AssetDetail, flags Flags) ([]byte, error) {
	env := Envelope{
		Data:  detail,
		AsOf:  WireTime(time.Now().UTC()),
		Flags: flags,
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(env); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeCachedAssetDetail writes a cached entry with writeJSON's header set. The body's AsOf is up
// to ttl stale, which clients can read and which ADR-0015's closed-bucket contract allows.
func writeCachedAssetDetail(w http.ResponseWriter, entry *assetDetailEntry) {
	if entry.degraded {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Stellarindex-Cache", "HIT")
	_, _ = w.Write(entry.body)
}
