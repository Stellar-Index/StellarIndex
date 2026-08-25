package v1

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// errNetworkStatsColdFailed is returned to a waiter that joined a cold
// fetch which then failed upstream (no stale value to serve). The handler
// maps it to a 500 — same outcome as an uncached upstream error.
var errNetworkStatsColdFailed = errors.New("network stats: cold fetch failed")

// CachedNetworkStatsReader wraps a [NetworkStatsReader] with a single-key
// stale-while-revalidate cache. GetNetworkStats runs a network-wide 24h
// aggregate over the served tier (~485ms p95 on r1, the slowest /v1 route)
// for /v1/network/stats — the explorer's network strip. The figures
// (24h volume, market/asset counts, latest ledger) move slowly, so the
// SWR contract — serve the cached value instantly, revalidate in a single
// background flight, never block a request on the slow upstream — keeps it
// off the request path with no material staleness. Same shape as the
// coins/markets caches (asset_catalogue_cache.go), reduced to one key.
type CachedNetworkStatsReader struct {
	upstream NetworkStatsReader
	ttl      time.Duration

	mu     sync.Mutex
	at     time.Time
	val    timescale.NetworkStats
	hasVal bool
	flight chan struct{}
}

// NewCachedNetworkStatsReader wraps upstream. ttl<=0 disables the cache
// (every call passes through). 30s is the production default — matches the
// coins cache and is far below the cadence at which these aggregates move.
func NewCachedNetworkStatsReader(upstream NetworkStatsReader, ttl time.Duration) *CachedNetworkStatsReader {
	return &CachedNetworkStatsReader{upstream: upstream, ttl: ttl}
}

// GetNetworkStats implements NetworkStatsReader with SWR semantics. It
// delegates to GetNetworkStatsAt and drops the freshness signals for
// callers that don't stamp them.
func (c *CachedNetworkStatsReader) GetNetworkStats(ctx context.Context) (timescale.NetworkStats, error) {
	v, _, _, err := c.GetNetworkStatsAt(ctx)
	return v, err
}

// GetNetworkStatsAt is GetNetworkStats plus honest freshness: it returns
// the served value's observation time and whether it came from the SWR
// stale-serve path (the value is a prior success the cache kept past its
// TTL while a refresh is pending/failing). The handler stamps flags.stale
// + an honest as_of from these, instead of silently asserting stale:false
// / as_of=now over data a failing refresh has let age (REC-05; mirrors
// CachedMarketsReader.SourceMarketsAt). A zero observedAt means "live /
// uncached read" — the handler defaults as_of to now and stale=false.
func (c *CachedNetworkStatsReader) GetNetworkStatsAt(ctx context.Context) (timescale.NetworkStats, time.Time, bool, error) {
	if c.ttl <= 0 {
		v, err := c.upstream.GetNetworkStats(ctx)
		return v, time.Time{}, false, err // live read: caller stamps as_of=now, not stale
	}
	c.mu.Lock()

	// (A) Fresh hit.
	if c.hasVal && c.flight == nil && time.Since(c.at) < c.ttl {
		v, at := c.val, c.at
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("network_stats", "get", "hit").Inc()
		return v, at, false, nil
	}

	// (A') Stale-while-revalidate: a prior success exists but is expired.
	// Serve stale immediately; kick exactly one background refresh. The
	// returned observedAt (the aged c.at) + stale=true let the handler
	// tell the truth about the freshness of what it just served.
	if c.hasVal {
		v, at := c.val, c.at
		if c.flight == nil {
			done := make(chan struct{})
			c.flight = done
			c.mu.Unlock()
			obs.APICacheOpsTotal.WithLabelValues("network_stats", "get", "stale").Inc()
			//nolint:gosec,contextcheck // G118 / contextcheck: intentional —
			// the SWR background refresh MUST use a fresh context (refresh ->
			// context.Background), NOT the request ctx, which is cancelled the
			// instant the stale response is written; reusing it would abort
			// every refresh, defeating the point of serving stale.
			go c.refresh(done)
			return v, at, true, nil
		}
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("network_stats", "get", "stale").Inc()
		return v, at, true, nil
	}

	// (B) Cold fetch already in flight (nothing stale to serve) — join it.
	if c.flight != nil {
		ch := c.flight
		c.mu.Unlock()
		select {
		case <-ch:
			c.mu.Lock()
			v, at, ok := c.val, c.at, c.hasVal
			c.mu.Unlock()
			if !ok {
				obs.APICacheOpsTotal.WithLabelValues("network_stats", "get", "miss").Inc()
				return timescale.NetworkStats{}, time.Time{}, false, errNetworkStatsColdFailed
			}
			obs.APICacheOpsTotal.WithLabelValues("network_stats", "get", "hit").Inc()
			return v, at, false, nil // freshly refreshed by the leader — not stale
		case <-ctx.Done():
			return timescale.NetworkStats{}, time.Time{}, false, ctx.Err()
		}
	}

	// (C) Cold leader: block inline — nothing stale to serve.
	done := make(chan struct{})
	c.flight = done
	c.mu.Unlock()
	obs.APICacheOpsTotal.WithLabelValues("network_stats", "get", "miss").Inc()

	v, err := c.upstream.GetNetworkStats(ctx)

	c.mu.Lock()
	var at time.Time
	if err == nil {
		c.val = v
		c.at = time.Now()
		c.hasVal = true
		at = c.at
	}
	c.flight = nil
	c.mu.Unlock()
	close(done)
	return v, at, false, err // just fetched live — not stale
}

// refresh runs the upstream call OFF the request path for the (A') branch:
// a fresh background context (the triggering request's ctx dies the instant
// the stale response is written), on success swaps val+at under the lock, on
// failure keeps the stale value and only clears the in-flight marker.
func (c *CachedNetworkStatsReader) refresh(done chan struct{}) {
	defer close(done)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	v, err := c.upstream.GetNetworkStats(ctx)

	c.mu.Lock()
	if err == nil {
		c.val = v
		c.at = time.Now()
		c.hasVal = true
	}
	c.flight = nil
	c.mu.Unlock()

	if err != nil {
		obs.APICacheOpsTotal.WithLabelValues("network_stats", "get", "refresh_error").Inc()
	}
}
