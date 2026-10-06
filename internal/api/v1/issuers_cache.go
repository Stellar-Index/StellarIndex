package v1

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// CachedIssuersReader wraps an [IssuersReader] with a per-process
// TTL cache + single-flight refetch. F-0011 audit (2026-05-26)
// measured `/v1/issuers` at p95 ~404ms — over the 200ms SLO. The
// underlying SQL is a 5-table-equivalent aggregate:
//
//	SELECT ... FROM issuers i JOIN classic_assets c USING(g_strkey)
//	 GROUP BY i.g_strkey ... ORDER BY total_obs DESC LIMIT $1
//
// EXPLAIN ANALYZE on r1 (2026-05-26) showed two seq scans
// (issuers ~58k rows + classic_assets ~190k rows) feeding a
// HashAggregate over 57k groups and a top-N heapsort. No single
// index helps because the GROUP BY + sum(observation_count)
// requires the full hashagg regardless of access path. The data
// shape (one row per (g_strkey, asset)) is small enough that the
// scan itself is the right plan — Postgres just has nothing else
// it can do. Query alone is ~196ms; the rest of the p95 budget is
// JSON marshalling + HTTP overhead.
//
// The catalogue moves on the human timescale of "new issuer
// observed on SDEX" (minutes-to-hours) — the underlying ranking by
// 24h+ observation totals isn't materially stale at 5 min. Same
// freshness rationale as CachedSourcesStatsReader / CachedMarketsReader.
//
// GetIssuer + ListIssuerAssets are pass-through — they're keyed
// too narrowly (one G-strkey) to share across callers and the
// underlying queries already hit `issuers_pkey` /
// `classic_assets_issuer_idx`, so they're sub-millisecond at the
// DB layer. Caching them would just add LRU bookkeeping for no
// throughput win.
//
// Single-flight: concurrent callers during a refetch share one
// upstream call. Same write-on-success / delete-on-error /
// waiter-err-pointer pattern as CachedMarketsReader (the proven
// race-clean shape; markets had a panic-on-error-waiter bug
// before that pattern).
type CachedIssuersReader struct {
	upstream IssuersReader
	ttl      time.Duration

	mu      sync.Mutex
	entries map[string]*issuersCacheEntry
}

type issuersCacheEntry struct {
	at     time.Time
	flight chan struct{}

	list []timescale.IssuerSummary

	// err is set by the leader before close(flight) on a failing
	// upstream call. Waiters hold a pointer to the SAME entry they
	// joined the flight on — so even if the leader removes the
	// entry from the map (we don't TTL-cache errors), waiters can
	// still read entry.err here and return it instead of nil-
	// derefing the missing entry. Mirrors CachedMarketsReader's
	// fix.
	err error
}

// NewCachedIssuersReader wraps `upstream` with a TTL cache. ttl=0
// disables caching (every call passes through). 5 min is the
// production default, set by the caller (cmd/stellarindex-api) — the
// verified-issuer catalogue's top-N ranking is stable on the
// timescale of new SDEX activity. There is no config knob for this;
// a deployment needing a different TTL passes one at the call site.
func NewCachedIssuersReader(upstream IssuersReader, ttl time.Duration) *CachedIssuersReader {
	return &CachedIssuersReader{
		upstream: upstream,
		ttl:      ttl,
		entries:  map[string]*issuersCacheEntry{},
	}
}

// GetIssuer — pass-through. Single-row PK lookup on `issuers_pkey`
// is sub-ms; caching by g_strkey would scatter the working set
// across thousands of LRU slots with no shared-callers win.
func (c *CachedIssuersReader) GetIssuer(ctx context.Context, gStrkey string) (timescale.IssuerRow, error) {
	return c.upstream.GetIssuer(ctx, gStrkey)
}

// ListIssuerAssets — pass-through. Indexed scan via
// `classic_assets_issuer_idx`, bounded by timescale.issuerAssetsHardCap
// (500 rows).
//
// It used to be bounded by nothing but the per-issuer asset count, which
// this comment called "typically <20". That was true of a registry fed
// only by trades. Migration 0158 admits every classic asset with a
// trustline, and minting many codes and airdropping trustlines is a spam
// pattern, so the store now applies an explicit cap.
func (c *CachedIssuersReader) ListIssuerAssets(ctx context.Context, gStrkey string) ([]timescale.IssuerAsset, error) {
	return c.upstream.ListIssuerAssets(ctx, gStrkey)
}

