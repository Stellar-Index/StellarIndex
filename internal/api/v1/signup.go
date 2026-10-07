package v1

import (
	"context"
	"errors"
	"net/http"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// handleSignupRetired answers every method on /v1/signup and
// /v1/signup/verify with the same 410. It deliberately reads neither the
// body nor the email: answering 201 for a new email and 409 for a known
// one is an email-existence oracle, so no unauthenticated response may
// depend on request content.
func (s *Server) handleSignupRetired(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/endpoint-retired",
		"Endpoint retired", http.StatusGone,
		"POST /v1/signup and /v1/signup/verify have been retired; use POST /v1/register to obtain an API key")
}

// SignupIPThrottle is the v1 boundary for the per-IP signup
// rate-limit. Production wires a Redis-backed token bucket with
// a tight cap (default 5/hour); nil disables the check entirely,
// leaving only the global rate-limit middleware.
//
// Designed as a separate seam from the global rate limit so a
// future deployment can swap in a stricter / different policy
// (e.g. CAPTCHA, proof-of-work, federated denylist) without
// touching the global path.
//
// The global anonymous bucket allows 60/min per IP — plenty for
// browsing the public surfaces but
// 60 signups/min/IP is also 3,600 keys/hour per IP, well above
// any legitimate signup rate. Tightening here closes the
// bulk-mint vector without affecting other anonymous traffic.
type SignupIPThrottle interface {
	// CheckIP returns nil when the IP is below its signup quota,
	// or [auth.ErrSignupRateLimited] when the quota is exhausted.
	// Other errors (typically Redis unavailable) propagate so the
	// handler can fall open with a 503 — better than silently
	// disabling the throttle.
	CheckIP(ctx context.Context, ip string) error
}

// signupIPThrottleOK runs the per-IP signup throttle check.
// Returns true when the request should proceed, false when the
// handler has already written the response (429 on quota
// exhaustion). Falls open on Redis errors so a transient backend
// blip doesn't take signup offline; the global rate-limit
// middleware still applies as a safety net.
//
// Extracted from handleSignup to keep that function under the
// gocognit threshold.
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
		// Sustained Redis outage: fail-CLOSED rather than disabling
		// abuse-prevention indefinitely. Retry-After MUST be set
		// BEFORE writeProblem because writeProblem calls
		// w.WriteHeader(status) internally — headers added after
		// that point are silently dropped by net/http. The 30s
		// hint matches [auth.DefaultSignupThrottleDwellTime];
		// clients that obey Retry-After will naturally space
		// retries far enough apart to ride out a typical Redis
		// fail-over.
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
