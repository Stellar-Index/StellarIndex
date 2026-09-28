package v1

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// CachedSourcesStatsReader wraps a [SourcesStatsReader] with a
// per-process TTL cache. The underlying SQL aggregations scan ~24h
// of the trades hypertable (millions of rows) and take 5-10s; for
// /v1/sources?include=stats they fire on every page load. Caching
// the result for ttl=60s drops p95 from seconds to microseconds
// without making the data measurably less fresh — these are 24h-
// trailing aggregates that don't move materially in 60s.
//
// Single-flight: concurrent callers during a refetch share one
// upstream call, not N. Important when the perf-bug page-load
// triggers two parallel fetches in the explorer (the table + the
// home strip both want stats).
type CachedSourcesStatsReader struct {
	upstream SourcesStatsReader
	ttl      time.Duration

	mu     sync.Mutex
	stats  sourcesStatsSlot[timescale.SourceStats]
	hist   sourcesStatsSlot[timescale.SourceVolumeBucket]
	hist7d sourcesStatsSlot[timescale.SourceVolumeBucket]
}

// sourcesStatsSlot is one cached method's value, guarded by the reader's mu.
type sourcesStatsSlot[T any] struct {
	rows   []T
	at     time.Time
	err    error
	flight chan struct{}
}

// NewCachedSourcesStatsReader wraps `upstream` with a TTL cache.
// ttl=0 disables the cache (every call goes through). 60s is the
// typical production value.
func NewCachedSourcesStatsReader(upstream SourcesStatsReader, ttl time.Duration) *CachedSourcesStatsReader {
	return &CachedSourcesStatsReader{upstream: upstream, ttl: ttl}
}

// GetSourceStats returns the cached value when fresh; otherwise
// triggers exactly one upstream refetch (sharing it across all
// concurrent callers).
func (c *CachedSourcesStatsReader) GetSourceStats(ctx context.Context) ([]timescale.SourceStats, error) {
	if c.ttl <= 0 {
		return c.upstream.GetSourceStats(ctx)
	}
	return fetchSourcesSlot(ctx, c, &c.stats, "source_stats", c.upstream.GetSourceStats)
}

// GetSourceVolumeHistory24h: same shape as GetSourceStats.
func (c *CachedSourcesStatsReader) GetSourceVolumeHistory24h(ctx context.Context) ([]timescale.SourceVolumeBucket, error) {
	if c.ttl <= 0 {
		return c.upstream.GetSourceVolumeHistory24h(ctx)
	}
	return fetchSourcesSlot(ctx, c, &c.hist, "volume_history_24h", c.upstream.GetSourceVolumeHistory24h)
}

// GetSourceVolumeHistory7d: same single-flight TTL pattern as the 24h
// variant, on its own cache slot.
func (c *CachedSourcesStatsReader) GetSourceVolumeHistory7d(ctx context.Context) ([]timescale.SourceVolumeBucket, error) {
	if c.ttl <= 0 {
		return c.upstream.GetSourceVolumeHistory7d(ctx)
	}
	return fetchSourcesSlot(ctx, c, &c.hist7d, "volume_history_7d", c.upstream.GetSourceVolumeHistory7d)
}

// fetchSourcesSlot is the TTL + single-flight loop shared by the three
// methods. A stale or empty slot joins the running fill or starts one,
// then waits on it OR this caller's ctx. The fill runs detached
// (runDetachedFill), so one caller's abort cannot fail the others. The
// waiter reads the fill's error, never the slot contents alone, so a
// failed fill is not served as a successful empty result.
func fetchSourcesSlot[T any](
	ctx context.Context,
	c *CachedSourcesStatsReader,
	slot *sourcesStatsSlot[T],
	op string,
	upstream func(context.Context) ([]T, error),
) ([]T, error) {
	c.mu.Lock()
	if time.Since(slot.at) < c.ttl && slot.rows != nil {
		out := slot.rows
		c.mu.Unlock()
		obs.APICacheOpsTotal.WithLabelValues("sources_stats", op, "hit").Inc()
		return out, nil
	}

	ch, leader := slot.flight, slot.flight == nil
	if leader {
		ch = make(chan struct{})
		slot.flight = ch
		//nolint:gosec,contextcheck // G118 / contextcheck: intentional
		// detached fill — see runDetachedFill.
		go runDetachedFill(nil, "api-sources-stats-fill", cacheFillBudget, ch, upstream, settleSourcesSlot(c, slot))
	}
	c.mu.Unlock()
	if leader {
		obs.APICacheOpsTotal.WithLabelValues("sources_stats", op, "miss").Inc()
	}

	select {
	case <-ch:
		c.mu.Lock()
		out, refetchErr := slot.rows, slot.err
		c.mu.Unlock()
		if refetchErr != nil {
			if !leader {
				obs.APICacheOpsTotal.WithLabelValues("sources_stats", op, "error").Inc()
			}
			return nil, refetchErr
		}
		if !leader {
			obs.APICacheOpsTotal.WithLabelValues("sources_stats", op, "hit").Inc()
		}
		return out, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// settleSourcesSlot applies a fill's outcome to slot.
func settleSourcesSlot[T any](c *CachedSourcesStatsReader, slot *sourcesStatsSlot[T]) func([]T, error) {
	return func(rows []T, err error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		slot.err = err
		if err == nil {
			slot.rows = rows
			slot.at = time.Now()
		}
		slot.flight = nil
	}
}