// ListIssuers — cached. One entry holds the IssuersListMaxLimit page and
// every request slices its prefix from it: the aggregate's cost is set by
// the scan, not by LIMIT, so a per-limit key would let a caller sweeping
// `?limit=` force one uncollapsed upstream scan per value.
func (c *CachedIssuersReader) ListIssuers(ctx context.Context, limit int) ([]timescale.IssuerSummary, error) {
	list, _, err := c.ListIssuersAt(ctx, limit)
	return list, err
}

// ListIssuersAt is ListIssuers plus the served entry's fill time; zero on
// an uncached (ttl<=0) read.
func (c *CachedIssuersReader) ListIssuersAt(ctx context.Context, limit int) ([]timescale.IssuerSummary, time.Time, error) {
	if c.ttl <= 0 {
		list, err := c.upstream.ListIssuers(ctx, limit)
		return list, time.Time{}, err
	}
	key := newCacheKey("ListIssuers").int(IssuersListMaxLimit).build()
	list, at, err := c.fetchList(ctx, key, func(ctx context.Context) ([]timescale.IssuerSummary, error) {
		return c.upstream.ListIssuers(ctx, IssuersListMaxLimit)
	})
	if err != nil || limit <= 0 || limit >= len(list) {
		return list, at, err
	}
	// Cap capacity so a caller's append cannot write into the shared entry.
	return list[:limit:limit], at, nil
}

// fetchList is the TTL + single-flight loop. Mirrors
// CachedMarketsReader.fetchPairs (delete-on-error,
// waiter-err-pointer panic safety). No SWR — the underlying query
// is ~200ms not multi-second, so an expired entry waits on a fresh
// fill rather than serving stale. (If r1 measurements show
// post-cache p95 still spiking on miss, the swr[T] helper from
// asset_catalogue_cache.go drops in.)
func (c *CachedIssuersReader) fetchList(
	ctx context.Context,
	key string,
	upstream func(context.Context) ([]timescale.IssuerSummary, error),
) ([]timescale.IssuerSummary, time.Time, error) {
	c.mu.Lock()
	e, ok := c.entries[key]

	// (A) Fresh hit.
	if ok && e.flight == nil && time.Since(e.at) < c.ttl {
		out, at := e.list, e.at
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("issuers", "list_issuers", "hit").Inc()
		return out, at, nil
	}

	// (B)/(C) No fresh value: join the running fill, or take the slot
	// and start one. Either way this caller only WAITS, on the fill or its
	// own ctx: the fill runs detached (runDetachedFill), so one caller's abort
	// cannot fail the others. The entry pointer is captured so the
	// result/err is read off the SAME struct we joined, surviving the
	// fill's delete-on-error.
	entry, leader := e, false
	if !ok || e.flight == nil {
		entry, leader = &issuersCacheEntry{flight: make(chan struct{})}, true
		c.entries[key] = entry
		//nolint:gosec,contextcheck // G118 / contextcheck: intentional
		// detached fill — see runDetachedFill.
		go runDetachedFill(nil, "api-issuers-fill", cacheFillBudget, entry.flight, upstream, c.settleList(key, entry))
	}
	ch := entry.flight
	c.mu.Unlock()
	if leader {
		obs.APICacheOpsTotal.WithLabelValues("issuers", "list_issuers", "miss").Inc()
	}
	select {
	case <-ch:
		c.mu.Lock()
		list, at, err := entry.list, entry.at, entry.err
		c.mu.Unlock()
		if err != nil {
			if !leader {
				obs.APICacheOpsTotal.WithLabelValues("issuers", "list_issuers", "miss").Inc()
			}
			return nil, time.Time{}, err
		}
		if !leader {
			obs.APICacheOpsTotal.WithLabelValues("issuers", "list_issuers", "hit").Inc()
		}
		return list, at, nil
	case <-ctx.Done():
		return nil, time.Time{}, ctx.Err()
	}
}

// settleList applies a fetchList fill to entry: cache on success; on
// failure hand err to the waiters and drop the entry so errors are never
// cached.
func (c *CachedIssuersReader) settleList(key string, entry *issuersCacheEntry) func([]timescale.IssuerSummary, error) {
	return func(rows []timescale.IssuerSummary, err error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			entry.err = err
			if c.entries[key] == entry {
				delete(c.entries, key)
			}
			return
		}
		entry.at = time.Now()
		entry.list = rows
		entry.flight = nil
	}
}
