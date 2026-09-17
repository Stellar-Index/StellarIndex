package v1

import (
	"context"
	"sync"
	"time"
)

// contractProtocolIndexTTL bounds how stale the contract → protocol map
// may be. Rosters change when a factory deploys a new instance; ten
// minutes late on a label is fine, a registry read per contract is not.
const contractProtocolIndexTTL = 10 * time.Minute

type contractProtocolIndex struct {
	mu       sync.Mutex
	built    time.Time
	byID     map[string]string
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

func (s *Server) contractProtocolIndexFor(ctx context.Context) map[string]string {
	s.contractIndex.mu.Lock()
	fresh := s.contractIndex.byID != nil && time.Since(s.contractIndex.built) < contractProtocolIndexTTL
	if fresh || s.contractIndex.inFlight {
		m := s.contractIndex.byID
		s.contractIndex.mu.Unlock()
		return m
	}
	s.contractIndex.inFlight = true
	s.contractIndex.mu.Unlock()

	built := s.buildContractProtocolIndex(ctx)

	s.contractIndex.mu.Lock()
	defer s.contractIndex.mu.Unlock()
	s.contractIndex.inFlight = false
	if built != nil {
		s.contractIndex.byID = built
		s.contractIndex.built = time.Now()
	}
	return s.contractIndex.byID
}

// buildContractProtocolIndex walks the registry. Statics first, then the
// roster, so a contract both name is labelled by the roster (the more
// specific source: a factory is "blend", its pool is also "blend").
func (s *Server) buildContractProtocolIndex(ctx context.Context) map[string]string {
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
			continue // logged where it failed; the statics still label
		}
		for _, r := range rows {
			if r.ContractID != "" {
				out[r.ContractID] = meta.Name
			}
		}
	}
	return out
}
