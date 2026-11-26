package v1

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// assetDetailEntry holds a fully-rendered /v1/assets/{id} response (post-render JSON bytes, flags already encoded) plus its
// cache time, skipping the handler chain and overlays on a hit. body, detail and flags are as built, before any exclusion
// judgement: a hit under an empty exclusion set writes body, otherwise it judges detail.
type assetDetailEntry struct {
	body      []byte
	detail    AssetDetail
	flags     Flags
	cachedAt  time.Time
	expiresAt time.Time
	// degraded: the body was built over a failed or slow read, so a replay
	// must not carry the route's shared-cache band.
	degraded bool
	stale    bool
	// reason is the [detailDegradedReason] of a degraded entry, replayed on every serve.
	reason string
}

// assetDetailDegradedTTL is the life of a degraded entry or one over a stale catalogue value: it absorbs a burst, while the
// answer its read's detached refresh lands within seconds replaces it instead of waiting out the full TTL.
const assetDetailDegradedTTL = 3 * time.Second

// assetDetailResponseCache caches the whole /v1/assets/{id} response by normalised asset_id, so new fields and overlays flow
// through with no per-reader plumbing. TTL (set in server.New) MUST exceed the selfPrewarmAssetEndpoints cadence, or the cache
// is cold between expiry and the next prewarm. Staleness is inside the closed-bucket contract (ADR-0015).
type assetDetailResponseCache struct {
	mu      sync.RWMutex
	entries map[string]*assetDetailEntry
	ttl     time.Duration
	now     func() time.Time
}

// newAssetDetailResponseCache constructs a cache; ttl=0 disables caching (every probe misses).
func newAssetDetailResponseCache(ttl time.Duration) *assetDetailResponseCache {
	return &assetDetailResponseCache{
		entries: make(map[string]*assetDetailEntry),
		ttl:     ttl,
		now:     time.Now,
	}
}

// get returns the entry for assetID if present and unexpired; otherwise (nil, false).
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
	if c.now().After(e.expiresAt) {
		return nil, false
	}
	return e, true
}

// assetDetailCacheMaxEntries hard-bounds the cache: TTL only checks freshness at READ time and never removes anything, so a
// crawler walking the ~10^5 real ids would slowly OOM. Engages only under id churn.
const assetDetailCacheMaxEntries = 4096

// newAssetDetailEntry is the entry for one built detail and its rendered body.
func newAssetDetailEntry(detail AssetDetail, body []byte, flags Flags) *assetDetailEntry {
	return &assetDetailEntry{body: body, detail: detail, flags: flags, degraded: flags.Degraded, stale: flags.Stale}
}

// put stores an entry under assetID, last-write-wins: concurrent writers are accepted as a stampede cost over a
// single-flight layer. Callers pass marshalled bytes so the lock is not held during encoding.
func (c *assetDetailResponseCache) put(assetID string, e *assetDetailEntry) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ttl := c.ttl
	// A catalogue value past its TTL has its refresh running: the body that refresh lands within seconds replaces it.
	if e.degraded || e.detail.extensionStale {
		ttl = min(ttl, assetDetailDegradedTTL)
	}
	e.cachedAt = c.now()
	e.expiresAt = e.cachedAt.Add(ttl)
	if _, exists := c.entries[assetID]; !exists && len(c.entries) >= assetDetailCacheMaxEntries {
		c.evictLocked()
	}
	c.entries[assetID] = e
}

// evictLocked reclaims room at capacity: drop every expired entry, else the single oldest. O(n) with n capped, only at
// capacity. Caller holds c.mu.
func (c *assetDetailResponseCache) evictLocked() {
	before := len(c.entries)
	now := c.now()
	for id, e := range c.entries {
		if now.After(e.expiresAt) {
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
	if entry.degraded && entry.reason != "" {
		obs.MarkDegradedReason(w, entry.reason)
	}
	obs.MarkResponseFlags(w, entry.degraded, entry.stale)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Stellarindex-Cache", "HIT")
	_, _ = w.Write(entry.body)
}

// isPrewarmRequest is the API's own loopback prewarm (selfPrewarmAssetEndpoints), never a client claiming its User-Agent.
func isPrewarmRequest(r *http.Request) bool {
	return obs.IsSyntheticRequest(r) && strings.HasPrefix(r.UserAgent(), "stellarindex-prewarm/")
}
