package v1

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TipStreamDivergenceBudget is the production divergence sub-budget.
const TipStreamDivergenceBudget = tipStreamDivergenceBudget

// LivezLakeTTL is the production /v1/livez/lake cache round.
const LivezLakeTTL = livezLakeTTL

// SetStreamTimingForTest shortens one second of a stream's cadence
// parameter and the tip stream's divergence sub-budget, so tests exercise
// the same edges without waiting whole seconds. Call before serving.
func (s *Server) SetStreamTimingForTest(second, divergenceBudget time.Duration) {
	s.streamSecondFor, s.tipDivergenceBudgetFor = second, divergenceBudget
}

// ValueBespokeKPIsForTest runs the serve-time USD valuation of a bespoke block's token KPIs.
func (s *Server) ValueBespokeKPIsForTest(ctx context.Context, blk *timescale.BespokeBlock, out *ProtocolBespoke) {
	s.valueBespokeKPIs(ctx, blk, out)
}

// BespokeFromStoreForTest is bespokeFromStore.
func BespokeFromStoreForTest(b *timescale.BespokeBlock) *ProtocolBespoke { return bespokeFromStore(b) }

// ClassicSupplyTTL is the production broad classic supply map TTL.
const ClassicSupplyTTL = classicSupplyTTL

// SeedClassicSupplyForTest installs m as the broad classic supply map, filled age ago, with the refresh retry gap just
// started so no refresh can replace it.
func (s *Server) SeedClassicSupplyForTest(m map[string]string, age time.Duration) {
	s.classicSupplyMu.Lock()
	defer s.classicSupplyMu.Unlock()
	s.classicSupplyCache, s.classicSupplyAt, s.classicSupplyAttemptAt = m, time.Now().Add(-age), time.Now()
}

// AgeLivezLakeCacheForTest moves the cached lake-ping round d into the
// past, as if d had elapsed since it ran.
func (s *Server) AgeLivezLakeCacheForTest(d time.Duration) {
	s.livezLakeMu.Lock()
	defer s.livezLakeMu.Unlock()
	s.livezLakeAt = s.livezLakeAt.Add(-d)
}

// AgeRWASeriesCachesForTest moves the cached /v1/rwa/history and
// /v1/rwa/premium assemblies d into the past, as if d had elapsed.
func (s *Server) AgeRWASeriesCachesForTest(d time.Duration) {
	s.rwaHistMu.Lock()
	s.rwaHistAt = s.rwaHistAt.Add(-d)
	s.rwaHistMu.Unlock()
	s.rwaPremMu.Lock()
	s.rwaPremAt = s.rwaPremAt.Add(-d)
	s.rwaPremMu.Unlock()
}

// AgeRWAAssetsViewForTest moves the cached /v1/rwa/assets view and its expiry d into the past.
func (s *Server) AgeRWAAssetsViewForTest(d time.Duration) {
	s.rwaViewMu.Lock()
	defer s.rwaViewMu.Unlock()
	if s.rwaViewCache != nil {
		s.rwaViewCache.builtAt = s.rwaViewCache.builtAt.Add(-d)
	}
	s.rwaViewExpires = s.rwaViewExpires.Add(-d)
}

// RWAAssetsViewLifetimeForTest is how long the cached /v1/rwa/assets view has left before it expires.
func (s *Server) RWAAssetsViewLifetimeForTest() time.Duration {
	s.rwaViewMu.Lock()
	defer s.rwaViewMu.Unlock()
	return time.Until(s.rwaViewExpires)
}

// RWAAssetsDegradedTTL is the production lifetime of a degraded /v1/rwa/assets view.
const RWAAssetsDegradedTTL = rwaAssetsDegradedTTL

// RWAHistoryTTL is the production /v1/rwa/history cache TTL.
const RWAHistoryTTL = rwaHistoryTTL
