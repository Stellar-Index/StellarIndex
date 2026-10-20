package auth

import "errors"

// Sentinel errors, mapped by middleware: ErrUnauthorized 401, ErrForbidden 403 (valid creds, lacking scope), ErrTokenExpired 401 + WWW-Authenticate, ErrTokenMalformed 400,
// ErrNotImplemented 503 auth-not-configured (middleware) or 404 sep10-unavailable (/v1/auth/sep10/*). Compare with [errors.Is], never by string: wrappers preserve sentinels.
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

	// ErrNotImplemented means no real validator is wired (the Noop validators, or a nil validator in the middleware): 503 auth-not-configured, or 404 sep10-unavailable on the SEP-10 handlers. Never silently authorise or silently reject.
	ErrNotImplemented = errors.New("auth: validator not implemented in this build")

	// ErrSignupRateLimited: `v1.SignupIPThrottle` fired because one IP exhausted its hourly signup budget. Distinct from the global 429 so the
	// handler can return a more specific envelope.
	ErrSignupRateLimited = errors.New("auth: signup rate limited for this IP")

	// ErrThrottleUnavailable: the throttle layer has failed past its dwell time (default 30s), so fail-open is unsafe and the handler must return 503 + Retry-After.
	// Blips shorter than the dwell time fall open; a sustained outage, which an attacker could hold to disable abuse prevention, flips to fail-CLOSED.
	ErrThrottleUnavailable = errors.New("auth: throttle layer unavailable (sustained backend errors)")

	// ErrAccountStatusUnavailable: the account kill-switch read is degraded with no usable last-known status. 503 + Retry-After, NOT 401: a 401 would reject every active customer and tell clients to rotate keys during a server-side outage.
	// A short-TTL status cache rides blips out; this is returned only when nothing within the staleness bound is cached.
	// Distinct from [ErrUnauthorized] so [isCredentialRejection] does not count it against the per-IP failed-auth budget.
	ErrAccountStatusUnavailable = errors.New("auth: account status layer unavailable (kill-switch read degraded)")
)
