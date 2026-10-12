package v1

import (
	"context"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// LakeWatermarkReader reports the highest captured lake ledger and its close time (ADR-0041); nil omits `as_of_ledger` and never flips `flags.stale`.
type LakeWatermarkReader interface {
	LakeWatermark(ctx context.Context) (ledger uint32, closedAt time.Time, err error)
}

// lakeStaleThreshold: a lake-backed response is flagged `flags.stale` when the watermark's close
// time trails now by more than this. 300s (~60 ledgers) is beyond normal sink lag, so it does not
// flap on an indexer restart, yet catches a wedged sink. It tracks the ops data-freshness
// watchdog, not the tighter served-tier price SLA.
const lakeStaleThreshold = 300 * time.Second

// lakeWatermarkTTL bounds how often the watermark query runs: 15s of drift is invisible next to
// the 300s threshold and caps ClickHouse round-trips at four a minute across ALL lake endpoints.
const lakeWatermarkTTL = 15 * time.Second

const (
	// lakeWatermarkRefreshTimeout bounds ONE detached read. The query is a cheap `max()`, so this
	// only stops a wedged lake from parking a goroutine for the connection's 30s ReadTimeout.
	lakeWatermarkRefreshTimeout = 10 * time.Second
	// lakeWatermarkRetryGap is the negative cache: the earliest a new read may start after a
	// FAILED one, so a wedged lake is not re-read per request nor hammered while it struggles.
	lakeWatermarkRetryGap = 5 * time.Second
	// lakeWatermarkColdWait bounds how long a caller with NO cached watermark waits on the
	// in-flight read before degrading to "no watermark", so a slow lake cannot spend a generous
	// caller's whole budget on a freshness annotation.
	lakeWatermarkColdWait = 2 * time.Second
	// lakeWatermarkFlightKey names the single-slot singleflight group.
	lakeWatermarkFlightKey = "lake-watermark"
)

// lakeWatermark returns the cached lake watermark: the ledger to stamp as `as_of_ledger` and
// whether the response must carry `flags.stale`. ok=false means no ledger to stamp. stale fails
// CLOSED: a wired reader with no measurement yet (or an empty lake) reports stale=true, since
// unknown freshness is not fresh; only an unwired reader reports stale=false.
//
// It is stale-serve: refreshing under lakeWMMu on the caller's context would queue every lake
// route behind one ClickHouse round-trip on a non-context-aware mutex. An existing entry is
// ALWAYS returned immediately and a lapsed one kicks ONE detached read; only a process that
// has never read a watermark waits, for the shorter of its deadline and lakeWatermarkColdWait.
func (s *Server) lakeWatermark(ctx context.Context) (ledger uint32, stale bool, ok bool) {
	if s.LakeWatermark == nil {
		return 0, false, false
	}
	l, closedAt, fresh, mayRead := s.lakeWatermarkCached()
	switch {
	case fresh:
		// Within the TTL — nothing to do.
	case l != 0:
		// Lapsed but serviceable: closedAt only gets older, so `stale` stays correct while the
		// refresh runs off the request path.
		if mayRead {
			s.refreshLakeWatermark() //nolint:contextcheck // intentional detach — the read must outlive the request that noticed the lapse (see refreshLakeWatermark)
		}
	case mayRead:
		l, closedAt = s.waitForLakeWatermark(ctx)
	}
	if l == 0 {
		return 0, true, false
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

// waitForLakeWatermark is the COLD path only: nothing is cached, so the caller waits for the
// shared read, bounded by its own context and lakeWatermarkColdWait, never by the lake. It
// leaves with whatever the cache holds (possibly nothing); the read keeps running detached.
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

// refreshLakeWatermark starts (or joins) the ONE in-flight watermark read and returns the channel
// that fires when it completes. The read uses a detached, bounded context: tied to a request it
// would die with the first caller that gives up.
//
// Nothing can wedge on the channel: singleflight delivers to every waiter, the read is bounded
// by lakeWatermarkRefreshTimeout, and a panic is recovered, leaving the last-good entry.
func (s *Server) refreshLakeWatermark() <-chan singleflight.Result {
	return s.lakeWMFlight.DoChan(lakeWatermarkFlightKey, func() (any, error) {
		defer worker.Recover(s.logger, "api-lake-watermark-refresh")
		ctx, cancel := context.WithTimeout(context.Background(), lakeWatermarkRefreshTimeout)
		defer cancel()
		l, closedAt, err := s.LakeWatermark.LakeWatermark(ctx)
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
