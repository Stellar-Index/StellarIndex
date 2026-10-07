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

// KeyToucher is the storage-side primitive the [TouchUsage]
// middleware drives. Production implementation:
// `*postgresstore.APIKeyStore.TouchUsage`. Declared as a narrow
// interface so tests substitute fakes without standing up
// Postgres.
type KeyToucher interface {
	TouchUsage(ctx context.Context, id string, ip net.IP, userAgent string) error
}

// TouchDebouncer gates calls to [KeyToucher.TouchUsage] so we
// don't hammer the api_keys hot row with one UPDATE per
// request. Production implementation: a Redis-SETNX adapter
// keyed on `touch:apikey:<keyID>` with a configurable TTL
// (5 minutes by default — a once-per-minute debounce with
// safety margin).
type TouchDebouncer interface {
	// ShouldTouch returns true exactly once per (keyID, TTL)
	// window. Subsequent calls inside the window return false
	// without contacting the underlying touch store.
	//
	// A storage-side error returns (false, err); the caller
	// treats both branches as "skip this tick" — TouchUsage is
	// best-effort and must not block customer requests.
	ShouldTouch(ctx context.Context, keyID string) (bool, error)
}

// TouchUsage returns a [Middleware] that updates the api_keys
// row's `last_used_at` / `last_used_ip` / `last_used_user_agent`
// columns for authenticated requests.
//
// Behaviour:
//
//   - Wraps `next.ServeHTTP` so the touch fires post-handler
//     (touch is bookkeeping, not load-bearing). [AfterResponse]
//     flushes the response to the client immediately and hands the
//     actual touch to the shared bounded worker pool — not a
//     per-request goroutine, so fan-out stays capped under load.
//   - Skips anonymous Subjects, Subjects without a KeyID, and
//     deployments where toucher OR debouncer is nil — Redis-less
//     deployments fall in here and get no last_used updates.
//   - The SETNX debounce keeps the per-request cost to one
//     Redis round-trip in the common case (debounce window held
//     by a recent request); only the first call per (key, TTL)
//     window pays the additional Postgres UPDATE.
//   - Best-effort: every error path logs at debug and drops.
//     The dashboard's "last seen" column is operator UX, not
//     auth — a debounce-store blip must never surface to the
//     customer.
//
// Wire AFTER [Auth] (so SubjectFrom returns) — placement in the
// chain after that is flexible because the middleware fires
// post-handler.
func TouchUsage(toucher KeyToucher, debouncer TouchDebouncer, logger *slog.Logger) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Deferred so a panicking handler still gets its touch
			// attempted: Recoverer sits OUTSIDE this middleware, so a
			// panic unwinds through here on its way up, and straight-line
			// bookkeeping after next.ServeHTTP would never run.
			// recover()+re-panic so the outer Recoverer still sees and
			// logs it; on a panic the response is NOT known-complete
			// (Recoverer hasn't written its 500 yet), so the recorder
			// must not flush it out from under that write.
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

// touchUsageRecord is [TouchUsage]'s post-dispatch bookkeeping, pulled
// into its own deferred call so it still runs when the handler panics
// On the non-panic path it hands the actual touch to
// [AfterResponse], which flushes w for the client immediately and runs
// the debounce check + touch on the shared post-response pool
// instead of blocking the request goroutine. On a panic, the work is
// still enqueued (the touch is still worth attempting) but w is left
// unflushed so the outer Recoverer can still write its 500.
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
		// Post-response bookkeeping: detached from the request's
		// cancellation (an aborted client or an exhausted RequestTimeout
		// budget must not silently drop the touch) but independently
		// bounded so a wedged Redis/Postgres can't pin the pool worker.
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

// truncateUserAgentForTouch caps the User-Agent at 512 bytes so
// a misbehaving client can't blow up the api_keys.last_used_user_agent
// column (TEXT in Postgres but the column is rendered in the
// dashboard, where multi-KB UAs distort the table layout).
// Returns the empty string for blank input so the postgres NULLIF
// in TouchUsage's UPDATE leaves the column NULL.
func truncateUserAgentForTouch(ua string) string {
	const limit = 512
	if len(ua) > limit {
		return ua[:limit]
	}
	return ua
}
