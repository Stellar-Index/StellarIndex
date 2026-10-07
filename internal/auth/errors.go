package auth

import "errors"

// Sentinel errors. Middleware translates these to HTTP status codes:
//
//	ErrUnauthorized       → 401 (caller can fix by presenting valid creds)
//	ErrForbidden          → 403 (caller's creds are valid but lack scope)
//	ErrTokenExpired       → 401 with WWW-Authenticate hint
//	ErrTokenMalformed     → 400 (the token isn't even decodable)
//	ErrNotImplemented     → 503 auth-not-configured (auth middleware);
//	                        404 sep10-unavailable (/v1/auth/sep10/* handlers)
//
// Code outside this package should compare via [errors.Is], not
// string match — wrappers add context but preserve sentinels.
var (
	// ErrUnauthorized — credential was missing or didn't validate.
	// 401 Unauthorized; clients can retry with a fresh credential.
	ErrUnauthorized = errors.New("auth: credential missing or invalid")

	// ErrForbidden — credential is valid but the subject lacks the
	// scope/role required for the action. 403 Forbidden; clients
	// shouldn't retry without an admin re-issuing a higher-tier
	// credential.
	ErrForbidden = errors.New("auth: subject not authorised for this action")

	// ErrTokenExpired — JWT exp claim has passed. Distinct from
	// ErrUnauthorized so the middleware can set a more useful
	// WWW-Authenticate header.
	ErrTokenExpired = errors.New("auth: token expired")

	// ErrTokenMalformed — credential bytes don't parse as a token
	// at all (bad base64, missing dots, etc.). 400 Bad Request.
	ErrTokenMalformed = errors.New("auth: token malformed")

	// ErrNotImplemented — no real validator is wired. Returned by
	// [NoopAPIKeyValidator] (an API-key mode whose Redis or Postgres
	// backend is unavailable), by [NoopSEP10Validator] (SEP-10 not
	// configured and auth_mode is not sep10; under sep10 that fails
	// startup instead), and by the auth middleware when the mode's
	// validator is nil. The auth middleware answers 503
	// auth-not-configured; the /v1/auth/sep10/* challenge and token
	// handlers answer 404 sep10-unavailable. Never silently authorise
	// or silently reject. The Noop validators are the deliberate "no
	// validator wired" state, not stubs awaiting replacement.
	ErrNotImplemented = errors.New("auth: validator not implemented in this build")

	// ErrSignupRateLimited — returned by the per-IP signup throttle
	// (`v1.SignupIPThrottle`) when a single IP exhausts its
	// hourly signup budget. Distinct from the global rate-limit
	// 429 so the handler can return a more specific error envelope.
	ErrSignupRateLimited = errors.New("auth: signup rate limited for this IP")

	// ErrThrottleUnavailable — the throttle layer has been failing
	// long enough that fail-open is unsafe; the handler must
	// return 503 + Retry-After. Returned by abuse-prevention seams
	// ([RedisSignupIPThrottle.CheckIP] + [ratelimit.Bucket.Take]) once
	// their dwell-time threshold is crossed on a sustained Redis
	// outage.
	//
	// Dwell-time inversion: transient Redis blips (< dwell-time,
	// default 30s) fall open so a single MISCONF / network hiccup
	// doesn't take signup or the rate limiter offline. Sustained
	// outages — where an attacker holds Redis down to disable abuse
	// prevention — flip to fail-CLOSED once the dwell-time elapses.
	// The 30s window keeps the UX defence (better to accept
	// unthrottled briefly than reject every request during a blip)
	// while closing the indefinitely-disabled-throttle attack surface.
	ErrThrottleUnavailable = errors.New("auth: throttle layer unavailable (sustained backend errors)")

	// ErrAccountStatusUnavailable — the account-level kill-switch read
	// (the Postgres account store the Redis validator consults for
	// suspension status) is degraded and there is no usable last-known
	// status to ride out on. 503 + Retry-After, NOT 401.
	//
	// The Redis validator reads the account on the hot path to honour
	// the account suspension gate. A transient Postgres degradation
	// (failover, restart, pool exhaustion, statement_timeout, vacuum
	// stall) makes GetBySlug return a non-[platform.ErrNotFound] error;
	// mapped to 401, that would reject EVERY active customer — turning a
	// partial-dependency blip into a total API-key auth outage AND
	// mis-signalling "your credential is invalid" so clients rotate keys
	// during a server-side outage. A short-TTL status cache rides a blip
	// out on last-known status; this sentinel is returned only for the
	// truly-unknown case (no cached
	// status within the staleness bound), so the degradation is a
	// retryable "auth layer degraded" rather than a credential rejection.
	// Distinct from [ErrUnauthorized] so [isCredentialRejection] does not
	// count it against the per-IP failed-auth budget.
	ErrAccountStatusUnavailable = errors.New("auth: account status layer unavailable (kill-switch read degraded)")
)
