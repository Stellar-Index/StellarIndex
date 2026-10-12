package v1

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// This file puts a LAST-GOOD layer under the per-category bespoke analytics block of
// /v1/protocols/{name}.
//
// The bespoke block is built LAST in buildProtocolDetail, so it inherits whatever remains of
// the rebuild's 90-second budget; under load that reaches zero and enrichBespoke drops the
// block, and on a cold process the bespoke-less view would be cached and stamped fresh,
// rendering the suite as ABSENT.
//
// The block therefore has its own TTL-less cache with a detached, single-flighted, GATED
// refresh. A build serves the previous block instantly; only a TRUE first-ever miss waits,
// bounded by the build's context (the compute survives that deadline, so the next build lands
// warm). A failed or starved refresh keeps the last good block. Every build kicks a refresh
// UNCONDITIONALLY (single flight collapses duplicates); a freshness-gated kick would sawtooth.

// bespokeStaleAfter is the age past which a SERVED bespoke block is reported stale (analytics.status drops ok to stale); not a refresh
// trigger.
// Three sweeps' worth: a healthy block is ~13-16 min old, so crossing it means refreshes are failing or starved.
const bespokeStaleAfter = 45 * time.Minute

// bespokeRefreshTimeout bounds ONE detached bespoke battery (~2s under load) so a wedged query cannot pin the single-flight.
const bespokeRefreshTimeout = time.Minute

// bespokeGateLimit bounds concurrent detached bespoke batteries (a cold-start burst could launch one per key, ~80 keys). 8 (class cap 4)
// is a third of timescale.PoolMaxOpenConns,
// so the detached tier cannot crowd out request-path reads. SEPARATE from the explorer's lake gate: these hit Postgres, and sharing the
// ClickHouse bound would couple unrelated pools.
const bespokeGateLimit = 8

// bespokeGateClass is this cache's refresh class, its own so a burst of bespoke refreshes cannot consume the gate's whole allowance
// (clickhouse.RefreshGate.TryAcquireClass).
const bespokeGateClass = "protocol_bespoke"

// errBespokeSaturated is the flight error for a build SKIPPED (not queued) on a saturated gate. Never reaches the wire: a first-ever miss
// degrades to "no block" and the next build re-kicks.
var errBespokeSaturated = errors.New("v1: protocol bespoke refresh capacity saturated; retry shortly")

// errBespokePanicked is the flight error for a panicked detached build, distinct from a build error so operators can tell "the builder
// died" (also moves stellarindex_worker_panics_total).
var errBespokePanicked = errors.New("v1: protocol bespoke build panicked")

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

// protocolBespokeCacheKey is the single cache-key grammar for the bespoke cache: every dimension BuildProtocolBespoke dispatches on goes
// through here, so no two call sites key a block differently.
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

// refreshBespoke kicks ONE detached build for key (returning the live flight if one is up). Detached on purpose: a build bound to the
// caller's budget dies at the deadline and drops the block.
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
	reader := s.ProtocolBespoke
	go func() {
		defer gate.ReleaseClass(bespokeGateClass)
		// End the flight from a defer so a panic cannot wedge this key:
		// bespokeCache.end both deregisters the flight and closes its
		// done channel, and it cannot be called twice (it closes a
		// channel), so this is the one place it may run.
		var err error
		defer func() {
			if rec := recover(); rec != nil {
				worker.Report(s.logger, "api-protocol-bespoke-build", rec)
				err = errBespokePanicked
			}
			s.protocolBespokeCache.end(key, fl, err)
		}()
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
			return
		}
		s.labelOracleSACs(rctx, blk)
		s.protocolBespokeCache.put(key, blk)
	}()
	return fl
}

// cachedBespoke returns the bespoke block for (source, category, window): a cached block immediately (stale once older than
// bespokeStaleAfter) while one detached refresh runs;
// a first-ever miss waits for that build, bounded by ctx (the build outlives the wait). ok=false means no block could be produced, and
// the caller drops it.
func (s *Server) cachedBespoke(ctx context.Context, source, category string, windowDays int) (blk *timescale.BespokeBlock, stale, ok bool) {
	if s.ProtocolBespoke == nil {
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

// labelOracleSACs swaps each SAC contract id in an oracle block for the classic asset it provably wraps (reflector-dex keys feeds by SAC,
// an opaque C-address on the page).
func (s *Server) labelOracleSACs(ctx context.Context, blk *timescale.BespokeBlock) {
	if blk == nil || blk.Category != "oracle" || s.Explorer == nil {
		return
	}
	memo := map[string]string{}
	for i := range blk.Breakdowns {
		for j := range blk.Breakdowns[i].Rows {
			blk.Breakdowns[i].Rows[j].Label = s.relabelSACTokens(ctx, memo, blk.Breakdowns[i].Rows[j].Label)
		}
	}
	for i := range blk.Tables {
		for _, row := range blk.Tables[i].Rows {
			for k := range row {
				row[k] = s.relabelSACTokens(ctx, memo, row[k])
			}
		}
	}
}

// relabelSACTokens replaces each space-separated SAC id in text, resolving
// each distinct id once through memo.
func (s *Server) relabelSACTokens(ctx context.Context, memo map[string]string, text string) string {
	toks := strings.Split(text, " ")
	for i, tok := range toks {
		if len(tok) != 56 || tok[0] != 'C' {
			continue
		}
		name, seen := memo[tok]
		if !seen {
			name = tok
			if asset, ok := s.resolveSACToClassic(ctx, tok); ok {
				name = asset.String()
			}
			memo[tok] = name
		}
		toks[i] = name
	}
	return strings.Join(toks, " ")
}
