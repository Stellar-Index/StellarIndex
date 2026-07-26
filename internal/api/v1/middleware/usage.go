package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// postResponseWriteTimeout bounds the bookkeeping writes that run AFTER
// the response has been flushed (usage counters, last-used touch). They
// deliberately do not inherit the request's cancellation — see the call
// sites — so they need a bound of their own or a wedged Redis would pin
// the request goroutine forever. Generous relative to go-redis's 3 s
// default so it is a backstop, not the usual limiter.
const postResponseWriteTimeout = 5 * time.Second

// UsageTracker records per-request daily counters keyed on the
// authenticated subject (KeyID for API-key callers; the auth.Subject
// Identifier when KeyID is empty). Anonymous requests are skipped —
// /v1/account/usage is per-account, and there's no account to bill
// for IP-only callers.
//
// Two counter families per request:
//
//   - The LEGACY per-day total (usage:<sub>:<day>) — feeds
//     [MonthlyQuota] and the /v1/account/usage fallback path. Only
//     BILLABLE traffic increments it: 429s are excluded so
//     rate-limit rejections don't eat monthly quota, and 5xx are
//     excluded for the same reason with more force (COR-05) — a
//     platform-caused failure is our fault, not the customer's, so
//     an outage must not burn through the quota they paid for and
//     lock them out of their own plan once we recover. Both classes
//     stay fully visible in the detail family below, so the traffic
//     is still observable; it is only the BILLABLE total that
//     excludes them.
//   - The per-endpoint DETAIL hash (usage:ep:<sub>:<day>) — one
//     field per (route pattern, outcome class): ok / 4xx / 429 /
//     5xx. The rollup worker folds these into the `usage_daily`
//     hypertable that backs the dashboard's per-endpoint analytics.
//
// The endpoint label is the mux route PATTERN (bounded cardinality),
// read via [obs.RouteFromContext] — the innermost obs.CaptureRoute
// middleware writes it after dispatch — with r.Pattern as fallback
// for stacks without the obs pair (tests). Unmatched paths bucket
// under "unmatched".
//
// Failures are logged at debug and dropped — usage tracking must
// never block a request.
//
// Wire this AFTER the auth middleware (so SubjectFrom returns) and
// OUTSIDE rate-limit, so 429 rejections are observed and counted as
// `throttled` in the detail family.
func UsageTracker(counter *usage.Counter, logger *slog.Logger) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if counter == nil {
				next.ServeHTTP(w, r)
				return
			}
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			subject, ok := auth.SubjectFrom(r.Context())
			if !ok {
				return
			}
			id := UsageKeyForSubject(subject)
			if id == "" {
				return
			}
			family := endpointFamily(r)
			class := outcomeClass(rec.status)
			// The response is already written, so these counters MUST NOT
			// inherit the request's cancellation: a client that aborted, or
			// a handler that consumed the whole RequestTimeout budget, would
			// otherwise silently lose its usage row — and the legacy total is
			// the monthly-quota input. Still bounded, so a wedged Redis can't
			// pin the request goroutine (C3-102, audit-2026-07-23).
			//
			// DELIBERATE BEHAVIOUR CHANGE, recorded rather than discovered:
			// an ABORTED request now consumes monthly quota where before it
			// silently did not. statusRecorder defaults to 200, so a served-
			// then-abandoned request classes as billable and the Increment
			// (which used to die on the cancelled context) now lands. That
			// is the intended semantics — the request consumed the read, the
			// pool connection and the CPU, and NOT counting it is a
			// quota-evasion vector: a caller could abort before the body
			// completes and get unmetered traffic indefinitely.
			//
			// The 5 s bound is only as hard as the store's context honouring.
			// go-redis and database/sql both respect ctx cancellation on the
			// wire, so it holds for every store wired here today; a driver
			// that ignored ctx would block the request goroutine for its own
			// timeout instead.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), postResponseWriteTimeout)
			defer cancel()
			if billableClass(class) {
				// Legacy total: billable traffic only (quota input).
				if err := counter.Increment(ctx, id); err != nil {
					logger.Debug("usage: increment failed", "err", err, "subject", id)
				}
			}
			if err := counter.IncrementDetail(ctx, id, family, class); err != nil {
				logger.Debug("usage: detail increment failed",
					"err", err, "subject", id, "endpoint", family, "class", class)
			}
		})
	}
}

// UsageKeyForSubject picks the stable identifier we count under.
// Order of preference:
//  1. KeyID — distinguishes per-key when one account has many.
//  2. Identifier — group-by stable identifier across keys.
//  3. "" — anonymous; skip.
//
// Exported (HLT-01) so every reader of these counters — [MonthlyQuota]
// and /v1/account/usage's handler — calls this single implementation
// instead of reimplementing the derivation. A duplicated copy that
// drifts from this one silently breaks the reader: it would key off
// something the writer never wrote under, and /v1/account/usage would
// return [] despite incoming requests being recorded.
func UsageKeyForSubject(s auth.Subject) string {
	if s.Tier == auth.TierAnonymous || s.Tier == "" {
		return ""
	}
	if s.KeyID != "" {
		return "key:" + s.KeyID
	}
	if s.Identifier != "" {
		return "id:" + s.Identifier
	}
	return ""
}

// endpointFamily resolves the bounded-cardinality endpoint label for
// the request: the mux route pattern path, never the raw URL.
func endpointFamily(r *http.Request) string {
	if route := obs.RouteFromContext(r.Context()); route != "" {
		return route
	}
	// Fallback: the mux mutates r.Pattern in place, so when no
	// inner middleware re-wrapped the request (plain test stacks)
	// the pattern is visible here directly.
	if p := r.Pattern; p != "" {
		if i := strings.IndexByte(p, ' '); i >= 0 {
			return p[i+1:]
		}
		return p
	}
	return "unmatched"
}

// billableClass reports whether an outcome class increments the
// LEGACY per-day total — the counter [MonthlyQuota] enforces against.
//
// Only outcomes the CALLER caused are billable. A 429 is our
// throttle firing and a 5xx is our failure; charging either against
// the customer's monthly quota means a rate-limit storm or an outage
// on our side eats the plan they paid for and locks them out for the
// rest of the month (COR-05). Both remain counted in the per-endpoint
// DETAIL family under their own class, so nothing becomes invisible —
// only unbillable.
func billableClass(class string) bool {
	return class != usage.ClassThrottled && class != usage.ClassServerError
}

// outcomeClass maps a response status onto the four bounded usage
// classes. <400 (incl. 3xx/304) counts as ok; 429 is its own class
// so throttling is visible separately from real client errors.
func outcomeClass(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return usage.ClassThrottled
	case status >= 500:
		return usage.ClassServerError
	case status >= 400:
		return usage.ClassClientError
	default:
		return usage.ClassOK
	}
}
