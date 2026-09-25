package v1

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// contractProtocolIndexTTL bounds how stale a COMPLETE contract → protocol
// map may be. Rosters change when a factory deploys a new instance; ten
// minutes late on a label is fine, a registry read per contract is not.
const contractProtocolIndexTTL = 10 * time.Minute

// contractProtocolIndexRetryTTL is how long an INCOMPLETE map (a build in
// which at least one roster read failed) is served before the next call
// rebuilds it. Short enough that a registry blip does not leave every
// cohort unlabelled for ten minutes; long enough that a registry outage
// does not turn every request into seventeen failing reads.
const contractProtocolIndexRetryTTL = 30 * time.Second

// contractProtocolIndexBuildTimeout bounds one build. A request never
// waits on it: a request-triggered build runs on its own goroutine,
// detached from the request that noticed the map was due, so a request
// whose budget is spent can neither stall on the build nor cut it short
// into a statics-only map served to everyone else.
const contractProtocolIndexBuildTimeout = 5 * time.Second

type contractProtocolIndex struct {
	mu    sync.Mutex
	built time.Time
	byID  map[string]string
	// complete is false when the last build lost at least one roster
	// source; such a map is served but retried after
	// contractProtocolIndexRetryTTL rather than contractProtocolIndexTTL.
	complete bool
	inFlight bool
}

// contractProtocol names the protocol a C… contract belongs to: the
// registry's own statics (factories, extra contracts), every instance the
// roster knows for each registered protocol (the same read
// /v1/protocols/{name}/contracts serves — PG registry, projection
// fallback, soroswap's pair registry), cached for
// contractProtocolIndexTTL. A miss is a contract no protocol claims.
//
// Serves the cohort view's contract labels; a nil-safe seam so a
// deployment with no protocol readers simply labels nothing.
func (s *Server) contractProtocol(ctx context.Context, contractID string) (string, bool) {
	idx := s.contractProtocolIndexFor(ctx)
	name, ok := idx[contractID]
	return name, ok
}

// PrewarmContractProtocolIndex builds the contract → protocol map before
// any request needs it. Called from the API's 5-minute prewarm loop
// (cmd/stellarindex-api/main.go) — under contractProtocolIndexTTL, so a
// complete map is always refreshed before it expires and no request
// triggers it. Builds inline on ctx, so shutdown cancels it. A no-op
// while the map is fresh or another build is running.
func (s *Server) PrewarmContractProtocolIndex(ctx context.Context) {
	if ctx.Err() != nil || !s.claimContractProtocolIndexBuild() {
		return
	}
	s.rebuildContractProtocolIndex(ctx)
}

// contractProtocolIndexFor serves the current map and, when it is due,
// starts ONE rebuild in the background; it never builds on the caller's
// goroutine. Until the first build lands the map is nil, which labels
// nothing — the answer a contract no protocol claims gets.
func (s *Server) contractProtocolIndexFor(ctx context.Context) map[string]string {
	s.contractIndex.mu.Lock()
	m := s.contractIndex.byID
	s.contractIndex.mu.Unlock()
	if s.claimContractProtocolIndexBuild() {
		go func(bctx context.Context) {
			defer worker.Recover(s.logger, "api-contract-protocol-index")
			s.rebuildContractProtocolIndex(bctx)
		}(context.WithoutCancel(ctx))
	}
	return m
}

// claimContractProtocolIndexBuild reports whether the map is due for a
// build and, if so, marks one in flight so no other caller starts a
// second.
func (s *Server) claimContractProtocolIndexBuild() bool {
	s.contractIndex.mu.Lock()
	defer s.contractIndex.mu.Unlock()
	ttl := contractProtocolIndexRetryTTL
	if s.contractIndex.complete {
		ttl = contractProtocolIndexTTL
	}
	fresh := s.contractIndex.byID != nil && time.Since(s.contractIndex.built) < ttl
	if fresh || s.contractIndex.inFlight {
		return false
	}
	s.contractIndex.inFlight = true
	return true
}

// rebuildContractProtocolIndex runs one claimed build and publishes it.
// inFlight is cleared from a defer so a panic in the build cannot leave
// it stuck true, with every later call serving the old map forever.
func (s *Server) rebuildContractProtocolIndex(ctx context.Context) {
	defer func() {
		s.contractIndex.mu.Lock()
		s.contractIndex.inFlight = false
		s.contractIndex.mu.Unlock()
	}()
	bctx, cancel := context.WithTimeout(ctx, contractProtocolIndexBuildTimeout)
	defer cancel()
	start := time.Now()
	built, complete, failed := s.buildContractProtocolIndex(bctx)
	s.logger.Info("contract protocol index built",
		"entries", len(built), "complete", complete, "failed_sources", failed,
		"took_ms", time.Since(start).Milliseconds())

	s.contractIndex.mu.Lock()
	defer s.contractIndex.mu.Unlock()
	s.contractIndex.byID = carryForwardFailedProtocols(s.contractIndex.byID, built, failed)
	s.contractIndex.built = time.Now()
	s.contractIndex.complete = complete
}

// carryForwardFailedProtocols keeps the previous map's labels for every
// protocol whose roster read failed in this build, so a transient
// registry error on a rebuild cannot unlabel contracts a complete map
// already knew. A protocol that answered is taken as it answered (a
// contract it no longer lists is dropped), and on a conflict the fresh
// build wins. built is still private to the caller and is extended in
// place; prev is published and is never written.
func carryForwardFailedProtocols(prev, built map[string]string, failed []string) map[string]string {
	if len(failed) == 0 || len(prev) == 0 {
		return built
	}
	lost := make(map[string]struct{}, len(failed))
	for _, name := range failed {
		lost[name] = struct{}{}
	}
	for id, name := range prev {
		if _, ok := lost[name]; !ok {
			continue
		}
		if _, has := built[id]; !has {
			built[id] = name
		}
	}
	return built
}

// buildContractProtocolIndex walks the registry. Statics first, then the
// roster, so a contract both name is labelled by the roster (the more
// specific source: a factory is "blend", its pool is also "blend").
// complete is false, and failed names the sources, when any roster read
// errored: the map is still worth serving (the statics and every source
// that did answer label), but it is not worth caching for long.
func (s *Server) buildContractProtocolIndex(ctx context.Context) (byID map[string]string, complete bool, failed []string) {
	out := make(map[string]string, 1024)
	for _, meta := range protocolRegistry {
		for _, id := range meta.Factories {
			out[id] = meta.Name
		}
		for _, id := range meta.ExtraContracts {
			out[id] = meta.Name
		}
	}
	for _, meta := range protocolRegistry {
		rows, err := s.protocolContractsErr(ctx, meta.Name)
		if err != nil {
			failed = append(failed, meta.Name) // logged where it failed; the statics still label
			continue
		}
		for _, r := range rows {
			if r.ContractID != "" {
				out[r.ContractID] = meta.Name
			}
		}
	}
	return out, len(failed) == 0, failed
}
