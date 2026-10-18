package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// assetListCacheTTL is how long a rendered /v1/assets page is fresh; the overlays are each cached a minute or more, so 30s adds
// little age while removing the fan-out from the request path.
const assetListCacheTTL = 30 * time.Second

// assetListCacheMaxAge bounds stale-while-revalidate: past it a page is rebuilt on the request. Matches the listing rows'
// 2-minute cache, so a replayed page is never older than its rows could be.
const assetListCacheMaxAge = 2 * time.Minute

// assetListCacheMaxEntries bounds the cache; the key carries free-text
// `q` and cursors, so without a cap a crawler grows it without limit.
const assetListCacheMaxEntries = 512

// assetListEntry holds one rendered listing page twice: as built, and with
// flags.stale set for replays past the TTL, so a hit never re-encodes.
type assetListEntry struct {
	fresh    []byte
	stale    []byte
	cachedAt time.Time
	flight   bool
}

// assetListResponseCache caches the rendered default /v1/assets listing (handleAssetListFromAssets), covering all the
// per-request overlay fan-out, as assetDetailResponseCache does for detail.
type assetListResponseCache struct {
	mu      sync.Mutex
	entries map[string]*assetListEntry
	ttl     time.Duration
	maxAge  time.Duration
	now     func() time.Time
}

func newAssetListResponseCache(ttl, maxAge time.Duration) *assetListResponseCache {
	return &assetListResponseCache{
		entries: make(map[string]*assetListEntry),
		ttl:     ttl,
		maxAge:  maxAge,
		now:     time.Now,
	}
}

// lookup returns the body to replay for key. Past the TTL it returns the stale-flagged body and refresh=true to exactly one
// caller until the refresh settles; past maxAge it misses.
func (c *assetListResponseCache) lookup(key string) (body []byte, refresh, ok bool) {
	if c == nil || c.ttl <= 0 {
		return nil, false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[key]
	if !found {
		return nil, false, false
	}
	age := c.now().Sub(e.cachedAt)
	switch {
	case age < c.ttl:
		return e.fresh, false, true
	case age <= c.maxAge:
		refresh = !e.flight
		e.flight = true
		return e.stale, refresh, true
	default:
		return nil, false, false
	}
}

// put renders env and stores it under key, clearing any refresh flight.
func (c *assetListResponseCache) put(key string, env Envelope) error {
	if c == nil || c.ttl <= 0 {
		return nil
	}
	fresh, err := renderEnvelope(env)
	if err != nil {
		return err
	}
	env.Flags.Stale = true
	stale, err := renderEnvelope(env)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; !exists && len(c.entries) >= assetListCacheMaxEntries {
		c.evictLocked()
	}
	c.entries[key] = &assetListEntry{fresh: fresh, stale: stale, cachedAt: c.now()}
	return nil
}

// settleFlight lets a later request retry after a refresh that produced
// nothing cacheable.
func (c *assetListResponseCache) settleFlight(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		e.flight = false
	}
}

// evictLocked drops every entry past maxAge, else the oldest one.
func (c *assetListResponseCache) evictLocked() {
	now := c.now()
	oldestKey, oldestAt := "", time.Time{}
	for k, e := range c.entries {
		if now.Sub(e.cachedAt) > c.maxAge {
			delete(c.entries, k)
			continue
		}
		if oldestKey == "" || e.cachedAt.Before(oldestAt) {
			oldestKey, oldestAt = k, e.cachedAt
		}
	}
	if len(c.entries) >= assetListCacheMaxEntries && oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

// renderEnvelope encodes env exactly as writeEnvelope does.
func renderEnvelope(env Envelope) ([]byte, error) {
	if env.AsOf.IsZero() {
		env.AsOf = WireTime(time.Now().UTC())
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(env); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// assetListCacheKey keys a page on its whole query string, canonically
// ordered, so any parameter that shapes the page also separates the slot.
func assetListCacheKey(r *http.Request) string {
	return r.URL.Query().Encode()
}

// assetListPageCacheable reports whether a built page may be replayed: a stale-flagged page or one with a transient read
// failure would outlive the outage that shaped it.
func assetListPageCacheable(env Envelope, rows []AssetDetail) bool {
	if env.Flags.Stale {
		return false
	}
	for i := range rows {
		if rows[i].DecimalsUnresolved || rows[i].issuerDirectoryUnchecked {
			return false
		}
	}
	return true
}

// refreshAssetListPage rebuilds a listing page off the request path and stores it when cacheable; the clone carries r's query
// onto a detached build on its own budget.
func (s *Server) refreshAssetListPage(
	r *http.Request, key string, filters assetListFilters, cursor string, limit int, order timescale.AssetsOrder,
) {
	req := r.Clone(context.Background())
	build := func(ctx context.Context) (bool, error) {
		env, rows, err := s.buildAssetListPage(req.WithContext(ctx), filters, cursor, limit, order)
		if err != nil || ctx.Err() != nil || !assetListPageCacheable(env, rows) {
			return false, err
		}
		return true, s.assetListCache.put(key, env)
	}
	settle := func(cached bool, err error) {
		if err != nil {
			s.logger.Warn("assets listing background refresh failed", "err", err)
		}
		if !cached || err != nil {
			s.assetListCache.settleFlight(key)
		}
	}
	go runDetachedFill(s.logger, "api-assets-listing-page-refresh", cacheFillBudget, make(chan struct{}), build, settle)
}

// writeCachedAssetList replays a cached listing body.
func writeCachedAssetList(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Stellarindex-Cache", "HIT")
	_, _ = w.Write(body)
}

// AssetListPrewarmCadence is how often [Server.PrewarmAssetList] runs: one page TTL.
const AssetListPrewarmCadence = assetListCacheTTL

// PrewarmAssetList serves the default /v1/assets page through the real handler, so a page past its TTL refreshes off the
// request path; run every assetListCacheTTL it keeps the page inside assetListCacheMaxAge for a client polling minutes apart.
func (s *Server) PrewarmAssetList(ctx context.Context) {
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/assets", nil)
	s.handleAssetList(httptest.NewRecorder(), req)
}
