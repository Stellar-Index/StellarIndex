package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/swrverdict"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
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

// assetListEntry holds one listing page as built, before any exclusion judgement: rendered twice (as built, and with
// flags.stale set for last-good replays, so a hit under an empty exclusion set never re-encodes) and as the envelope a
// hit judges when the set is not empty.
type assetListEntry struct {
	fresh    []byte
	stale    []byte
	env      Envelope
	cachedAt time.Time
	flight   bool
	// refreshFailed: the last refresh errored, panicked or built nothing cacheable; only a put clears it.
	refreshFailed bool
	// rowsFailed re-reads, at each lookup, whether the catalogue's last refresh of the page's rows failed; nil when unknown.
	rowsFailed func() bool
}

// assetListHit is one cache replay: body to write as it is, or env to judge first.
type assetListHit struct {
	body  []byte
	env   Envelope
	stale bool
	// lastGood names why the replay is a last-good copy, "" when it is not.
	lastGood string
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

// lookup returns the body to replay for key. Past the TTL it returns refresh=true to exactly one caller until the refresh
// settles, and the stale-flagged body once the page is a last-good copy per [assetListResponseCache.staleLocked]; past
// maxAge it misses.
func (c *assetListResponseCache) lookup(key string) (hit assetListHit, refresh, ok bool) {
	return c.read(key, true)
}

// peek is lookup for a caller that cannot start the page's refresh: it never claims the flight, so the next lookup still does.
func (c *assetListResponseCache) peek(key string) (assetListHit, bool) {
	hit, _, ok := c.read(key, false)
	return hit, ok
}

func (c *assetListResponseCache) read(key string, claim bool) (hit assetListHit, refresh, ok bool) {
	if c == nil || c.ttl <= 0 {
		return hit, false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[key]
	if !found {
		return hit, false, false
	}
	now := c.now()
	age := now.Sub(e.cachedAt)
	if age > c.maxAge {
		return hit, false, false
	}
	if age >= c.ttl && claim {
		refresh = !e.flight
		e.flight = true
	}
	if !c.staleLocked(e, now) {
		return assetListHit{body: e.fresh, env: e.env}, refresh, true
	}
	env := e.env
	env.Flags.Stale = true
	hit = assetListHit{body: e.stale, env: env, stale: true}
	// Rows whose last refresh failed make the replay a last-good copy. A page refresh that built nothing cacheable does not:
	// a page over rows merely past their TTL is uncacheable too.
	if e.rowsFailed != nil && e.rowsFailed() {
		hit.lastGood, hit.env.Flags.Degraded = obs.DegradedRefreshFailed, true
	}
	return hit, refresh, true
}

// staleLocked judges the page by its own age and its rows by theirs: the rows (env.AsOf when they predate the build) are held
// to the catalogue's bound and its refresh failure, so caching a page cannot replay them clean past either.
func (c *assetListResponseCache) staleLocked(e *assetListEntry, now time.Time) bool {
	rowsAt := e.cachedAt
	if at := e.env.AsOf.Time(); !at.IsZero() && at.Before(rowsAt) {
		rowsAt = at
	}
	return swrverdict.Stale(now.Sub(e.cachedAt), c.ttl, cacheFillBudget, e.refreshFailed) ||
		swrverdict.Stale(now.Sub(rowsAt), AssetCatalogueTTL, assetsRefreshBudget, e.rowsFailed != nil && e.rowsFailed())
}

// assetListRowsOptions is the catalogue listing read behind one default-listing page.
func assetListRowsOptions(filters assetListFilters, cursor string, limit int, order timescale.AssetsOrder) timescale.ListAssetsOptions {
	return timescale.ListAssetsOptions{
		Limit:  limit + AssetsListOverfetchBy,
		Issuer: filters.issuer,
		Code:   filters.code,
		Type:   filters.typ,
		Q:      filters.q,
		Cursor: cursor,
		Order:  order,
	}
}

// catalogueListingFailed binds [Server.catalogueListingsFailed] to one listing read, for a cached page to re-check at lookup.
func (s *Server) catalogueListingFailed(opts timescale.ListAssetsOptions) func() bool {
	return func() bool { return s.catalogueListingsFailed([]timescale.ListAssetsOptions{opts}) }
}

// put renders env and stores it under key, clearing any refresh flight; rowsFailed is the rows' catalogue failure, re-read at
// every lookup.
func (c *assetListResponseCache) put(key string, env Envelope, rowsFailed func() bool) error {
	if c == nil || c.ttl <= 0 {
		return nil
	}
	if env.AsOf.IsZero() {
		env.AsOf = WireTime(time.Now().UTC())
	}
	fresh, err := renderEnvelope(env)
	if err != nil {
		return err
	}
	built := env
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
	c.entries[key] = &assetListEntry{fresh: fresh, stale: stale, env: built, cachedAt: c.now(), rowsFailed: rowsFailed}
	return nil
}

// settleFlight records a refresh that produced nothing cacheable as failed and lets a later request retry.
func (c *assetListResponseCache) settleFlight(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		e.flight, e.refreshFailed = false, true
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

// assetListCacheKey keys a page on the inputs the listing reads, each in its parsed form, so a parameter the handler ignores
// cannot mint a slot (and evict a prewarmed page) while every input that shapes the page still separates one. q is the
// client's form, before any SAC rewrite, so an unread SAC lookup can find the page a resolved one cached.
func assetListCacheKey(r *http.Request) string {
	q := r.URL.Query()
	limit := strings.TrimSpace(q.Get("limit"))
	if limit == "" {
		limit = "100"
	} else if n, err := strconv.Atoi(limit); err == nil {
		limit = strconv.Itoa(n)
	}
	typ := strings.TrimSpace(q.Get("type"))
	if typ == "any" {
		typ = ""
	}
	order := q.Get("order_by")
	if order == "" {
		order = "observation_count_desc"
	}
	return url.Values{
		"asset_class":  {normaliseAssetClass(q.Get("asset_class"))},
		"code":         {strings.TrimSpace(q.Get("code"))},
		"cursor":       {q.Get("cursor")},
		"include":      {strings.Join(requestedSparklines(q.Get("include")), ",")},
		"include_thin": {strconv.FormatBool(includeThinRequested(r))},
		"issuer":       {strings.TrimSpace(q.Get("issuer"))},
		"limit":        {limit},
		"order_by":     {order},
		"q":            {strings.TrimSpace(q.Get("q"))},
		"type":         {typ},
	}.Encode()
}

// assetListPageCacheable reports whether a built page may be replayed: a stale- or degraded-flagged page or one with a
// transient read failure would outlive the outage that shaped it.
func assetListPageCacheable(env Envelope, rows []AssetDetail) bool {
	if env.Flags.Stale || env.Flags.Degraded {
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
		return true, s.assetListCache.put(key, env, s.catalogueListingFailed(assetListRowsOptions(filters, cursor, limit, order)))
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

// writeCachedAssetList replays a cached listing body; a last-good replay is no-store and counted with its reason.
func writeCachedAssetList(w http.ResponseWriter, hit assetListHit) {
	degraded := hit.lastGood != ""
	if degraded {
		w.Header().Set("Cache-Control", "no-store")
		obs.MarkDegradedReason(w, hit.lastGood)
	}
	obs.MarkResponseFlags(w, degraded, hit.stale)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Stellarindex-Cache", "HIT")
	_, _ = w.Write(hit.body)
}

// AssetListPrewarmCadence is how often [Server.PrewarmAssetList] runs: one page TTL.
const AssetListPrewarmCadence = assetListCacheTTL

// PrewarmAssetList serves the default /v1/assets page through the real handler, so a page past its TTL refreshes off the
// request path; run every assetListCacheTTL it keeps the page inside assetListCacheMaxAge for a client polling minutes apart.
func (s *Server) PrewarmAssetList(ctx context.Context) {
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/assets", nil)
	s.handleAssetList(httptest.NewRecorder(), req)
}
