package v1

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// This file puts a prewarmed, stale-while-revalidate layer under the contract_count served on
// GET /v1/protocols.
//
// Without it handleProtocolsList would call protocolRoster PER protocol on every origin miss; for
// a registry-empty source that is a `SELECT DISTINCT ... LIMIT 5000` full served-tier scan, on an
// UNAUTHENTICATED route where a cache-busting query string skips the edge cache.
//
// The count changes slowly, so a per-source last-good count is exact enough and collapses the
// request path to a map read. One background refresh runs per source per protocolRosterTTL
// regardless of request rate, and the prewarm sweep keeps entries warm.

// protocolRosterTTL is the freshness horizon of a cached roster count. Past it the entry is NOT
// dropped but served while ONE detached refresh runs. 20 minutes exceeds the protocol prewarm
// sweep interval (~13-16 min), so prewarm refreshes every entry and requests rarely refresh. It
// matches protocolDetailTTL because the roster feeds the directory the detail cache warms.
const protocolRosterTTL = 20 * time.Minute

// protocolRosterRefreshTimeout bounds one detached roster refresh. The
// scan is ~0.5–0.8s measured live; 30s is generous headroom for contention
// while still bounding a wedged query so it cannot pin the single-flight.
const protocolRosterRefreshTimeout = 30 * time.Second

type rosterEntry struct {
	count int
	at    time.Time
}

// rosterFlight is one in-flight detached refresh; done closes when it
// finishes.
type rosterFlight struct {
	done chan struct{}
}

// rosterCache is the per-server last-good cache for protocol roster
// counts, keyed by source name. Zero value ready to use.
type rosterCache struct {
	mu      sync.Mutex
	entries map[string]rosterEntry
	flights map[string]*rosterFlight
}

// initLocked lazy-creates the maps. Caller holds mu.
func (c *rosterCache) initLocked() {
	if c.entries == nil {
		c.entries = map[string]rosterEntry{}
		c.flights = map[string]*rosterFlight{}
	}
}

// cachedRosterCount returns meta's contract_count with stale-while-revalidate semantics:
//
//   - fresh entry: served as-is.
//   - entry past protocolRosterTTL: served immediately while ONE detached refresh runs.
//   - never built: wait for the detached single-flight refresh up to the caller's deadline. The
//     refresh uses a background ctx + protocolRosterRefreshTimeout, so a request that times out
//     still lets the fill complete and the next request lands warm.
//
// ok=false only when the caller's context is cancelled OR the first-ever read for this source
// FAILED. A nil reader is not a failure: it yields a genuine (possibly zero) count with ok=true.
// The caller omits an ok=false source rather than publishing a fabricated contract_count: 0.
func (s *Server) cachedRosterCount(ctx context.Context, meta ProtocolMeta) (count int, ok bool) {
	key := meta.Name
	c := &s.protocolRosterCache
	c.mu.Lock()
	c.initLocked()
	if e, has := c.entries[key]; has {
		if time.Since(e.at) >= protocolRosterTTL {
			s.refreshRosterLocked(key, meta) //nolint:contextcheck // intentional detach — the refresh must outlive this request
		}
		c.mu.Unlock()
		return e.count, true
	}
	fl := s.refreshRosterLocked(key, meta) //nolint:contextcheck // intentional detach — a request that times out must not kill the fill
	c.mu.Unlock()
	select {
	case <-fl.done:
		c.mu.Lock()
		e, has := c.entries[key]
		c.mu.Unlock()
		return e.count, has
	case <-ctx.Done():
		return 0, false
	}
}

// refreshRosterLocked kicks ONE detached roster refresh for key (returning the live flight if
// one is up). Caller holds mu. It runs on a background context, never a request deadline, and
// only DISPLACES the cached entry on success: a failed read keeps the last-good count (old-but-
// real beats a fabricated zero), and a first-ever failure leaves the key absent.
// endRosterFlight deregisters fl and releases its waiters. It is deferred so it runs on EVERY
// exit including a panic: a flight left in the map is never retried and never closes.
func (s *Server) endRosterFlight(key string, fl *rosterFlight) {
	s.protocolRosterCache.mu.Lock()
	delete(s.protocolRosterCache.flights, key)
	s.protocolRosterCache.mu.Unlock()
	close(fl.done)
}

func (s *Server) refreshRosterLocked(key string, meta ProtocolMeta) *rosterFlight {
	if fl, up := s.protocolRosterCache.flights[key]; up {
		return fl
	}
	fl := &rosterFlight{done: make(chan struct{})}
	s.protocolRosterCache.flights[key] = fl
	go func() {
		// Deregistering the flight and closing fl.done run from a DEFER so a panic in rosterErr
		// cannot skip them; otherwise refreshRosterLocked keeps handing the dead flight to every
		// later request, each waiting on a channel that never fires.
		defer func() {
			if rec := recover(); rec != nil {
				worker.Report(s.logger, "api-protocol-roster-refresh", rec)
			}
			s.endRosterFlight(key, fl)
		}()
		rctx, cancel := context.WithTimeout(context.Background(), protocolRosterRefreshTimeout)
		defer cancel()
		// The count, not the roster's length: a source whose contracts are
		// counted rather than enumerated caches its TRUE total instead of a
		// roster capped far below it (protocolContractCounter).
		count, err := s.rosterCountErr(rctx, meta)

		s.protocolRosterCache.mu.Lock()
		if err == nil {
			s.protocolRosterCache.entries[key] = rosterEntry{count: count, at: time.Now()}
		}
		s.protocolRosterCache.mu.Unlock()

		if err != nil && s.logger != nil {
			s.logger.Warn("protocol roster refresh failed", "source", meta.Name, "err", err)
		}
	}()
	return fl
}

// PrewarmProtocolRosters refreshes EVERY source's roster-count entry unconditionally, one read at a time, at exactly the sweep cadence.
func (s *Server) PrewarmProtocolRosters(ctx context.Context) {
	for _, meta := range protocolRegistry {
		if ctx.Err() != nil {
			return
		}
		s.protocolRosterCache.mu.Lock()
		s.protocolRosterCache.initLocked()
		fl := s.refreshRosterLocked(meta.Name, meta) //nolint:contextcheck // intentional detach — prewarm refreshes run on the refresh budget, not the sweep ctx
		s.protocolRosterCache.mu.Unlock()
		select {
		case <-fl.done:
		case <-ctx.Done():
			return
		}
	}
}
