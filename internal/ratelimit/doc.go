// Package ratelimit is a Redis-backed fixed-window rate limiter: one
// atomic Lua INCRBY/EXPIRE per request on `rl:<key>:<unix_seconds/60>`
// (ADR-0007), keys expiring after 120 s. A client can spend its limit at
// the end of one minute and again at the start of the next, so size a
// tier so that 2× its limit for a moment is survivable.
//
// [Bucket.Take] spends one token; [Bucket.Charge] spends a cost clamped
// into [1, limit]. Any handler whose uncached store work a client
// parameter selects, or that fans out per request, must re-price itself
// through middleware.ChargeRateLimit before reading; nothing enforces
// this across routes, so a new route of that shape is enrolled by hand.
//
// Failure is fail-open only for [DefaultDwellTime]: inside it Take
// returns the transport error and the caller serves the request; past
// it Take returns [ErrThrottleUnavailable] and the caller must answer
// 503 with Retry-After, or a sustained outage becomes an unlimited
// bypass. Callers MUST branch on errors.Is(err, ErrThrottleUnavailable);
// middleware.RateLimit is the reference.
package ratelimit
