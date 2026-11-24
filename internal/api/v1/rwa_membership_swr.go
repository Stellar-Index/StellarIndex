package v1

import "time"

// rwaMembershipServedStaleLocked is the [swrServedStale] verdict for the cached membership set; a lapsed set with no rebuild
// running (held back by the retry gap) is stale too. Caller holds rwaMu.
func (s *Server) rwaMembershipServedStaleLocked() bool {
	if s.rwaAt.IsZero() || time.Since(s.rwaAt) < rwaMembershipTTL {
		return false
	}
	return s.rwaFlight == nil || swrServedStale(s.rwaAt, rwaMembershipTTL, rwaMembershipBudget, !s.rwaFailedAt.IsZero())
}

// failUnfinishedRWARebuild records a rebuild that panicked before its build returned as a failed one; deferred after
// worker.Recover has run.
func (s *Server) failUnfinishedRWARebuild(finished *bool) {
	if *finished {
		return
	}
	s.rwaMu.Lock()
	defer s.rwaMu.Unlock()
	s.rwaFailedAt = time.Now().UTC()
}
