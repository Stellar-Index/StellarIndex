package v1

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// This file puts a LAST-GOOD layer under the per-category bespoke
// analytics block of /v1/protocols/{name} — the visual suite (KPIs,
// series, tables) the CCTP/DEX/lending/yield/oracle pages render.
//
// Why (§2.6b grounding incident): the detail VIEW has been prewarmed +
// stale-served since 2026-07-31, but the bespoke block inside it had no
// cache of its own. The block is built LAST in buildProtocolDetail, so it
// inherits whatever is left of the rebuild's 90-second budget after the
// roster + lake analytics; under load that remainder went to zero and the
// store returned `context deadline exceeded` ("protocol bespoke build
// failed …"). enrichBespoke then honestly dropped the block, and — when
// no HEALTHY entry existed yet (cold process, fresh deploy, a whole
// window that has never built) — the bespoke-less view was cached and
// stamped fresh, so the page rendered its suite as ABSENT.
//
// The fix is the pattern already used for every other expensive read
// here: the block gets its own TTL-less cache with a detached,
// single-flighted, GATED refresh. A build serves the previous block
// instantly and never waits — except on a TRUE first-ever miss, which
// waits for the detached compute bounded by the build's own context (the
// compute survives that deadline, so the next build lands warm). A failed
// or starved refresh keeps the last good block: old-but-real beats blank.
//
// Refresh cadence: every build kicks a refresh UNCONDITIONALLY (the
// single flight collapses duplicates). Builds are already rate-limited by
// the detail cache — the prewarm sweep touches each (protocol, window)
// key about every 13–16 minutes — so an unconditional kick refreshes the
// block at exactly the sweep cadence. A freshness-gated kick would
// instead sawtooth (the sweep would skip the still-fresh block, then find
// it stale one sweep later), which is the same failure PrewarmProtocolDetails
// avoids by refreshing unconditionally.

// bespokeStaleAfter is the age past which a SERVED bespoke block is
// reported stale (the detail view's analytics.status drops ok → stale).
// It is NOT a refresh trigger — refreshes are driven by the build cadence
// above. Three sweeps' worth (~45 min): a healthy system always serves a
// block from the previous sweep (≈13–16 min old), so crossing this line
// means the refresh has been failing or starved for a while, which is
// exactly when a client should be told.
const bespokeStaleAfter = 45 * time.Minute

// bespokeRefreshTimeout bounds ONE detached bespoke battery. Measured on
// r1 UNDER replay load (2026-07-31): soroswap 90d ~1.9s, cctp ~0.4s. A
// minute is two orders of magnitude of headroom for contention while
// still bounding a wedged query so it cannot pin the single-flight.
const bespokeRefreshTimeout = time.Minute

// bespokeGateLimit bounds concurrently-running detached bespoke
// batteries. The key space is closed (registry × the four whitelisted
// windows ≈ 80 keys, none of them caller-chosen), but a cold-start burst
// could still launch one Postgres battery per key at once, so the gate
// caps it. 8 (class cap 4 — TryAcquireClass allows half the global
// limit) is a third of timescale.PoolMaxOpenConns, so the detached tier
// can never crowd out request-path served-tier reads, while leaving
// enough headroom that a legitimate burst of never-built keys is served
// rather than skipped.
//
// Deliberately a SEPARATE gate from the explorer's lake gate: these
// queries run against the served tier (Postgres), and borrowing the
// ClickHouse pool's bound would couple two pools that share no resource
// — a bespoke burst would starve lake refreshes and vice versa.
const bespokeGateLimit = 8

// bespokeGateClass is this cache's refresh class. Its own name so a burst
// of bespoke refreshes can never consume the gate's whole allowance (see
// clickhouse.RefreshGate.TryAcquireClass).
const bespokeGateClass = "protocol_bespoke"

// errBespokeSaturated is the flight error recorded when the refresh gate
// was saturated and the build was SKIPPED rather than queued. It never
// reaches the wire: a first-ever miss degrades to "no block" (exactly as
// a failed build already did) and the next build re-kicks.
var errBespokeSaturated = errors.New("v1: protocol bespoke refresh capacity saturated; retry shortly")

type bespokeEntry struct {
	// blk is the last successfully-built block. A nil blk with ok=true is
	// a legitimate answer — the category has no bespoke suite (see
	// timescale.Store.BuildProtocolBespoke's default arm) — and is cached
	// so those protocols don't re-run the probe on every build.
	blk *timescale.BespokeBlock
	at  time.Time
}

// bespokeFlight is one in-flight detached build. done closes when it
// finishes; err is readable only after done closes.
type bespokeFlight struct {
	done chan struct{}
	err  error
}

// bespokeCache is the per-server last-good cache for the bespoke blocks,
// keyed by protocolBespokeCacheKey. Zero value ready to use.
type bespokeCache struct {
	mu       sync.Mutex
	entries  map[string]bespokeEntry
	flights  map[string]*bespokeFlight
	gateOnce sync.Once
	gate     *clickhouse.RefreshGate
	// now stamps entries and ages them against bespokeStaleAfter. Nil
	// means time.Now; tests inject a fake so the staleness horizon is a
	// deterministic clock step, never a wall-clock race.
	now func() time.Time
}

