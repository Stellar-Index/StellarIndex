package v1

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// logger sinks a panic recovered in a detached refresh goroutine.
	// It is the PROCESS DEFAULT rather than the API Server's logger:
	// these reader wrappers are constructed directly in
	// cmd/stellarindex-api/main.go before the Server exists, and
	// widening the exported constructor would churn every caller for a
	// sink that ends up in the same journal either way. The
	// load-bearing signal is stellarindex_worker_panics_total, which
	// worker.Report moves regardless of the sink.
	logger *slog.Logger

	upstream NetworkStatsReader
	ttl      time.Duration

	mu     sync.Mutex
	at     time.Time
	val    timescale.NetworkStats
	hasVal bool
	// err is the last fill's failure, handed to cold waiters.
	err    error
	flight chan struct{}
}

// NewCachedNetworkStatsReader wraps upstream. ttl<=0 disables the cache
// (every call passes through). 30s is the production default — matches the
// coins cache and is far below the cadence at which these aggregates move.
func NewCachedNetworkStatsReader(upstream NetworkStatsReader, ttl time.Duration) *CachedNetworkStatsReader {
	return &CachedNetworkStatsReader{logger: slog.Default(), upstream: upstream, ttl: ttl}
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
// / as_of=now over data a failing refresh has let age (mirrors
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
			// the SWR background refresh MUST use a fresh context
			// (runDetachedFill), NOT the request ctx, which is cancelled the
			// instant the stale response is written; reusing it would abort
			// every refresh, defeating the point of serving stale.
			go runDetachedFill(c.logger, "api-network-stats-refresh", cacheFillBudget, done, c.upstream.GetNetworkStats, c.settle)
			return v, at, true, nil
		}
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("network_stats", "get", "stale").Inc()
		return v, at, true, nil
	}

	// (B)/(C) Cold: nothing stale to serve. Join the running fill or start
	// one, then wait on it OR this caller's ctx. The fill never runs on a
	// request ctx, so one caller's abort cannot fail the others.
	ch, leader := c.flight, c.flight == nil
	if leader {
		ch = make(chan struct{})
		c.flight = ch
		//nolint:gosec,contextcheck // G118 / contextcheck: intentional
		// detached fill — see runDetachedFill.
		go runDetachedFill(c.logger, "api-network-stats-refresh", cacheFillBudget, ch, c.upstream.GetNetworkStats, c.settle)
	}
	c.mu.Unlock()
	if leader {
		obs.APICacheOpsTotal.WithLabelValues("network_stats", "get", "miss").Inc()
	}
	select {
	case <-ch:
		c.mu.Lock()
		v, at, ok, fillErr := c.val, c.at, c.hasVal, c.err
		c.mu.Unlock()
		if !ok {
			if !leader {
				obs.APICacheOpsTotal.WithLabelValues("network_stats", "get", "miss").Inc()
			}
			if fillErr != nil {
				return timescale.NetworkStats{}, time.Time{}, false, fmt.Errorf("%w: %w", errNetworkStatsColdFailed, fillErr)
			}
			return timescale.NetworkStats{}, time.Time{}, false, errNetworkStatsColdFailed
		}
		if !leader {
			obs.APICacheOpsTotal.WithLabelValues("network_stats", "get", "hit").Inc()
		}
		return v, at, false, nil // freshly filled — not stale
	case <-ctx.Done():
		return timescale.NetworkStats{}, time.Time{}, false, ctx.Err()
	}
}

// settle applies a fill's outcome (the upstream call ran detached, never
// on a request ctx). On success it swaps val+at; on failure it keeps any
// stale value and records err for cold waiters. Clearing the in-flight
// marker is what makes the next request retry.
func (c *CachedNetworkStatsReader) settle(v timescale.NetworkStats, err error) {
	c.mu.Lock()
	c.err = err
	if err == nil {
		c.val = v
		c.at = time.Now()
		c.hasVal = true
	}
	stale := c.hasVal
	c.flight = nil
	c.mu.Unlock()

	if err != nil && stale {
		obs.APICacheOpsTotal.WithLabelValues("network_stats", "get", "refresh_error").Inc()
	}
}
