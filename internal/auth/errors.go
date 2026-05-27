package auth

import "errors"

// Sentinel errors. Middleware translates these to HTTP status codes:
//
//	ErrUnauthorized       → 401 (caller can fix by presenting valid creds)
//	ErrForbidden          → 403 (caller's creds are valid but lack scope)
//	ErrTokenExpired       → 401 with WWW-Authenticate hint
//	ErrTokenMalformed     → 400 (the token isn't even decodable)
//	ErrNotImplemented     → 503 (validator stub; not configured yet)
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

	// ErrNotImplemented — returned by the Noop validator fallbacks
	// ([NoopAPIKeyValidator], [NoopSEP10Validator]) when an
	// auth-mode is configured but no real validator is wired (e.g.
	// auth_mode=apikey selected but Redis unavailable, or
	// auth_mode=sep10 selected without the SEP-10 signing seed).
	// The middleware translates it to 503 Service Unavailable —
	// fail-loud, never silently authorise or silently reject. Stays
	// in this package as long as the Noop fallbacks do (i.e.
	// indefinitely; they are the deliberate "no validator wired"
	// disabled state, not a stub awaiting replacement).
	ErrNotImplemented = errors.New("auth: validator not implemented in this build")

	// ErrSignupRateLimited — returned by the per-IP signup throttle
	// (`v1.SignupIPThrottle`) when a single IP exhausts its
	// hourly signup budget. Distinct from the global rate-limit
	// 429 so the handler can ship a more specific error envelope.
	// F-1232 (audit-2026-05-12).
	ErrSignupRateLimited = errors.New("auth: signup rate limited for this IP")

	// ErrThrottleUnavailable — the throttle layer has been failing
	// long enough that fail-open is no longer safe; the handler must
	// return 503 + Retry-After. Returned by abuse-prevention seams
	// ([RedisSignupIPThrottle.CheckIP] + [ratelimit.Bucket.Take]) once
	// their dwell-time threshold is crossed on a sustained Redis
	// outage.
	//
	// Dwell-time inversion (F-0049 / F-0050 / F-0149 / F-0150,
	// audit-2026-05-27): transient Redis blips (< dwell-time, default
	// 30s) still fall open so a single MISCONF / network hiccup
	// doesn't take signup or the rate limiter offline. Sustained
	// outages — the J40 adversarial vector where an attacker holds
	// Redis down to disable abuse prevention — flip to fail-CLOSED
	// once the dwell-time elapses. The 30s window preserves the
	// existing UX defence (better to accept unthrottled briefly than
	// reject every request during a blip) while closing the
	// indefinitely-disabled-throttle attack surface.
	ErrThrottleUnavailable = errors.New("auth: throttle layer unavailable (sustained backend errors)")
)
