package v1

import (
	"context"
	"sync"
	"time"
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

// contractProtocolIndexBuildTimeout bounds one build. The build runs on
// its own deadline, detached from the request that triggered it: a
// request whose own budget is already spent must not produce a
// statics-only map that is then served to everyone else.
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
// builds it cold. A no-op while the map is fresh.
func (s *Server) PrewarmContractProtocolIndex(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s.contractProtocolIndexFor(ctx)
}

func (s *Server) contractProtocolIndexFor(ctx context.Context) map[string]string {
	s.contractIndex.mu.Lock()
	ttl := contractProtocolIndexRetryTTL
	if s.contractIndex.complete {
		ttl = contractProtocolIndexTTL
	}
	fresh := s.contractIndex.byID != nil && time.Since(s.contractIndex.built) < ttl
	if fresh || s.contractIndex.inFlight {
		m := s.contractIndex.byID
		s.contractIndex.mu.Unlock()
		return m
	}
	s.contractIndex.inFlight = true
	s.contractIndex.mu.Unlock()

	// Detached from the caller's deadline: the map outlives this request
	// and is served to every other one, so it is built on its own budget,
	// never on whatever the triggering request had left.
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), contractProtocolIndexBuildTimeout)
	defer cancel()
	start := time.Now()
	built, complete, failed := s.buildContractProtocolIndex(bctx)
	s.logger.Info("contract protocol index built",
		"entries", len(built), "complete", complete, "failed_sources", failed,
		"took_ms", time.Since(start).Milliseconds())

	s.contractIndex.mu.Lock()
	defer s.contractIndex.mu.Unlock()
	s.contractIndex.inFlight = false
	s.contractIndex.byID = built
	s.contractIndex.built = time.Now()
	s.contractIndex.complete = complete
	return s.contractIndex.byID
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
