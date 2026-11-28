package v1

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// LendingHealthExcludedSourceProof is /health's source-exclusion proof, for the census in package v1_test.
var LendingHealthExcludedSourceProof = TestLendingHealth_ExcludedSourceWithholdsOraclePrice

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

// EmissionTokenPriceForTest is emissionTokenPrice's price and reason.
func (s *Server) EmissionTokenPriceForTest(ctx context.Context, protocol, token string) (string, string) {
	p := s.emissionTokenPrice(ctx, protocol, token, nil)
	return p.price, p.reason
}

// EnrichBespokeForTest runs enrichBespoke for one protocol and returns the served bespoke block.
func (s *Server) EnrichBespokeForTest(ctx context.Context, name, category string, windowDays int) *ProtocolBespoke {
	var view ProtocolDetailView
	s.enrichBespoke(ctx, ProtocolMeta{Name: name, Category: category}, &view, windowDays)
	return view.Bespoke
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

// TopAccountsTTLs are the production lifetimes of a reconciled and an unreconciled top-accounts fill.
const TopAccountsTTL, TopAccountsUnreconciledTTL = lendingReservesTTL, topAccountsUnreconciledTTL

// TopAccountsTTLForTest is the lifetime the top-accounts cache gives pool's current fill; 0 when none is cached.
func (s *Server) TopAccountsTTLForTest(pool string) time.Duration {
	c := s.lendingTopAccounts
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[pool]; ok && e.flight == nil {
		return c.entryTTL(e.val)
	}
	return 0
}

// AgeTopAccountsForTest moves pool's cached top-accounts fill d into the past.
func (s *Server) AgeTopAccountsForTest(pool string, d time.Duration) {
	c := s.lendingTopAccounts
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[pool]; ok {
		e.at = e.at.Add(-d)
	}
}

// RWAAssetsDegradedTTL is the production lifetime of a degraded /v1/rwa/assets view.
const RWAAssetsDegradedTTL = rwaAssetsDegradedTTL

// RWAHistoryTTL is the production /v1/rwa/history cache TTL.
const RWAHistoryTTL = rwaHistoryTTL

// RWAHistoryMaxStale is how long a /v1/rwa/history assembly is carried past failed rebuilds.
const RWAHistoryMaxStale = rwaHistoryMaxStale

// StorageSupplyIdleAge is an age past the storage cache's TTL and one refresh budget: a key nobody has asked for in that long.
const StorageSupplyIdleAge = tokenSupplyTTL + cacheFillBudget + time.Minute

// StorageSupplyTTL is the storage supply cache's TTL.
const StorageSupplyTTL = tokenSupplyTTL

// ExpireForTest backdates key's fill time by age, as a key nobody has read for that long leaves it.
func (c *CachedContractStorageSupplyReader) ExpireForTest(key string, age time.Duration) {
	c.cache.mu.Lock()
	defer c.cache.mu.Unlock()
	if e, ok := c.cache.entries[key]; ok {
		e.at = time.Now().Add(-age)
	}
}

// AwaitRefreshForTest waits for key's refresh in flight, if any, to settle.
func (c *CachedContractStorageSupplyReader) AwaitRefreshForTest(key string) {
	c.cache.mu.Lock()
	var ch chan struct{}
	if e, ok := c.cache.entries[key]; ok {
		ch = e.flight
	}
	c.cache.mu.Unlock()
	if ch != nil {
		<-ch
	}
}

// BackdateRefreshForTest moves the start of key's refresh in flight d into the past.
func (c *CachedContractStorageSupplyReader) BackdateRefreshForTest(key string, d time.Duration) {
	c.cache.mu.Lock()
	defer c.cache.mu.Unlock()
	if e, ok := c.cache.entries[key]; ok && e.flight != nil {
		e.flightAt = e.flightAt.Add(-d)
	}
}
