package v1

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// classicSupplyTTL is how long the broad trustline-derived supply map is reused; the GROUP BY is ~0.5s and totals move slowly.
const classicSupplyTTL = 10 * time.Minute

// classicSupplyMaxAge bounds the broad map's age while refresh keeps failing; it matches classicLakeSupplyTTL because
// higherClassicSupply max()es the two, and a floor only holds against a reading of comparable vintage.
const classicSupplyMaxAge = classicLakeSupplyTTL

// classicFloorMaxLag bounds how long ago the trustline sums may have been taken and still count as a floor against a net read
// now. A healthy sum is at most one 30-min holders-rollup cycle plus classicSupplyTTL (~45 min with run time) old; the
// unhealthy worst case is ~2 h (classicCirculatingRollupMaxAge plus classicSupplyMaxAge).
const classicFloorMaxLag = time.Hour

// classicSupplyRetryGap rate-limits refresh attempts after a failure: the backing query is a
// full-table GROUP BY, and retrying it per request turned one slow query into a sustained outage.
const classicSupplyRetryGap = 60 * time.Second

// classicSupplyRefreshBudget is the detached refresh's own deadline. It blocks no request, so it
// can exceed the API request timeout, which a request-scoped refresh never could.
const classicSupplyRefreshBudget = 3 * time.Minute

// classicSupplyColdWait bounds a request's wait when nothing is cached; short because the backing query outlives the request
// timeout.
const classicSupplyColdWait = 2 * time.Second

// classicSupplyReader is the narrow capability cachedClassicSupply needs from the explorer reader, shared by the request path
// and the detached refresh.
type classicSupplyReader interface {
	ClassicCirculatingSupply(context.Context) (map[string]string, time.Time, error)
}

var _ classicSupplyReader = (*clickhouse.ExplorerReader)(nil)

// cachedClassicSupply returns the classic circulating-supply map (CODE-ISSUER to raw 7dp total), cached with a TTL and
// single-flight since the ClickHouse GROUP BY is too heavy per request. Nil without a capable explorer reader (callers fall
// back to precise supply); serves the last good map on refresh errors until classicSupplyMaxAge.
func (s *Server) cachedClassicSupply(ctx context.Context) map[string]string {
	er, ok := s.Explorer.(classicSupplyReader)
	if !ok {
		return nil
	}
	m, _ := s.readCachedClassicSupply(ctx, er)
	return m
}

// classicSupplyMap is [Server.cachedClassicSupply] also reporting whether a capable reader's read answered, and
// recording a stale read on ctx's report when the map served is past classicSupplyTTL.
func (s *Server) classicSupplyMap(ctx context.Context) (m map[string]string, unread bool) {
	er, ok := s.Explorer.(classicSupplyReader)
	if !ok {
		return nil, false
	}
	m, stale := s.readCachedClassicSupply(ctx, er)
	if stale {
		reportStaleRead(ctx)
	}
	return m, m == nil
}

