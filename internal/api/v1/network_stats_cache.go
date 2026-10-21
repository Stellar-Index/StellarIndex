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

// errNetworkStatsColdFailed goes to waiters of a failed cold fetch with no stale value; the handler maps it to 500, like an uncached
// error.
var errNetworkStatsColdFailed = errors.New("network stats: cold fetch failed")

// CachedNetworkStatsReader is a single-key stale-while-revalidate cache over the slow network-wide 24h aggregate, so no request blocks on the upstream.
type CachedNetworkStatsReader struct {
	// logger sinks a panic recovered in a detached refresh goroutine. It is the process default,
	// not the Server's logger, because readers are built before the Server exists. The
	// load-bearing signal is stellarindex_worker_panics_total, which worker.Report moves
	// regardless of the sink.
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

// NewCachedNetworkStatsReader wraps upstream; ttl<=0 disables the cache and 30s is the production default.
func NewCachedNetworkStatsReader(upstream NetworkStatsReader, ttl time.Duration) *CachedNetworkStatsReader {
	return &CachedNetworkStatsReader{logger: slog.Default(), upstream: upstream, ttl: ttl}
}

// GetNetworkStats implements NetworkStatsReader with SWR semantics, dropping the freshness signals GetNetworkStatsAt returns.
func (c *CachedNetworkStatsReader) GetNetworkStats(ctx context.Context) (timescale.NetworkStats, error) {
	v, _, _, err := c.GetNetworkStatsAt(ctx)
	return v, err
}

// GetNetworkStatsAt is GetNetworkStats plus observation time and a served-stale flag; a zero observedAt means a live uncached read.
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
			// the refresh must use a fresh context (runDetachedFill), not the request ctx,
			// which is cancelled as soon as the stale response is written.
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

// settle applies a detached fill's outcome: success swaps val+at; failure keeps any stale value and records err for cold waiters.
// Clearing the in-flight marker makes the next request retry.
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
		c.logger.Warn("cache refresh failed; serving stale value", "cache", "network_stats", "op", "get", "err", err)
	}
}
