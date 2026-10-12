// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"context"
	"log/slog"
	"net"
	"net/http"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// KeyToucher is the storage-side primitive [TouchUsage] drives; narrow so tests can fake it without Postgres.
type KeyToucher interface {
	TouchUsage(ctx context.Context, id string, ip net.IP, userAgent string) error
}

// TouchDebouncer gates [KeyToucher.TouchUsage] so the api_keys hot row does not take one UPDATE per request.
type TouchDebouncer interface {
	// ShouldTouch returns true exactly once per (keyID, TTL) window. A storage error returns
	// (false, err); callers skip the tick either way because touching is best-effort and must
	// not block customer requests.
	ShouldTouch(ctx context.Context, keyID string) (bool, error)
}

// TouchUsage records last_used_at/ip/user_agent on the api_keys row post-response, debounced and best-effort (errors dropped; nil toucher or debouncer disables it); wire AFTER [Auth].
func TouchUsage(toucher KeyToucher, debouncer TouchDebouncer, logger *slog.Logger) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Deferred so a panicking handler still gets its touch: Recoverer sits outside, so
			// straight-line code after ServeHTTP would never run. Re-panic keeps the outer
			// Recoverer's logging; the response is not complete yet, so it must not be flushed.
			defer func() {
				p := recover()
				touchUsageRecord(w, toucher, debouncer, logger, r, p != nil)
				if p != nil {
					panic(p)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// touchUsageRecord is [TouchUsage]'s post-dispatch bookkeeping, split out so it runs on panic
// too. Normally [AfterResponse] flushes w and runs the touch off-goroutine; on a panic the work
// is still enqueued but w is left unflushed so the outer Recoverer can write its 500.
func touchUsageRecord(w http.ResponseWriter, toucher KeyToucher, debouncer TouchDebouncer, logger *slog.Logger, r *http.Request, panicked bool) {
	if toucher == nil || debouncer == nil {
		return
	}
	subject, ok := auth.SubjectFrom(r.Context())
	if !ok || subject.Tier == auth.TierAnonymous || subject.KeyID == "" {
		return
	}
	keyID := subject.KeyID
	ip := net.ParseIP(RemoteIPFrom(r))
	ua := truncateUserAgentForTouch(r.UserAgent())
	fn := func() { //nolint:contextcheck // intentional detach: post-response work outlives the request ctx
		// Detached from request cancellation so an aborted client or spent RequestTimeout does
		// not drop the touch, but bounded so a wedged Redis/Postgres cannot pin a pool worker.
		ctx, cancel := context.WithTimeout(context.Background(), postResponseWriteTimeout)
		defer cancel()
		shouldTouch, err := debouncer.ShouldTouch(ctx, keyID)
		if err != nil {
			logger.Debug("touch-usage: debounce check failed; skipping",
				"err", err, "key_id", keyID)
			return
		}
		if !shouldTouch {
			return
		}
		if err := toucher.TouchUsage(ctx, keyID, ip, ua); err != nil {
			logger.Debug("touch-usage: TouchUsage failed; bookkeeping lost for this tick",
				"err", err, "key_id", keyID)
		}
	}
	if panicked {
		submitAfterResponseTask(fn)
		return
	}
	AfterResponse(w, fn)
}

// truncateUserAgentForTouch caps the User-Agent at 512 bytes so multi-KB UAs cannot distort the
// dashboard. Blank input returns "" so TouchUsage's NULLIF leaves the column NULL.
func truncateUserAgentForTouch(ua string) string {
	const limit = 512
	if len(ua) > limit {
		return ua[:limit]
	}
	return ua
}
