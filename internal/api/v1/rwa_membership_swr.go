package v1

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/swrverdict"
)

// rwaMembershipServedStaleLocked is the [swrServedStale] verdict for the cached membership set; a lapsed set with no rebuild
// running (held back by the retry gap) is stale too. Caller holds rwaMu.
func (s *Server) rwaMembershipServedStaleLocked() bool {
	if s.rwaAt.IsZero() || time.Since(s.rwaAt) < rwaMembershipTTL {
		return false
	}
	return s.rwaFlight == nil || swrServedStale(s.rwaAt, rwaMembershipTTL, rwaMembershipBudget, !s.rwaFailedAt.IsZero())
}

// rwaMembershipLapsed reports whether a membership set filled at filledAt, read by a cached view or series, fails its own
// verdict now: the live one while it is still the cached set, else by age alone. Zero filledAt is no set. Takes rwaMu.
func (s *Server) rwaMembershipLapsed(filledAt time.Time) bool {
	if filledAt.IsZero() {
		return false
	}
	s.rwaMu.Lock()
	defer s.rwaMu.Unlock()
	if s.rwaAt.Equal(filledAt) {
		return s.rwaMembershipServedStaleLocked()
	}
	return swrverdict.Stale(time.Since(filledAt), rwaMembershipTTL, rwaMembershipBudget, false)
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
