package v1

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/redis/go-redis/v9"
)

// IsCacheUnavailable reports whether err is a Redis transport or MISCONF failure that should surface as 503 + Retry-After; deliberately narrow, application and WRONGTYPE errors stay 500.
func IsCacheUnavailable(err error) bool {
	if err == nil {
		return false
	}
	// Application-layer sentinels; not cache failures.
	if errors.Is(err, ErrPriceNotFound) || errors.Is(err, ErrPriceWithheld) {
		return false
	}

	// MISCONF: the prefix helper matches wrapped errors too.
	if redis.HasErrorPrefix(err, "MISCONF") {
		return true
	}

	// Transport sentinels: the client cannot serve requests.
	if errors.Is(err, redis.ErrClosed) ||
		errors.Is(err, redis.ErrPoolExhausted) ||
		errors.Is(err, redis.ErrPoolTimeout) {
		return true
	}

	// Network errors below the Redis layer (TCP RST, DNS failure); errors.As walks the wrap chain.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	// Substring fallback for an untyped MISCONF: the orchestrator wraps errors with fmt.Errorf before a
	// handler sees them, and HasErrorPrefix fails when the chain holds a plain errors.New. The trailing
	// space in "MISCONF " keeps words like "MISCONFIGURED" from matching.
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		if strings.Contains(err.Error(), "MISCONF ") {
			return true
		}
	}

	return false
}

// writeCacheUnavailableProblem emits the 503 + Retry-After + problem+json shape for handlers that
// would otherwise 500 on Redis MISCONF. Retry-After is 30s, matching writeThrottleUnavailableProblem;
// typical Redis fail-over lands well inside it. Retry-After MUST be set before writeProblem, which
// owns WriteHeader; net/http drops headers added after the status line is committed.
func writeCacheUnavailableProblem(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "30")
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/cache-unavailable",
		"Cache temporarily unavailable", http.StatusServiceUnavailable,
		"the cache layer reported a write-block (likely Redis bgsave failure); retry shortly. See https://api.stellarindex.io/v1/readyz for live health.")
}
