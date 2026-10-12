package explorer

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// This file extends the stale-while-revalidate layer (hot_reads.go) to
// GET /v1/network/throughput. The read is a FINAL scan over up to a year of
// stellar.ledgers, too slow for the inline read timeout, so it must fill detached.
//
//   - ONE cache entry holding the maximum window (365 days); requests slice its tail, since daily
//     buckets are independent aggregates. A single key also stops an unauthenticated caller
//     walking ?window_days=1..365 to buy 365 year-class scans.
//   - `partial` is read from the cached bucket, not recomputed at serve time, so a stale entry or
//     a peer region with a skewed clock cannot mislabel a bucket.

// networkThroughputTTL bounds how stale the cached series may be. Every daily bucket but today's
// is immutable; five minutes matches the API prewarm cadence, so the entry stays fresh.
const networkThroughputTTL = 5 * time.Minute

// networkThroughputMaxWindowDays is the window every cached entry holds: parseWindowDays' own ceiling, so one warm entry serves any
// accepted ?window_days= by slicing its tail.
const networkThroughputMaxWindowDays = 365

// networkThroughputRefreshTimeout bounds one detached recompute: the 365-day FINAL merge can take minutes under replay load, but a
// wedged query must not pin the single-flight forever.
const networkThroughputRefreshTimeout = 3 * time.Minute

// networkThroughputFlightKey is the constant single-flight key (the cache holds one entry, the max window); it exists only to reuse
// perKeyFlight.
const networkThroughputFlightKey = "throughput"

type networkThroughputEntry struct {
	buckets  []clickhouse.ThroughputBucket
	cachedAt time.Time
}

// networkThroughputCache is the single-entry TTL cache behind
// GET /v1/network/throughput. Zero value ready to use.
type networkThroughputCache struct {
	mu     sync.Mutex
	entry  networkThroughputEntry
	has    bool
	flight perKeyFlight
}

// get returns the entry whenever one exists, INCLUDING past the TTL (fresh=false): staleness is the caller's call, so failed refreshes
// degrade to old-but-real data, not 503s. Same contract as assetHoldersCache.get.
func (c *networkThroughputCache) get() (e networkThroughputEntry, ok, fresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.has {
		return networkThroughputEntry{}, false, false
	}
	return c.entry, true, time.Since(c.entry.cachedAt) <= networkThroughputTTL
}

func (c *networkThroughputCache) put(buckets []clickhouse.ThroughputBucket) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entry = networkThroughputEntry{buckets: buckets, cachedAt: time.Now()}
	c.has = true
}

// refreshNetworkThroughput kicks ONE detached recompute of the max-window series (no-op while a flight is up) and returns the flight to
// wait on. Detached because the year-window scan never finishes within a request deadline, so the cache would never fill.
func (h *Handler) refreshNetworkThroughput() *keyFlight {
	fl, owner := h.throughput.flight.begin(networkThroughputFlightKey)
	if !owner {
		return fl
	}
	// Own gate class: the scan shares the explorer lake pool with attacker-keyed caches, so a
	// cold-key burst there cannot starve this refresh (and vice versa). On saturation skip
	// rather than queue; the 5-minute prewarm pass re-kicks.
	gate := h.detachedGate()
	if !gate.TryAcquireClass("network_throughput") {
		h.throughput.flight.end(networkThroughputFlightKey, fl, errRefreshSaturated)
		return fl
	}
	go func() {
		defer gate.ReleaseClass("network_throughput")
		// End the flight from a defer so a panic cannot wedge the
		// throughput series forever — see perKeyFlight.end.
		var err error
		defer func() {
			if rec := recover(); rec != nil {
				worker.Report(h.Logger, "explorer-network-throughput-refresh", rec)
				err = errRefreshPanicked
			}
			h.throughput.flight.end(networkThroughputFlightKey, fl, err)
		}()
		start := time.Now()
		rctx, cancel := context.WithTimeout(context.Background(), networkThroughputRefreshTimeout)
		defer cancel()
		buckets, err := h.Reader.NetworkThroughput(rctx, networkThroughputMaxWindowDays)
		obs.ObserveExplorerSWRRefresh("network_throughput", start, err)
		if err != nil {
			// Keep the previous entry (if any) — old-but-real beats blank.
			h.Logger.Warn("network throughput detached refresh failed", "err", err)
			return
		}
		h.throughput.put(buckets)
	}()
	return fl
}

// networkThroughputCached serves the daily series for `windowDays`: a stale entry is served (degraded=true, real asOf) while a detached
// recompute runs. A never-computed cache waits only to the request deadline; the compute outlives it, so the retry lands warm.
func (h *Handler) networkThroughputCached(ctx context.Context, windowDays int) (buckets []clickhouse.ThroughputBucket, asOf time.Time, degraded bool, err error) {
	if e, ok, fresh := h.throughput.get(); ok {
		if !fresh {
			h.refreshNetworkThroughput() //nolint:contextcheck // intentional detach — the rescan must outlive this request (see refreshNetworkThroughput)
		}
		return sliceThroughput(e.buckets, windowDays), e.cachedAt, !fresh, nil
	}
	fl := h.refreshNetworkThroughput() //nolint:contextcheck // intentional detach — a request that times out must not kill the fill
	select {
	case <-fl.done:
		if e, ok, _ := h.throughput.get(); ok {
			return sliceThroughput(e.buckets, windowDays), e.cachedAt, false, nil
		}
		if fl.err != nil {
			return nil, time.Time{}, false, fl.err
		}
		return nil, time.Time{}, false, errRefreshFailed
	case <-ctx.Done():
		return nil, time.Time{}, false, ctx.Err()
	}
}

// sliceThroughput returns the last `windowDays` buckets of an ascending daily series. A shorter
// series (a lake holding less than the full year) is returned whole, as a direct query would.
func sliceThroughput(rows []clickhouse.ThroughputBucket, windowDays int) []clickhouse.ThroughputBucket {
	if windowDays > 0 && len(rows) > windowDays {
		return rows[len(rows)-windowDays:]
	}
	return rows
}

// PrewarmNetworkThroughput primes /v1/network/throughput by kicking the request path's detached single-flight refresh with the same
// argument, so prewarm and handler never warm different keys.
func (h *Handler) PrewarmNetworkThroughput(ctx context.Context) {
	if h.Reader == nil || ctx.Err() != nil {
		return
	}
	if _, _, fresh := h.throughput.get(); fresh {
		return
	}
	h.refreshNetworkThroughput() //nolint:contextcheck // intentional detach — prewarm kicks the same background compute
}