// readCachedClassicSupply returns the map to serve and whether it is past classicSupplyTTL. Sums taken more than
// classicFloorMaxLag ago are not served, and not waited on: a burn since then can leave a floor above true supply.
func (s *Server) readCachedClassicSupply(ctx context.Context, er classicSupplyReader) (map[string]string, bool) {
	s.classicSupplyMu.Lock()
	if s.classicSupplyCache != nil && time.Since(s.classicSupplyAt) < classicSupplyTTL {
		m := s.classicSupplyCache
		tooOld := s.classicFloorTooOldLocked()
		s.classicSupplyMu.Unlock()
		if tooOld {
			return nil, false
		}
		return m, false
	}
	// Stale or never filled: start a detached refresh (one in flight, one attempt per
	// classicSupplyRetryGap) and serve the last-good map without blocking.
	last := s.servableClassicSupplyLocked()
	lastTooOld := last != nil && s.classicFloorTooOldLocked()
	flight := s.classicSupplyFlight
	if flight == nil && time.Since(s.classicSupplyAttemptAt) >= classicSupplyRetryGap {
		s.classicSupplyAttemptAt = time.Now() // advances on failure too
		flight = make(chan struct{})
		s.classicSupplyFlight = flight
		// Detaching from the request context is intentional (G118): a refresh on the caller's ctx
		// dies at the request timeout without advancing classicSupplyAt, so every later request
		// would retry the doomed query. The goroutine has its own budget in refreshClassicSupply.
		go s.refreshClassicSupply(er, flight) //nolint:gosec,contextcheck // G118 + contextcheck: the detachment is intentional; see refreshClassicSupply's doc comment.
	}
	// Past its TTL the map is stale only as a last-good copy: no refresh is running (the last attempt failed and the retry gap
	// is open) or the one running has outrun its budget.
	lastGood := flight == nil || time.Since(s.classicSupplyAttemptAt) >= classicSupplyRefreshBudget
	s.classicSupplyMu.Unlock()

	// Serving stale beats blocking: this map only feeds market-cap enrichment,
	// and a 10-minute-old supply figure is far better than a 15s stall.
	if lastTooOld {
		return nil, false
	}
	if last != nil {
		return last, lastGood
	}
	// Nothing servable: wait briefly so a fast query still fills this response, never for the whole
	// request budget, since the query can outrun the 15s timeout. The detached refresh keeps
	// running so the next request hits cache; PrewarmClassicSupply normally fills it first.
	if flight == nil {
		return nil, false // retry gap still open and nothing cached
	}
	timer := time.NewTimer(classicSupplyColdWait)
	defer timer.Stop()
	select {
	case <-flight:
		s.classicSupplyMu.Lock()
		m := s.servableClassicSupplyLocked()
		if m != nil && s.classicFloorTooOldLocked() {
			m = nil
		}
		stale := m != nil && time.Since(s.classicSupplyAt) >= classicSupplyTTL
		s.classicSupplyMu.Unlock()
		return m, stale
	case <-timer.C:
		return nil, false // degrade now; the detached refresh fills the cache for the next request
	case <-ctx.Done():
		return nil, false
	}
}

// servableClassicSupplyLocked returns the cached broad map, or nil once it is
// classicSupplyMaxAge old. Caller holds classicSupplyMu.
func (s *Server) servableClassicSupplyLocked() map[string]string {
	if s.classicSupplyCache == nil || time.Since(s.classicSupplyAt) >= classicSupplyMaxAge {
		return nil
	}
	return s.classicSupplyCache
}

// classicFloorTooOldLocked reports whether the cached sums were taken more than classicFloorMaxLag ago: classicSupplyAt is
// when the cache was filled, classicSupplyLag how far the sums already trailed it. Caller holds classicSupplyMu.
func (s *Server) classicFloorTooOldLocked() bool {
	return time.Since(s.classicSupplyAt)+s.classicSupplyLag > classicFloorMaxLag
}

// PrewarmClassicSupply fills the classic supply cache out of band; concurrency-safe via the request path's single-flight, and a warm cache is a no-op.
func (s *Server) PrewarmClassicSupply(ctx context.Context) {
	s.cachedClassicSupply(ctx)
}

// refreshClassicSupply recomputes the map on a detached context. Load-bearing: a GROUP BY outgrowing the 15s request timeout
// never completes on the request context, and classicSupplyAt advances only on success, so every request would re-run the
// doomed query. classicSupplyAttemptAt rate-limits retries.
func (s *Server) refreshClassicSupply(er classicSupplyReader, done chan struct{}) {
	// Deferred so a panic in the GROUP BY cannot skip clearing the flight: a flight left set is
	// never replaced (refreshes start only when it is nil), which would freeze the map for the
	// life of the API and silently drop market_cap from every asset.
	defer s.endClassicSupplyFlight(done)
	defer worker.Recover(s.logger, "api-classic-supply-refresh")
	ctx, cancel := context.WithTimeout(context.Background(), classicSupplyRefreshBudget)
	defer cancel()

	m, computedAt, err := er.ClassicCirculatingSupply(ctx)

	s.classicSupplyMu.Lock()
	if err == nil && len(m) > 0 {
		s.classicSupplyCache = m
		s.classicSupplyAt = time.Now()
		s.classicSupplyLag = max(s.classicSupplyAt.Sub(computedAt), 0)
	}
	s.classicSupplyMu.Unlock()
	if err != nil {
		s.logger.Warn("classic supply refresh failed (serving last good)", "err", err)
	}
}

// endClassicSupplyFlight releases the single-flight marker and wakes cold-start waiters; deferred so it runs on EVERY exit,
// panics included.
func (s *Server) endClassicSupplyFlight(done chan struct{}) {
	s.classicSupplyMu.Lock()
	s.classicSupplyFlight = nil
	s.classicSupplyMu.Unlock()
	close(done)
}
