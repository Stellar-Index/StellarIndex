package explorer

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// This file extends the stale-while-revalidate layer (hot_reads.go) to
// GET /v1/network/throughput — the /network page's daily
// ledger/tx/op/event series and chain-economics panel.
//
// Why (§2.6b grounding incident, 2026-08-13): the read is a FINAL scan
// over up to a YEAR of stellar.ledgers with three argMax columns, and it
// ran INLINE on the 8-second explorerReadTimeout on every request. On a
// cold or loaded box it missed that budget, the handler 503'd (or the
// page rendered the panel absent), and — because the scan died WITH the
// request — no retry could ever land warm. Same failure shape the
// holders board and the contracts directory already fixed here.
//
// Two properties beyond the plain SWR shape:
//
//   - ONE cache entry, not one per window. The entry always holds the
//     MAXIMUM window (365 days) and every request slices its tail, exactly
//     as the holders/contracts caches hold the maximum page and slice by
//     `limit`. Daily buckets are independent aggregates, so the last N
//     buckets of the 365-day series ARE the N-day series — no rounding,
//     no ladder, no echo change. It also collapses the key space to 1:
//     pre-fix an unauthenticated caller could walk ?window_days=1..365 and
//     buy 365 distinct year-class scans (the C3-009 amplification shape).
//   - `partial` is recomputed at SERVE time, not read from the cached
//     bucket. Only a bucket covering today is partial; an entry that was
//     computed before UTC midnight would otherwise keep flagging
//     yesterday — a complete day — as still accumulating.

// networkThroughputTTL bounds how stale the cached series may be. The
// panel is a DAILY aggregate over closed ledgers: every bucket but
// today's is immutable, and today's moves by ~1/17,280th of a day per
// ledger. Five minutes matches the API's prewarm cadence exactly (as it
// does for opTypeStats and the contracts directory), so the entry is
// permanently fresh in production.
const networkThroughputTTL = 5 * time.Minute

// networkThroughputMaxWindowDays is the window every cached entry holds.
// It matches parseWindowDays' own ceiling, so one warm entry can serve
// any accepted ?window_days= by slicing its tail.
const networkThroughputMaxWindowDays = 365

// networkThroughputRefreshTimeout bounds one detached recompute. The
// 365-day rung is a FINAL merge over ~5.5M ledger rows — seconds-class
// warm, minutes-class when the lake is under replay load; generous so a
// refresh that would have succeeded isn't abandoned, bounded so a wedged
// query can't pin the single-flight forever.
const networkThroughputRefreshTimeout = 3 * time.Minute

// networkThroughputFlightKey is the single-flight key. The cache holds
// exactly one entry (the max window), so the key is a constant — it
// exists only to reuse perKeyFlight.
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

// get returns the entry whenever one exists — INCLUDING past the TTL
// (fresh=false); staleness is the caller's judgment, so a run of failed
// refreshes degrades to old-but-real data instead of 503s. Same contract
// as assetHoldersCache.get.
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

// refreshNetworkThroughput kicks ONE detached recompute of the max-window
// series (no-op when a flight is already up) and returns the flight to
// optionally wait on. Detached for the same reason as every sibling here:
// bound to a request deadline the year-window scan never finished, so the
// cache never filled and every request kept paying the timeout.
func (h *Handler) refreshNetworkThroughput() *keyFlight {
	fl, owner := h.throughput.flight.begin(networkThroughputFlightKey)
	if !owner {
		return fl
	}
	// Own gate CLASS: the key space here is a single entry, but the scan
	// contends on the same 8-connection lake pool as the attacker-keyed
	// caches. A distinct class means a cold-key burst on holders /
	// contract-detail can never starve this refresh (and vice versa),
	// while the global bound still holds. On saturation skip, don't queue
	// (see detachedGate); the 5-minute prewarm pass re-kicks.
	gate := h.detachedGate()
	if !gate.TryAcquireClass("network_throughput") {
		h.throughput.flight.end(networkThroughputFlightKey, fl, errRefreshSaturated)
		return fl
	}
	go func() {
		defer gate.ReleaseClass("network_throughput")
		start := time.Now()
		rctx, cancel := context.WithTimeout(context.Background(), networkThroughputRefreshTimeout)
		defer cancel()
		buckets, err := h.Reader.NetworkThroughput(rctx, networkThroughputMaxWindowDays)
		obs.ObserveExplorerSWRRefresh("network_throughput", start, err)
		if err != nil {
			// Keep the previous entry (if any) — old-but-real beats blank.
			h.Logger.Warn("network throughput detached refresh failed", "err", err)
			h.throughput.flight.end(networkThroughputFlightKey, fl, err)
			return
		}
		h.throughput.put(buckets)
		h.throughput.flight.end(networkThroughputFlightKey, fl, nil)
	}()
	return fl
}

// networkThroughputCached serves the daily series for `windowDays` from
// the cache: fresh entry sliced as-is; STALE entry served (degraded=true,
// with its real asOf) while a detached single-flight recompute runs;
// never-computed waits for the detached compute up to the REQUEST
// deadline only (the compute itself outlives it, so a cold process 503s
// that one request and the retry lands warm — and PrewarmNetworkThroughput
// means production never meets this branch).
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

// sliceThroughput returns the last `windowDays` buckets of an ascending
// daily series — the N-day window is the tail of the cached max window.
// A shorter cached series (a lake holding less than the full year) is
// returned whole, which is exactly what a direct query would have
// produced.
func sliceThroughput(rows []clickhouse.ThroughputBucket, windowDays int) []clickhouse.ThroughputBucket {
	if windowDays > 0 && len(rows) > windowDays {
		return rows[len(rows)-windowDays:]
	}
	return rows
}

// PrewarmNetworkThroughput primes the /v1/network/throughput series so no
// user meets the cold state. Called from the API's 5-minute prewarm loop
// (cmd/stellarindex-api/main.go), which matches networkThroughputTTL, so
// the entry stays permanently fresh. It kicks the SAME detached
// single-flight refresh the request path uses, with the SAME argument
// (networkThroughputMaxWindowDays — there is only one, and only one call
// site builds it), so prewarm and handler can never warm different keys
// (the feedback_prewarm_handler_drift class). Returns immediately when
// already warm; ctx only gates whether the kick happens at shutdown.
func (h *Handler) PrewarmNetworkThroughput(ctx context.Context) {
	if h.Reader == nil || ctx.Err() != nil {
		return
	}
	if _, _, fresh := h.throughput.get(); fresh {
		return
	}
	h.refreshNetworkThroughput() //nolint:contextcheck // intentional detach — prewarm kicks the same background compute
}
