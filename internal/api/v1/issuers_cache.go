package v1

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// CachedIssuersReader wraps an [IssuersReader] with a per-process TTL cache
// and single-flight refetch.
//
// The list SQL is a full GROUP BY over issuers JOIN classic_assets that no index can avoid,
// and the catalogue moves on a minutes-to-hours timescale, so a TTL-stale ranking is fine.
// GetIssuer + ListIssuerAssets are pass-through: one G-strkey, already index-backed.
//
// Single-flight follows CachedMarketsReader's write-on-success / delete-on-error pattern: a
// waiter holds its own pointer to the entry, so it reads the leader's error even after the
// leader removed the entry from the map.
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

	// err is set by the leader before close(flight) on a failing upstream call. Waiters hold
	// the SAME entry, so they still read it after the leader deletes the entry (errors are
	// not TTL-cached).
	err error
}

// NewCachedIssuersReader wraps `upstream` with a TTL cache. ttl=0 disables caching (every
// call passes through). The caller (cmd/stellarindex-api) picks the TTL.
func NewCachedIssuersReader(upstream IssuersReader, ttl time.Duration) *CachedIssuersReader {
	return &CachedIssuersReader{
		upstream: upstream,
		ttl:      ttl,
		entries:  map[string]*issuersCacheEntry{},
	}
}

// GetIssuer is a pass-through: a sub-ms PK lookup, and per-strkey caching would scatter the
// working set with no shared-callers win.
func (c *CachedIssuersReader) GetIssuer(ctx context.Context, gStrkey string) (timescale.IssuerRow, error) {
	return c.upstream.GetIssuer(ctx, gStrkey)
}

// ListIssuerAssets is a pass-through: an indexed scan bounded by timescale.issuerAssetsHardCap.
// The cap is needed because spammers mint many codes and airdrop trustlines, so the
// per-issuer asset count is not a bound by itself.
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

// fetchList is the TTL + single-flight loop, mirroring CachedMarketsReader.fetchPairs. No
// SWR: the query is ~200ms, so an expired entry waits on a fresh fill rather than serving stale.
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

	// (B)/(C) No fresh value: join the running fill, or take the slot and start one. This
	// caller only waits, on the fill or its own ctx; the fill runs detached (runDetachedFill),
	// so one caller's abort cannot fail the others. The entry pointer is captured so
	// result/err survive the fill's delete-on-error.
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
