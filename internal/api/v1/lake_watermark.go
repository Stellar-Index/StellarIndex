package v1

import (
	"context"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// LakeWatermarkReader reports how far the ClickHouse lake's certified capture
// has advanced: the highest captured ledger and that ledger's close time
// (ADR-0041 Decision 4 — every API response derived from the lake's
// current-state / supply projections carries its watermark so the client
// knows HOW FRESH the lake is). Production wiring is
// *clickhouse.ExplorerReader (same lake reader as Explorer /
// ProtocolActivity). Nil → responses omit `as_of_ledger` and never flip
// `flags.stale` from the watermark.
type LakeWatermarkReader interface {
	LakeWatermark(ctx context.Context) (ledger uint32, closedAt time.Time, err error)
}

// lakeStaleThreshold (ADR-0041 Decision 4): a lake-backed response is flagged
// `flags.stale` when the lake watermark's close time trails now by more than
// this. 300s ≈ 60 ledgers at the ~5s close cadence — two orders of magnitude
// beyond the normal galexie→ClickHouse sink lag (single-digit seconds), and
// wide enough not to flap across a routine indexer restart or a slow archive
// partition flush, while still catching a wedged sink long before the
// current-state / supply projections are meaningfully wrong. Deliberately
// aligned with the ops-side data-freshness watchdog's order of magnitude
// rather than the per-price freshness SLA (which is tighter and served-tier).
const lakeStaleThreshold = 300 * time.Second

// lakeWatermarkTTL bounds how often the watermark query actually runs.
// 15s ≈ 3 ledgers of drift at worst — invisible next to the 300s staleness
// threshold — and turns per-request ClickHouse round-trips into at most four
// a minute across ALL lake-backed endpoints (the protocols pillar uses the
// same "cache the tip, don't re-read per request" posture).
const lakeWatermarkTTL = 15 * time.Second

const (
	// lakeWatermarkRefreshTimeout bounds ONE detached watermark read. The
	// query is a `max()` over the small ledgers table (~ms), so 10s is pure
	// headroom — its job is to stop a wedged lake from parking a goroutine
	// for the connection's own 30s ReadTimeout.
	lakeWatermarkRefreshTimeout = 10 * time.Second
	// lakeWatermarkRetryGap is the negative cache: after a FAILED read, the
	// earliest a new one may start. Without it a wedged lake is re-read once
	// per request forever; with it, at most one attempt per gap. 5s is short
	// enough that recovery costs at most one extra gap of watermark age
	// (invisible against the 300s lakeStaleThreshold) and long enough that a
	// struggling lake is not hammered.
	lakeWatermarkRetryGap = 5 * time.Second
	// lakeWatermarkColdWait bounds how long a caller with NO cached
	// watermark at all waits on the in-flight read before degrading to "no
	// watermark". The caller's own context still wins if it is shorter; this
	// cap covers callers that have a generous deadline (or none) so a slow
	// lake can never spend their whole budget on a freshness annotation.
	lakeWatermarkColdWait = 2 * time.Second
	// lakeWatermarkFlightKey names the single-slot singleflight group.
	lakeWatermarkFlightKey = "lake-watermark"
)

// lakeWatermark returns the cached lake watermark: the ledger to stamp as
// `as_of_ledger` and whether the response must carry `flags.stale` per
// lakeStaleThreshold. ok=false when no watermark reader is wired, no read has
// succeeded yet, or the lake is empty — callers then omit `as_of_ledger` and
// leave flags untouched (graceful, matching every other nil-reader degrade in
// this package).
//
// STALE-SERVE (RLT-095, 2026-09-19), the same posture as nativeLPListing
// (#332 F4). The refresh used to run under lakeWMMu on the caller's own
// context, so a slow lake made every concurrent request on every lake-backed
// route — pools_reserves, asset_supply ×3, liquidity_pools ×2, lending, plus
// the three explorer account-state sites — queue on a non-context-aware mutex
// behind one ClickHouse round-trip and burn its own deadline for a freshness
// annotation. Now an existing entry is ALWAYS returned immediately and a
// lapsed one merely kicks ONE detached read; only a process that has never
// read a watermark waits, and then only for the shorter of its own deadline
// and lakeWatermarkColdWait.
func (s *Server) lakeWatermark(ctx context.Context) (ledger uint32, stale bool, ok bool) {
	if s.lakeWatermarkReader == nil {
		return 0, false, false
	}
	l, closedAt, fresh, mayRead := s.lakeWatermarkCached()
	switch {
	case fresh:
		// Within the TTL — nothing to do.
	case l != 0:
		// Lapsed but serviceable: closedAt only gets older, so the cached
		// value still yields correct `stale` semantics while the refresh
		// runs off the request path.
		if mayRead {
			s.refreshLakeWatermark() //nolint:contextcheck // intentional detach — the read must outlive the request that noticed the lapse (see refreshLakeWatermark)
		}
	case mayRead:
		l, closedAt = s.waitForLakeWatermark(ctx)
	}
	if l == 0 {
		return 0, false, false
	}
	return l, time.Since(closedAt) > lakeStaleThreshold, true
}

// lakeWatermarkCached snapshots the cached entry. fresh reports that it is
// inside the TTL; mayRead reports that the negative cache (see
// lakeWatermarkRetryGap) does not currently forbid a new read.
func (s *Server) lakeWatermarkCached() (ledger uint32, closedAt time.Time, fresh, mayRead bool) {
	s.lakeWMMu.Lock()
	defer s.lakeWMMu.Unlock()
	now := time.Now()
	fresh = !s.lakeWMFetched.IsZero() && now.Sub(s.lakeWMFetched) <= lakeWatermarkTTL
	return s.lakeWMLedger, s.lakeWMClosedAt, fresh, now.After(s.lakeWMNextTry)
}

// waitForLakeWatermark is the COLD path only: no watermark has ever been
// read, so there is nothing to serve and the caller waits for the shared
// read — bounded by its own context and lakeWatermarkColdWait, never by the
// lake. Whichever of the three fires first, the caller leaves with whatever
// the cache holds (possibly nothing); the read itself keeps running detached
// for the next caller.
func (s *Server) waitForLakeWatermark(ctx context.Context) (ledger uint32, closedAt time.Time) {
	done := s.refreshLakeWatermark() //nolint:contextcheck // intentional detach — the read is deliberately NOT bound to this caller's context so it survives for the next one (see refreshLakeWatermark); the caller's ctx bounds the WAIT below instead
	timer := time.NewTimer(lakeWatermarkColdWait)
	defer timer.Stop()
	select {
	case <-done:
	case <-ctx.Done():
	case <-timer.C:
	}
	s.lakeWMMu.Lock()
	defer s.lakeWMMu.Unlock()
	return s.lakeWMLedger, s.lakeWMClosedAt
}

// refreshLakeWatermark starts (or joins) the ONE in-flight watermark read and
// returns the channel that fires when it completes. The read runs on a
// detached, bounded context: tied to a request it would die with the first
// caller that gives up, and every later caller would pay the same wait again.
//
// Nothing can wedge on the returned channel — singleflight delivers to every
// waiter when the function returns, the function is bounded by
// lakeWatermarkRefreshTimeout, and a panic inside it is recovered (leaving
// the cache on its last-good entry, whose next lapse simply kicks a new
// read). Callers on the warm path ignore the channel entirely.
func (s *Server) refreshLakeWatermark() <-chan singleflight.Result {
	return s.lakeWMFlight.DoChan(lakeWatermarkFlightKey, func() (any, error) {
		defer worker.Recover(s.logger, "api-lake-watermark-refresh")
		ctx, cancel := context.WithTimeout(context.Background(), lakeWatermarkRefreshTimeout)
		defer cancel()
		l, closedAt, err := s.lakeWatermarkReader.LakeWatermark(ctx)
		s.lakeWMMu.Lock()
		defer s.lakeWMMu.Unlock()
		if err != nil {
			// Back off before the next attempt; the previous entry (if any)
			// stays in place and keeps being served.
			s.lakeWMNextTry = time.Now().Add(lakeWatermarkRetryGap)
			if s.logger != nil {
				s.logger.Warn("lake watermark read failed", "err", err,
					"serving_previous", s.lakeWMLedger != 0)
			}
			return nil, err
		}
		s.lakeWMLedger, s.lakeWMClosedAt = l, closedAt
		s.lakeWMFetched, s.lakeWMNextTry = time.Now(), time.Time{}
		return nil, nil
	})
}
