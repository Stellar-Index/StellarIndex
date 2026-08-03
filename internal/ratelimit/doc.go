// Package ratelimit is a Redis-backed fixed-window rate limiter.
//
// # Why fixed-window, not token bucket or sliding window?
//
// Our API SLA is "1000 req/min per client".
// That's a minute-granular ceiling, not a smooth-rate budget. A
// fixed 1-minute window keyed on `rl:<key>:<min>` matches the
// contract exactly + costs one Redis round-trip per request.
//
// Sliding windows need two counters and weighted maths; token
// buckets need INCRBYFLOAT + drift correction + more state per
// key. Neither is worth the complexity here.
//
// # Atomicity
//
// The check is a Lua script (EVAL) that does INCR + EXPIRE-on-first
// + TTL-return atomically. No race between "read counter" and "set
// TTL" — if two requests arrive simultaneously on a cold key, only
// one sets the expiry and both see the correct incremented count.
//
// # Redis key shape
//
// Matches ADR-0007:
//
//	rl:<key>:<minute-epoch>
//
// where `<key>` is an API-key hash or IP address and
// `<minute-epoch>` is `unix_seconds / 60` — deterministic so every
// API pod derives the same key. Keys TTL at 120 s (2× window), so
// they drain out on their own.
//
// # Usage
//
//	b := ratelimit.New(rdb, 1000, time.Minute)
//	res, err := b.Take(ctx, "rek_abc123")
//	if err != nil {
//	    if errors.Is(err, ratelimit.ErrThrottleUnavailable) {
//	        // Redis has been down past the dwell-time — fail CLOSED.
//	        w.Header().Set("Retry-After", "30")
//	        http.Error(w, "throttle unavailable", http.StatusServiceUnavailable)
//	        return
//	    }
//	    // Transient Redis error inside the dwell-time — fail OPEN + log.
//	    log.Debug("ratelimit: redis error, failing open", "err", err)
//	    next(w, r)
//	    return
//	}
//	if !res.Allowed {
//	    w.Header().Set("Retry-After", strconv.Itoa(int(res.RetryAfter.Seconds())))
//	    http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
//	    return
//	}
//	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(b.Max()))
//	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(res.Remaining))
//
// # Failure mode — dwell-time fail-open inversion (F-0050 / F-0150)
//
// The failure policy is NOT unconditional fail-open. Take() returns
// two distinct error kinds so the caller can invert its behaviour as
// an outage lengthens:
//
//   - A transient Redis error (a wrapped transport error) is returned
//     while the outage is still inside the dwell-time window (default
//     [DefaultDwellTime] = 30s). The caller fails OPEN — accept the
//     request + log — because a brief Redis blip must not refuse every
//     request.
//   - [ErrThrottleUnavailable] is returned once Redis has been failing
//     continuously for LONGER than the dwell-time. The caller fails
//     CLOSED (HTTP 503 + Retry-After), because a sustained outage would
//     otherwise let an attacker pivot to unbounded request volume by
//     keeping the limiter offline. A single genuine recovery (a full
//     dwell-time of unbroken successes) resets the clock and fail-open
//     resumes; a stray success under a flapping Redis does NOT reset it
//     (see [Bucket]).
//
// Callers MUST branch on `errors.Is(err, ErrThrottleUnavailable)` — a
// caller that treats every error as fail-open re-introduces the
// sustained-outage bypass this inversion exists to close. The
// production middleware
// (internal/api/v1/middleware.RateLimit / .RateLimitBySubject) is the
// reference implementation of the branch.
//
// The HA plan §9 documents the transient (fail-open) case as a
// stale_flag=true scenario.
package ratelimit
