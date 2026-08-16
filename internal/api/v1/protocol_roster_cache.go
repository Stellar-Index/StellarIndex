package v1

import (
	"context"
	"sync"
	"time"
)

// This file puts a prewarmed, stale-while-revalidate layer under the
// contract_count served on GET /v1/protocols — the SAME shape a8284d64
// established for the bespoke and network-throughput blocks, applied to
// the roster read (W1.3).
//
// Why: handleProtocolsList used to call protocolRoster PER protocol on
// every origin miss, with no server-side cache — only Cache-Control:
// max-age=60. For a registry-empty source that roster is a
// `SELECT DISTINCT … LIMIT 5000` full served-tier scan, so the loop was
// N such scans on an UNAUTHENTICATED, edge-cache-maskable route: a request
// that skips the edge cache (or an attacker cache-busting the query string)
// drives one served-tier scan per protocol. Measured 0.54–0.77s live,
// masked only by the CDN.
//
// The count changes slowly (contracts are registered rarely), so a
// per-source last-good count served stale-while-revalidate is exact enough
// and collapses the request path to a map read. One background refresh runs
// per source per protocolRosterTTL regardless of request rate, and the
// prewarm sweep keeps every entry warm so cold first loads are the
// exception, not the rule.

// protocolRosterTTL is the freshness horizon of a cached roster count.
// Past it the entry is NOT dropped — it is served while ONE detached
// refresh runs (a directory count is immaterially stale for minutes). 20
// minutes pairs with the protocol prewarm sweep (cmd/stellarindex-api
// re-sweeps ~13–16 min apart), so a healthy deployment refreshes every
// entry via prewarm and the request path essentially never triggers a
// refresh of its own. It matches protocolDetailTTL deliberately: the roster
// feeds the same directory the detail cache warms.
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

// cachedRosterCount returns meta's contract_count with stale-while-
// revalidate semantics:
//
//   - fresh entry → served as-is.
//   - entry past protocolRosterTTL → served immediately while ONE detached
//     refresh runs on its own budget.
//   - never built → wait for the detached single-flight refresh up to the
//     caller's deadline. The refresh itself runs detached (background ctx +
//     protocolRosterRefreshTimeout), so a request that times out still lets
//     the fill complete and the next request lands warm.
//
// ok=false only when the caller's context is cancelled OR the first-ever
// roster read for this source FAILED (store error). A nil reader is not a
// failure — it yields a genuine (possibly zero) count with ok=true. The
// caller omits an ok=false source from the directory rather than publishing
// a fabricated contract_count: 0.
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

// refreshRosterLocked kicks ONE detached roster refresh for key (returning
// the live flight when one is already up). Caller holds mu. The refresh runs
// on a background context + protocolRosterRefreshTimeout — never a request
// deadline — and only DISPLACES the cached entry on success: a failed read
// keeps the last-good count (old-but-real beats a fabricated zero), and a
// first-ever failure leaves the key absent so the caller omits the source.
func (s *Server) refreshRosterLocked(key string, meta ProtocolMeta) *rosterFlight {
	if fl, up := s.protocolRosterCache.flights[key]; up {
		return fl
	}
	fl := &rosterFlight{done: make(chan struct{})}
	s.protocolRosterCache.flights[key] = fl
	go func() {
		rctx, cancel := context.WithTimeout(context.Background(), protocolRosterRefreshTimeout)
		defer cancel()
		rows, err := s.rosterErr(rctx, meta)

		s.protocolRosterCache.mu.Lock()
		if err == nil {
			s.protocolRosterCache.entries[key] = rosterEntry{count: len(rows), at: time.Now()}
		}
		delete(s.protocolRosterCache.flights, key)
		s.protocolRosterCache.mu.Unlock()

		if err != nil && s.logger != nil {
			s.logger.Warn("protocol roster refresh failed", "source", meta.Name, "err", err)
		}
		close(fl.done)
	}()
	return fl
}

// PrewarmProtocolRosters refreshes EVERY source's roster-count cache entry
// one read at a time, so /v1/protocols serves warm counts before anyone asks
// and no user request pays for a cold roster scan. Driven by
// cmd/stellarindex-api's protocol prewarm goroutine (alongside
// PrewarmProtocolDetails). Refreshes unconditionally (the single flight
// collapses duplicates) so entries are refreshed at exactly the sweep
// cadence — the same reasoning as PrewarmProtocolDetails.
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
