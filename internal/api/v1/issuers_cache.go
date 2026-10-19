package v1

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// CachedIssuersReader wraps [IssuersReader] with a per-process TTL cache and single-flight for the expensive list; GetIssuer and ListIssuerAssets pass through.
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

// NewCachedIssuersReader wraps upstream with a TTL cache; ttl=0 disables caching and the caller picks the TTL.
func NewCachedIssuersReader(upstream IssuersReader, ttl time.Duration) *CachedIssuersReader {
	return &CachedIssuersReader{
		upstream: upstream,
		ttl:      ttl,
		entries:  map[string]*issuersCacheEntry{},
	}
}

// GetIssuer is a pass-through: a sub-ms PK lookup that per-strkey caching would only scatter.
func (c *CachedIssuersReader) GetIssuer(ctx context.Context, gStrkey string) (timescale.IssuerRow, error) {
	return c.upstream.GetIssuer(ctx, gStrkey)
}

// ListIssuerAssets is a pass-through bounded by timescale.issuerAssetsHardCap, needed because spammers mint many codes and airdrop trustlines.
func (c *CachedIssuersReader) ListIssuerAssets(ctx context.Context, gStrkey string) ([]timescale.IssuerAsset, error) {
	return c.upstream.ListIssuerAssets(ctx, gStrkey)
}

// ListIssuers is cached as one IssuersListMaxLimit page that every request prefix-slices, so sweeping `?limit=` cannot force one upstream scan per value.
func (c *CachedIssuersReader) ListIssuers(ctx context.Context, limit int) ([]timescale.IssuerSummary, error) {
	list, _, _, err := c.ListIssuersAt(ctx, limit)
	return list, err
}

// ListIssuersAt is ListIssuers plus the served entry's fill time (zero on an uncached, ttl<=0, read) and whether it is past its TTL.
func (c *CachedIssuersReader) ListIssuersAt(ctx context.Context, limit int) ([]timescale.IssuerSummary, time.Time, bool, error) {
	if c.ttl <= 0 {
		list, err := c.upstream.ListIssuers(ctx, limit)
		return list, time.Time{}, false, err
	}
	key := newCacheKey("ListIssuers").int(IssuersListMaxLimit).build()
	list, at, stale, err := c.fetchList(ctx, key, func(ctx context.Context) ([]timescale.IssuerSummary, error) {
		return c.upstream.ListIssuers(ctx, IssuersListMaxLimit)
	})
	if err != nil || limit <= 0 || limit >= len(list) {
		return list, at, stale, err
	}
	// Cap capacity so a caller's append cannot write into the shared entry.
	return list[:limit:limit], at, stale, nil
}

// fetchList is the TTL + single-flight + stale-while-revalidate loop, mirroring CachedMarketsReader.fetchPairs. The scan takes 3-4 s
// on r1, so an expired entry is served (dated by its fill time, flagged stale) while one detached fill replaces it.
func (c *CachedIssuersReader) fetchList(
	ctx context.Context,
	key string,
	upstream func(context.Context) ([]timescale.IssuerSummary, error),
) ([]timescale.IssuerSummary, time.Time, bool, error) {
	c.mu.Lock()
	e, ok := c.entries[key]

	// (A) Fresh hit.
	if ok && e.flight == nil && time.Since(e.at) < c.ttl {
		out, at := e.list, e.at
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("issuers", "list_issuers", "hit").Inc()
		return out, at, false, nil
	}

	// (A') Stale-while-revalidate: a prior successful fill exists (failed cold fills delete the entry) but has expired. Serve it
	// and start at most one detached refresh on the same entry; a failed refresh keeps serving it.
	if ok && !e.at.IsZero() {
		out, at := e.list, e.at
		if e.flight == nil {
			e.flight = make(chan struct{})
			//nolint:gosec,contextcheck // G118 / contextcheck: intentional detached fill — see runDetachedFill.
			go runDetachedFill(nil, "api-issuers-fill", cacheFillBudget, e.flight, upstream, c.settleList(key, e))
		}
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("issuers", "list_issuers", "stale").Inc()
		return out, at, true, nil
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
			return nil, time.Time{}, false, err
		}
		if !leader {
			obs.APICacheOpsTotal.WithLabelValues("issuers", "list_issuers", "hit").Inc()
		}
		return list, at, false, nil
	case <-ctx.Done():
		return nil, time.Time{}, false, ctx.Err()
	}
}

// settleList applies a fetchList fill to entry: cache on success; on failure hand err to waiters and drop the entry, so errors
// are never cached.
func (c *CachedIssuersReader) settleList(key string, entry *issuersCacheEntry) func([]timescale.IssuerSummary, error) {
	return func(rows []timescale.IssuerSummary, err error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			if !entry.at.IsZero() {
				entry.flight = nil
				obs.APICacheOpsTotal.WithLabelValues("issuers", "list_issuers", "refresh_error").Inc()
				return
			}
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
