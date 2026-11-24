package v1

import (
	"context"
	"errors"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/swrverdict"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// rwaAssetsViewTTL matches the membership TTL; PrewarmRWA rebuilds on a 5-minute cadence, so a reader never waits on the build.
const rwaAssetsViewTTL = 10 * time.Minute

// rwaAssetsDegradedTTL keeps a cut or partial build for a minute only, so the next reader retries for the full answer.
const rwaAssetsDegradedTTL = time.Minute

// rwaAssetsViewMaxStale bounds how long a view whose rebuilds keep failing is served: its reference expiry, staleness and
// review-due verdicts were judged at build time, so past this they are no longer true.
const rwaAssetsViewMaxStale = time.Hour

// errRWAViewTooOld is returned when the only view is past rwaAssetsViewMaxStale and its rebuild did not answer.
var errRWAViewTooOld = errors.New("rwa assets view is past its maximum age and the rebuild did not replace it")

// rwaAssetsBuild is one assembled /v1/rwa/assets response.
type rwaAssetsBuild struct {
	view  RWAAssetsView
	flags Flags
	// builtAt is when the build ran; observedAt, the envelope as_of, is the oldest store read behind it.
	builtAt, observedAt time.Time
	// catalogueAt is the oldest catalogue page's observation time, zero when no page came from the catalogue cache;
	// membershipAt is when the membership set the build read was filled, zero when there was none.
	catalogueAt, membershipAt time.Time
	// refVerdicts are the exclusion verdicts the build valued its references under.
	refVerdicts []rwaRefVerdict
}

// errRWAViewVerdictsChanged: the exclusion set no longer judges the view's references as its build did, and a rebuild did not
// catch up, so the view's figures are no longer true.
var errRWAViewVerdictsChanged = errors.New("rwa assets view was rebuilt but the source exclusions still judge its references otherwise")

// rwaAssetsUnavailableDetail is the 503 detail for a view the cache refused to serve, or false for any other error.
func rwaAssetsUnavailableDetail(err error) (string, bool) {
	switch {
	case errors.Is(err, errRWAViewVerdictsChanged):
		return "the source exclusions changed under the assets view and its rebuild does not yet agree with them; retry shortly", true
	case errors.Is(err, errRWAViewTooOld):
		return "the last assets view is past its maximum age and its rebuild has not answered; retry shortly", true
	}
	return "", false
}

// cachedRWAAssetsView serves the assembled view, never one whose reference verdicts the current exclusion set contradicts:
// that view is rebuilt, waiting up to ctx, and refused if the rebuild still disagrees or does not answer in time.
func (s *Server) cachedRWAAssetsView(ctx context.Context) (rwaAssetsBuild, error) {
	refused, err := s.cachedRWAAssetsViewOnce(ctx, false)
	if err != nil || s.rwaRefVerdictsHold(ctx, refused.refVerdicts) {
		return refused, err
	}
	// A build that landed since the read may already agree, so only the refused view itself is rebuilt.
	if cur, err := s.cachedRWAAssetsViewOnce(ctx, false); err == nil && !cur.builtAt.Equal(refused.builtAt) &&
		s.rwaRefVerdictsHold(ctx, cur.refVerdicts) {
		return cur, nil
	}
	b, err := s.cachedRWAAssetsViewOnce(ctx, true)
	switch {
	case err == nil && s.rwaRefVerdictsHold(ctx, b.refVerdicts):
		return b, nil
	case err == nil, errors.Is(err, context.DeadlineExceeded):
		return rwaAssetsBuild{}, errors.Join(errRWAViewVerdictsChanged, err)
	}
	return rwaAssetsBuild{}, err
}

// cachedRWAAssetsViewOnce serves the assembled view, stale-while-revalidate: past expiry it is served while a detached rebuild
// runs, under flags.stale and flags.degraded once it is a last-good copy (rwaAssetsViewLastGoodLocked). A server with no build,
// one past rwaAssetsViewMaxStale, or rebuild=true waits up to ctx.
func (s *Server) cachedRWAAssetsViewOnce(ctx context.Context, rebuild bool) (rwaAssetsBuild, error) {
	s.rwaViewMu.Lock()
	cached, expired := s.rwaViewCache, time.Now().After(s.rwaViewExpires)
	lastGood := expired && cached != nil && s.rwaAssetsViewLastGoodLocked()
	ch := s.rwaViewFlight
	if cached == nil || expired || rebuild {
		ch = s.startRWAAssetsBuildLocked() //nolint:contextcheck // the build outlives the request that found the view stale.
	}
	s.rwaViewMu.Unlock()
	if !rebuild && cached != nil && time.Since(cached.builtAt) <= rwaAssetsViewMaxStale {
		return s.judgeRWAAssetsView(*cached, lastGood), nil
	}
	select {
	case <-ch:
	case <-ctx.Done():
		return rwaAssetsBuild{}, ctx.Err()
	}
	s.rwaViewMu.Lock()
	if s.rwaViewCache == nil {
		defer s.rwaViewMu.Unlock()
		return rwaAssetsBuild{}, s.rwaViewErr
	}
	if time.Since(s.rwaViewCache.builtAt) > rwaAssetsViewMaxStale {
		defer s.rwaViewMu.Unlock()
		return rwaAssetsBuild{}, errors.Join(errRWAViewTooOld, s.rwaViewErr)
	}
	b, lastGood := *s.rwaViewCache, time.Now().After(s.rwaViewExpires) && s.rwaAssetsViewLastGoodLocked()
	s.rwaViewMu.Unlock()
	return s.judgeRWAAssetsView(b, lastGood), nil
}

// rwaCatalogueRowsStale judges catalogue rows by the view's TTL, not the catalogue's 2 minutes that every 5-minute prewarm
// outlives.
func rwaCatalogueRowsStale(observedAt, now time.Time) bool {
	return !observedAt.IsZero() && now.Sub(observedAt) >= rwaAssetsViewTTL
}

// judgeRWAAssetsView flags a view that is a last-good copy or holds an input past the bound it is judged by at build time:
// catalogue rows by [rwaCatalogueRowsStale], a membership set by its own verdict. Takes rwaMu; the caller holds no lock.
func (s *Server) judgeRWAAssetsView(b rwaAssetsBuild, lastGood bool) rwaAssetsBuild {
	now := time.Now()
	stale := lastGood || rwaCatalogueRowsStale(b.catalogueAt, now)
	if !stale && !b.membershipAt.IsZero() {
		s.rwaMu.Lock()
		if s.rwaAt.Equal(b.membershipAt) {
			stale = s.rwaMembershipServedStaleLocked()
		} else {
			stale = swrverdict.Stale(now.Sub(b.membershipAt), rwaMembershipTTL, rwaMembershipBudget, false)
		}
		s.rwaMu.Unlock()
	}
	if stale {
		b.flags.Stale, b.flags.Degraded = true, true
	}
	return b
}

// rwaAssetsViewLastGoodLocked is the [swrServedStale] verdict for the cached view, filled one TTL before it expires; the
// caller holds rwaViewMu and has a cached view.
func (s *Server) rwaAssetsViewLastGoodLocked() bool {
	ttl := rwaAssetsViewTTL
	if s.rwaViewCache.flags.Degraded {
		ttl = rwaAssetsDegradedTTL
	}
	return swrServedStale(s.rwaViewExpires.Add(-ttl), ttl, maxHandlerBudget, s.rwaViewErr != nil)
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
		// The last good build keeps serving under flags.stale, so this counter is the only signal stellarindex_api_cache_refresh_failing sees.
		obs.APICacheOpsTotal.WithLabelValues("rwa_assets", "build", "refresh_error").Inc()
		s.logger.Warn("rwa assets view build failed; serving the last good build", "err", err)
		return
	}
	ttl := rwaAssetsViewTTL
	if built.flags.Degraded {
		ttl = rwaAssetsDegradedTTL
	}
	s.rwaViewCache = &built
	s.rwaViewExpires = time.Now().Add(ttl)
}
