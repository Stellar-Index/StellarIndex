package middleware

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

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
//     ALLOWED traffic increments it: 429s are excluded so `requests`
//     keeps meaning "requests that ran" (rate-limit rejections must
//     not eat monthly quota).
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
			id := usageKeyForSubject(subject)
			if id == "" {
				return
			}
			if rec.status != http.StatusTooManyRequests {
				// Legacy total: allowed traffic only (quota input).
				if err := counter.Increment(r.Context(), id); err != nil {
					logger.Debug("usage: increment failed", "err", err, "subject", id)
				}
			}
			family := endpointFamily(r)
			class := outcomeClass(rec.status)
			if err := counter.IncrementDetail(r.Context(), id, family, class); err != nil {
				logger.Debug("usage: detail increment failed",
					"err", err, "subject", id, "endpoint", family, "class", class)
			}
		})
	}
}

// usageKeyForSubject picks the stable identifier we count under.
// Order of preference:
//  1. KeyID — distinguishes per-key when one account has many.
//  2. Identifier — group-by stable identifier across keys.
//  3. "" — anonymous; skip.
//
// Shared with [MonthlyQuota] (reads the same counters) and mirrored
// by /v1/account/usage's handler-side derivation.
func usageKeyForSubject(s auth.Subject) string {
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
