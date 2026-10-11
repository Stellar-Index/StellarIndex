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
// failure that should surface as HTTP 503 + Retry-After rather than 500.
//
// Covers:
//   - go-redis v9 transport errors (net.OpError, ErrClosed,
//     ErrPoolExhausted, ErrPoolTimeout).
//   - MISCONF replies: with `stop-writes-on-bgsave-error` active and BGSAVE
//     failing, every cache write returns MISCONF and cascade-affected handlers
//     would otherwise 500. 503 + Retry-After lets clients back
//     off while operators unblock writes.
//
// Returns false for:
//   - nil, and ErrPriceNotFound (an application "no data" sentinel).
//   - Plain context.Canceled / DeadlineExceeded: handler deadlines have their
//     own 503s (handlerTimedOut); folding them in would mislabel a
//     server-side timeout as a Redis blip.
//   - Other Redis reply errors (e.g. WRONGTYPE): programming errors that must
//     stay on the 500 path so an alert fires.
//
// Deliberately narrow: add a branch only when an outage shape gets a runbook
// entry.
func IsCacheUnavailable(err error) bool {
	if err == nil {
		return false
	}
	// Application-layer sentinels; not cache failures.
	if errors.Is(err, ErrPriceNotFound) || errors.Is(err, ErrPriceWithheld) {
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
// would otherwise fall through to a generic HTTP 500 on Redis MISCONF.
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
		"https://api.stellarindex.io/errors/cache-unavailable",
		"Cache temporarily unavailable", http.StatusServiceUnavailable,
		"the cache layer reported a write-block (likely Redis bgsave failure); retry shortly. See https://api.stellarindex.io/v1/readyz for live health.")
}
