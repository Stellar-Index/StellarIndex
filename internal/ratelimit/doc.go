// Package ratelimit is a Redis-backed fixed-window rate limiter: one atomic Lua INCRBY/EXPIRE per
// request on `rl:<key>:<unix_seconds/60>` (ADR-0007). A client can spend its limit at the end of
// one minute and again at the start of the next, so size a tier to survive 2x its limit.
//
// [Bucket.Take] spends one token; [Bucket.Charge] spends a cost clamped into [1, limit]. A handler
// whose uncached work is selected by a client parameter, or that fans out, must re-price itself
// through middleware.ChargeRateLimit; nothing enforces this across routes.
//
// Fail-open only for [DefaultDwellTime]: inside it Take returns the transport error and the caller
// serves the request; past it Take returns [ErrThrottleUnavailable] and the caller MUST answer 503
// with Retry-After (branch on errors.Is), or a sustained outage becomes an unlimited bypass.
package ratelimit
