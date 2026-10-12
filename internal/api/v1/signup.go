package v1

import (
	"context"
	"errors"
	"net/http"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// handleSignupRetired answers every method on /v1/signup and /v1/signup/verify with the same
// 410. It reads neither body nor email: 201-vs-409 would be an email-existence oracle, so no
// unauthenticated response may depend on request content.
func (s *Server) handleSignupRetired(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/endpoint-retired",
		"Endpoint retired", http.StatusGone,
		"POST /v1/signup and /v1/signup/verify have been retired; use POST /v1/register to obtain an API key")
}

// SignupIPThrottle is the optional per-IP signup rate limit (Redis token bucket, default 5/hour), separate because the global anonymous bucket would allow 3,600 keys/hour per IP; nil disables it.
type SignupIPThrottle interface {
	// CheckIP returns nil when the IP is below its signup quota, or [auth.ErrSignupRateLimited]
	// when exhausted. Other errors (typically Redis unavailable) propagate to the caller.
	CheckIP(ctx context.Context, ip string) error
}

// signupIPThrottleOK runs the per-IP signup throttle check. It returns true when the request
// should proceed, false when the response is already written (429 on quota exhaustion). It
// falls open on transient Redis errors; the global rate-limit middleware still applies.
func (s *Server) signupIPThrottleOK(w http.ResponseWriter, r *http.Request) bool {
	if s.SignupIPThrottle == nil {
		return true
	}
	ip := middleware.RemoteIP(r)
	if ip == "" {
		return true
	}
	err := s.SignupIPThrottle.CheckIP(r.Context(), ip)
	if err == nil {
		return true
	}
	if errors.Is(err, auth.ErrSignupRateLimited) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/signup-rate-limited",
			"Signup rate limit exceeded", http.StatusTooManyRequests,
			"too many signups from this IP recently; wait an hour and try again, or contact support if you're a legitimate operator bulk-onboarding a team")
		return false
	}
	if errors.Is(err, auth.ErrThrottleUnavailable) {
		// Sustained Redis outage: fail CLOSED rather than disable abuse prevention. Retry-After
		// MUST be set BEFORE writeProblem, which writes the header; later additions are dropped
		// by net/http. The 30s hint matches [auth.DefaultSignupThrottleDwellTime], enough to
		// ride out a typical Redis fail-over.
		w.Header().Set("Retry-After", "30")
		s.logger.Warn("signup IP throttle unavailable; failing closed (sustained Redis errors)",
			"err", err, "ip", ip)
		obs.RateLimitFailClosedTotal.WithLabelValues(obs.RateLimiterSignupIP).Inc()
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/throttle-unavailable",
			"Throttle layer unavailable", http.StatusServiceUnavailable,
			"the abuse-prevention layer has been unreachable for an extended period; retry in a moment")
		return false
	}
	s.logger.Warn("signup IP throttle check failed; falling open",
		"err", err, "ip", ip)
	// Fall open — Redis blip shouldn't take signup offline, and the
	// global rate limit still applies.
	return true
}