// clock returns the cache's time source (time.Now unless injected).
func (c *bespokeCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// protocolBespokeCacheKey is the single cache-key grammar for the bespoke
// cache — every dimension BuildProtocolBespoke dispatches on (source,
// category, window) goes through here, so no two call sites can key the
// same block differently (the feedback_prewarm_handler_drift class).
func protocolBespokeCacheKey(source, category string, windowDays int) string {
	return newCacheKey("protocolBespoke").str(source).str(category).int(windowDays).build()
}

func (c *bespokeCache) get(key string) (bespokeEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	return e, ok
}

func (c *bespokeCache) put(key string, blk *timescale.BespokeBlock) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]bespokeEntry)
	}
	c.entries[key] = bespokeEntry{blk: blk, at: c.clock()}
}

// begin returns the flight for key and whether the caller owns it. Owners
// must call end; waiters select on done.
func (c *bespokeCache) begin(key string) (*bespokeFlight, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if fl, up := c.flights[key]; up {
		return fl, false
	}
	if c.flights == nil {
		c.flights = make(map[string]*bespokeFlight)
	}
	fl := &bespokeFlight{done: make(chan struct{})}
	c.flights[key] = fl
	return fl, true
}

func (c *bespokeCache) end(key string, fl *bespokeFlight, err error) {
	c.mu.Lock()
	delete(c.flights, key)
	c.mu.Unlock()
	fl.err = err
	close(fl.done)
}

// refreshGate lazily creates this cache's served-tier refresh gate.
func (c *bespokeCache) refreshGate() *clickhouse.RefreshGate {
	c.gateOnce.Do(func() { c.gate = clickhouse.NewRefreshGate(bespokeGateLimit) })
	return c.gate
}

// refreshBespoke kicks ONE detached bespoke build for key (returning the
// live flight when one is already up) and returns it to optionally wait
// on. Detached on purpose: bound to the rebuild's remaining budget the
// battery died at the deadline and the block was dropped from the page —
// the incident this cache exists for.
func (s *Server) refreshBespoke(key, source, category string, windowDays int) *bespokeFlight {
	fl, owner := s.protocolBespokeCache.begin(key)
	if !owner {
		return fl
	}
	gate := s.protocolBespokeCache.refreshGate()
	if !gate.TryAcquireClass(bespokeGateClass) {
		// Skip, don't queue: the caller serves whatever is cached and the
		// next build re-kicks (same contract as clickhouse.RefreshGate).
		s.protocolBespokeCache.end(key, fl, errBespokeSaturated)
		return fl
	}
	reader := s.protocolBespoke
	go func() {
		defer gate.ReleaseClass(bespokeGateClass)
		start := time.Now()
		rctx, cancel := context.WithTimeout(context.Background(), bespokeRefreshTimeout)
		defer cancel()
		blk, err := reader.BuildProtocolBespoke(rctx, source, category, windowDays)
		obs.ObserveExplorerSWRRefresh("protocol_bespoke", start, err)
		if err != nil {
			// Keep the previous entry (if any) — old-but-real beats blank.
			if s.logger != nil {
				s.logger.Warn("protocol bespoke build failed", "source", source, "category", category, "window_days", windowDays, "err", err)
			}
			s.protocolBespokeCache.end(key, fl, err)
			return
		}
		s.protocolBespokeCache.put(key, blk)
		s.protocolBespokeCache.end(key, fl, nil)
	}()
	return fl
}

// cachedBespoke returns the bespoke block for (source, category, window).
//
//   - A cached block is returned IMMEDIATELY (never blocking a build on
//     the store), flagged stale once it is older than bespokeStaleAfter,
//     while one detached refresh runs.
//   - A TRUE first-ever miss waits for that detached build, bounded by
//     ctx. The build outlives the wait, so the next one lands warm.
//   - ok=false means no block could be produced at all (no reader wired,
//     or the first-ever build failed / was starved / outran ctx) — the
//     caller degrades exactly as it did before this cache existed.
func (s *Server) cachedBespoke(ctx context.Context, source, category string, windowDays int) (blk *timescale.BespokeBlock, stale, ok bool) {
	if s.protocolBespoke == nil {
		return nil, false, false
	}
	key := protocolBespokeCacheKey(source, category, windowDays)
	fl := s.refreshBespoke(key, source, category, windowDays) //nolint:contextcheck // intentional detach — the battery must outlive this build (see refreshBespoke)
	if e, has := s.protocolBespokeCache.get(key); has {
		return e.blk, s.protocolBespokeCache.clock().Sub(e.at) > bespokeStaleAfter, true
	}
	select {
	case <-fl.done:
		if e, has := s.protocolBespokeCache.get(key); has {
			return e.blk, false, true
		}
		return nil, false, false
	case <-ctx.Done():
		return nil, false, false
	}
}
