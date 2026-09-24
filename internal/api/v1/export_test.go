package v1

import "time"

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

// AgeLivezLakeCacheForTest moves the cached lake-ping round d into the
// past, as if d had elapsed since it ran.
func (s *Server) AgeLivezLakeCacheForTest(d time.Duration) {
	s.livezLakeMu.Lock()
	defer s.livezLakeMu.Unlock()
	s.livezLakeAt = s.livezLakeAt.Add(-d)
}
