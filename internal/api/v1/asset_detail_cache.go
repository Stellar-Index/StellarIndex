package v1

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// assetDetailEntry holds a fully-rendered /v1/assets/{id} response (post-render JSON bytes, flags already encoded) plus its
// cache time, skipping the handler chain and overlays on a hit.
type assetDetailEntry struct {
	body     []byte
	cachedAt time.Time
	// degraded: the body was built over a failed or slow read, so a replay
	// must not carry the route's shared-cache band.
	degraded bool
}

// assetDetailResponseCache caches the whole /v1/assets/{id} response by normalised asset_id, so new fields and overlays flow
// through with no per-reader plumbing. TTL (set in server.New) MUST exceed the selfPrewarmAssetEndpoints cadence, or the cache
// is cold between expiry and the next prewarm. Staleness is inside the closed-bucket contract (ADR-0015).
type assetDetailResponseCache struct {
	mu      sync.RWMutex
	entries map[string]*assetDetailEntry
	ttl     time.Duration
}

// newAssetDetailResponseCache constructs a cache; ttl=0 disables caching (every probe misses).
func newAssetDetailResponseCache(ttl time.Duration) *assetDetailResponseCache {
	return &assetDetailResponseCache{
		entries: make(map[string]*assetDetailEntry),
		ttl:     ttl,
	}
}

// get returns the entry for assetID if present and fresh (cachedAt + ttl > now); otherwise (nil, false).
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

// assetDetailCacheMaxEntries hard-bounds the cache: TTL only checks freshness at READ time and never removes anything, so a
// crawler walking the ~10^5 real ids would slowly OOM. Engages only under id churn.
const assetDetailCacheMaxEntries = 4096

// put stores a rendered response under assetID, last-write-wins: concurrent writers are accepted as a stampede cost over a
// single-flight layer. Callers pass marshalled bytes so the lock is not held during encoding.
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

// evictLocked reclaims room at capacity: drop every expired entry, else the single oldest. O(n) with n capped, only at
// capacity. Caller holds c.mu.
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

// renderAssetDetailEnvelope builds the wire bytes, mirroring writeJSON/writeEnvelope so a cached body matches live output
// byte-for-byte. A hit replays them verbatim: AsOf is frozen at render time and up to ttl stale (ADR-0015).
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
