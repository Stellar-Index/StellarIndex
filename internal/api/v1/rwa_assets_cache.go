package v1

import (
	"context"
	"time"
)

// rwaAssetsViewTTL matches the membership TTL; PrewarmRWA rebuilds on a 5-minute cadence, so a reader never waits on the build.
const rwaAssetsViewTTL = 10 * time.Minute

// rwaAssetsDegradedTTL keeps a cut or partial build for a minute only, so the next reader retries for the full answer.
const rwaAssetsDegradedTTL = time.Minute

// rwaAssetsBuild is one assembled /v1/rwa/assets response.
type rwaAssetsBuild struct {
	view  RWAAssetsView
	flags Flags
}

// cachedRWAAssetsView serves the assembled view, stale-while-revalidate: past expiry it is served under flags.stale while a detached
// rebuild runs. Only a server with no build yet waits, and only up to ctx.
func (s *Server) cachedRWAAssetsView(ctx context.Context) (rwaAssetsBuild, error) {
	s.rwaViewMu.Lock()
	cached, expired := s.rwaViewCache, time.Now().After(s.rwaViewExpires)
	ch := s.rwaViewFlight
	if cached == nil || expired {
		ch = s.startRWAAssetsBuildLocked() //nolint:contextcheck // the build outlives the request that found the view stale.
	}
	s.rwaViewMu.Unlock()
	if cached != nil {
		b := *cached
		if expired {
			b.flags.Stale = true
		}
		return b, nil
	}
	select {
	case <-ch:
	case <-ctx.Done():
		return rwaAssetsBuild{}, ctx.Err()
	}
	s.rwaViewMu.Lock()
	defer s.rwaViewMu.Unlock()
	if s.rwaViewCache == nil {
		return rwaAssetsBuild{}, s.rwaViewErr
	}
	return *s.rwaViewCache, nil
}

// refreshRWAAssetsView rebuilds the view whatever its age and waits for the build, so the prewarm cadence keeps it from expiring.
func (s *Server) refreshRWAAssetsView(ctx context.Context) {
	s.rwaViewMu.Lock()
	ch := s.startRWAAssetsBuildLocked() //nolint:contextcheck // detached so a cancelled prewarm never strands the flight.
	s.rwaViewMu.Unlock()
	select {
	case <-ch:
	case <-ctx.Done():
	}
}

// startRWAAssetsBuildLocked joins the running build or starts one; the caller holds rwaViewMu.
func (s *Server) startRWAAssetsBuildLocked() chan struct{} {
	if s.rwaViewFlight != nil {
		return s.rwaViewFlight
	}
	ch := make(chan struct{})
	s.rwaViewFlight = ch
	//nolint:gosec,contextcheck // G118 / contextcheck: intentional detached build — see runDetachedFill.
	go runDetachedFill(s.logger, "api-rwa-assets-build", maxHandlerBudget, ch, s.buildRWAAssetsView, s.settleRWAAssetsView)
	return ch
}

// settleRWAAssetsView caches a build that answered and releases the flight; a failed build keeps the previous view.
func (s *Server) settleRWAAssetsView(built rwaAssetsBuild, err error) {
	s.rwaViewMu.Lock()
	defer s.rwaViewMu.Unlock()
	s.rwaViewFlight = nil
	s.rwaViewErr = err
	if err != nil {
		return
	}
	ttl := rwaAssetsViewTTL
	if built.flags.Degraded {
		ttl = rwaAssetsDegradedTTL
	}
	s.rwaViewCache = &built
	s.rwaViewExpires = time.Now().Add(ttl)
}
