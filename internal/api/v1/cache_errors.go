package v1

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/redis/go-redis/v9"
)

// IsCacheUnavailable reports whether err is a Redis transport-or-state
// failure that should surface to clients as HTTP 503 + Retry-After
// (vs HTTP 500 for a true internal-error/code-bug).
//
// Covers:
//   - go-redis v9 transport errors (net.OpError + transport-layer
//     sentinels: ErrClosed, ErrPoolExhausted, ErrPoolTimeout).
//   - MISCONF replies — Redis returns "MISCONF Redis is configured to
//     save RDB snapshots, but it's currently unable to persist to
//     disk" once `stop-writes-on-bgsave-error` is active and BGSAVE
//     has failed. This was the May-10 SEV-2 (incidents/data/
//     2026-05-10-redis-writes-blocked-disk-full.md): every cache write
//     returned MISCONF, the orchestrator's per-pair Set bombed, and
//     downstream the cascade-affected handlers — /v1/oracle/*,
//     /v1/lending/pools, /v1/vwap, /v1/observations*, /v1/price/tip* —
//     surfaced HTTP 500 generic-internal-error to clients (F-0086,
//     F-0087, F-0089, F-0090, F-0145, F-0146). 503 + Retry-After lets
//     well-behaved clients back off automatically while operators
//     unblock the writes.
//
// Returns false for:
//   - nil (no error).
//   - ErrPriceNotFound (an application-layer "no data" sentinel — not
//     a cache failure).
//   - Plain context.Canceled / context.DeadlineExceeded (handler-side
//     deadlines are surfaced as their own per-handler 503s via
//     handlerTimedOut + the in-handler timeout problem-type URLs;
//     mixing them into the cache-unavailable branch would mis-label
//     a server-side timeout as a Redis blip).
//   - Any other generic Redis reply error (e.g. WRONGTYPE) — those
//     are programming errors, NOT operational outages, and stay on
//     the 500 path so an alert fires.
//
// The function is deliberately narrow: it covers MISCONF + transport-
// layer failures, nothing else. A new branch is added only when a
// specific outage shape gets a runbook entry.
func IsCacheUnavailable(err error) bool {
	if err == nil {
		return false
	}
	// Application-layer sentinel; not a cache failure.
	if errors.Is(err, ErrPriceNotFound) {
		return false
	}

	// MISCONF — Redis stop-writes-on-bgsave-error active. Use
	// go-redis's prefix helper so wrapped errors still match.
	if redis.HasErrorPrefix(err, "MISCONF") {
		return true
	}

	// Transport-layer sentinels — the Redis client itself is in a
	// state where it cannot serve requests.
	if errors.Is(err, redis.ErrClosed) ||
		errors.Is(err, redis.ErrPoolExhausted) ||
		errors.Is(err, redis.ErrPoolTimeout) {
		return true
	}

	// Network errors below the Redis layer (TCP RST, DNS failure on
	// reconnect, etc.). errors.As walks the wrap chain so the helper
	// matches whether the caller returned the raw net.OpError or a
	// fmt.Errorf("...: %w") wrap.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	// Defensive substring match for plain (non-typed) MISCONF — the
	// orchestrator wraps the underlying error via
	// fmt.Errorf("redis set %s: %w", key, err) before it ever
	// reaches a handler, so the wrap chain may contain a non-
	// redis.Error type by the time the storage seam returns it.
	// HasErrorPrefix above matches via errors.As(err, &redis.Error)
	// — which fails for any wrap layer that's a plain *fmt.wrapError
	// around an errors.New ("MISCONF ..."). strings.Contains catches
	// both the wrapped and unwrapped cases.
	//
	// MISCONF is Redis-specific (RESP server reply tag), so a
	// substring match on "MISCONF " — note the trailing space — has
	// no realistic false-positive surface. The leading-space guard
	// prevents an unrelated word like "PREFIX_MISCONFIGURED" from
	// being mis-classified.
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		if strings.Contains(err.Error(), "MISCONF ") {
			return true
		}
	}

	return false
}

// writeCacheUnavailableProblem emits the canonical 503 + Retry-After +
// RFC-7807 problem+json shape for the cascade-affected handlers that
// previously fell through to a generic HTTP 500 on Redis MISCONF
// (F-0086, F-0087, F-0089, F-0090, F-0145, F-0146; audit-2026-05-27).
//
// Retry-After is 30s, mirroring the rate-limit middleware's
// writeThrottleUnavailableProblem — typical Redis fail-over windows
// land well inside that span, so a single retry usually succeeds.
//
// Header order matters: Retry-After MUST precede writeProblem because
// writeProblem owns the WriteHeader call and net/http silently drops
// headers added after the status line is committed.
func writeCacheUnavailableProblem(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "30")
	writeProblem(w, r,
		"https://api.stellaratlas.xyz/errors/cache-unavailable",
		"Cache temporarily unavailable", http.StatusServiceUnavailable,
		"the cache layer reported a write-block (likely Redis bgsave failure); retry shortly. See https://api.stellaratlas.xyz/v1/readyz for live health.")
}
