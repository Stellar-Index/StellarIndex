package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
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
// authenticated subject's OWNER ACCOUNT (auth.Subject Identifier, with
// KeyID as the fallback for credentials carrying no owner reference —
// see [UsageKeyForSubject]). Anonymous requests are skipped —
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
			deadlineFired := new(atomic.Bool)
			reqCtx := r.Context()
			r = r.WithContext(context.WithValue(reqCtx, readDeadlineKey{}, deadlineFired))
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			subject, ok := auth.SubjectFrom(reqCtx)
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
			ctx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), postResponseWriteTimeout)
			defer cancel()
			if billableClass(class, deadlineFired.Load()) {
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
//  1. Identifier — the OWNER-ACCOUNT reference ([auth.AccountIdentifier],
//     `acct:<slug>`, for API keys; the client account id for SEP-10).
//     Every credential the same account holds shares ONE counter.
//  2. KeyID — per-credential fallback, reached only by a credential
//     whose store stamped no owner reference (legacy hand-seeded Redis
//     records). Metered narrowly beats not metered at all.
//  3. "" — anonymous; skip.
//
// # Why the ACCOUNT, not the credential (RLT-404)
//
// The counter this derives feeds [MonthlyQuota], and the ceiling it is
// compared against is a PLAN budget, not a per-credential allowance:
// platform.Tier.MaxMonthlyQuota is the ladder a dashboard-minted
// key's quota is clamped to, and the account-level
// `MonthlyRequestQuotaOverride` is the hard ceiling above it — the
// documented contract is that a customer may only ever LOWER their cap,
// never raise it. Keying on KeyID handed them two ways to raise it
// anyway, because the counter's identity was the credential's:
//
//   - MINT: N live keys under one account meant N independent
//     month-to-date counters, so the plan's allowance multiplied by the
//     number of keys held (bounded by account.go's
//     defaultAccountKeyQuota, but bounded is not capped).
//   - ROTATE: revoke-and-mint produced a fresh KeyID, hence a
//     month-to-date of zero, so the cap reset on demand mid-month —
//     and `accountKeyQuotaOK` counts only un-revoked keys, so the
//     rotation was free.
//
// Counting per account closes both: the allowance is conserved across
// every credential the account holds and survives rotation, which is
// what a plan budget means.
//
// The per-credential `Subject.MonthlyQuota` still decides the ceiling
// for the request in front of us, so a customer who deliberately lowers
// ONE key's quota now gets that key cut off at that many ACCOUNT-wide
// requests. That is stricter than the pre-fix reading and deliberately
// so: it fails closed, and in the ordinary case every key on an account
// carries the same clamped plan value, where the two readings coincide.
//
// Exported (HLT-01) so every reader of these counters — [MonthlyQuota]
// and /v1/account/usage's handler — calls this single implementation
// instead of reimplementing the derivation. A duplicated copy that
// drifts from this one silently breaks the reader: it would key off
// something the writer never wrote under, and /v1/account/usage would
// return [] despite incoming requests being recorded. That coupling is
// why the derivation can only move with its writer: a reader pointed at
// an account key the writer never writes meters NOTHING.
func UsageKeyForSubject(s auth.Subject) string {
	if s.Tier == auth.TierAnonymous || s.Tier == "" {
		return ""
	}
	if s.Identifier != "" {
		return "id:" + s.Identifier
	}
	if s.KeyID != "" {
		return "key:" + s.KeyID
	}
	return ""
}

// resolvedRouteKey is the context key [ResolveRoute] stashes its
// pre-dispatch pattern match under.
type resolvedRouteKey struct{}

// ResolveRoute pre-computes the mux-matched route pattern via a
// read-only mux.Handler lookup — no dispatch, so it never runs a
// handler — and stashes it in the request context. Wire this OUTSIDE
// every pre-dispatch gate (auth, key policy, monthly quota, rate
// limit, usage tracker): a request one of those gates rejects never
// reaches the mux, so [obs.CaptureRoute] (wired innermost, directly
// above the mux) never runs and obs.RouteFromContext/r.Pattern stay
// empty for the rest of the stack, including [endpointFamily]'s
// post-response read. Without this, every gate-rejected request
// bucketed under the bounded-cardinality "unmatched" family instead
// of its real route (Q177), hiding exactly the per-endpoint throttle
// pattern the DETAIL family exists to surface.
//
// [obs.CaptureRoute] remains the authoritative source once the mux
// actually dispatches — [endpointFamily] still prefers it — so this
// only fills the gap for requests a gate stops short of the mux.
func ResolveRoute(mux muxMatcher) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, pattern := mux.Handler(r); pattern != "" {
				r = r.WithContext(context.WithValue(r.Context(), resolvedRouteKey{}, pattern))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// stripMethodPrefix strips the leading "METHOD " a Go 1.22+ mux
// pattern carries (e.g. "GET /v1/assets/{id}") down to the path.
func stripMethodPrefix(pattern string) string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		return pattern[i+1:]
	}
	return pattern
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
		return stripMethodPrefix(p)
	}
	// A pre-dispatch gate (RateLimit, MonthlyQuota, ...) rejected the
	// request before the mux ever ran — [ResolveRoute]'s early match
	// is the only source left. See that doc comment for why this
	// exists at all.
	if p, ok := r.Context().Value(resolvedRouteKey{}).(string); ok && p != "" {
		return stripMethodPrefix(p)
	}
	return "unmatched"
}

// billableClass reports whether an outcome class increments the
// LEGACY per-day total — the counter [MonthlyQuota] enforces against.
//
// Only outcomes the CALLER caused are billable. A 429 is our
// throttle firing and a fast-failing 5xx is our failure; charging
// either against the customer's monthly quota means a rate-limit storm
// or an outage on our side eats the plan they paid for (COR-05).
//
// The exception is a 5xx on which a server-side read deadline fired
// ([MarkReadDeadline]): that read held a pool connection for its whole
// budget, usually because of the request's own size, and leaving it
// unbilled made the most expensive request shape free. Every class
// stays counted in the per-endpoint DETAIL family regardless.
func billableClass(class string, readDeadlineFired bool) bool {
	switch class {
	case usage.ClassThrottled:
		return false
	case usage.ClassServerError:
		return readDeadlineFired
	default:
		return true
	}
}

type readDeadlineKey struct{}

// MarkReadDeadline records that a server-side read deadline fired while
// serving the request ctx belongs to, making a 5xx answer billable (see
// [billableClass]). A no-op outside [UsageTracker]; safe for concurrent use.
func MarkReadDeadline(ctx context.Context) {
	if fired, ok := ctx.Value(readDeadlineKey{}).(*atomic.Bool); ok {
		fired.Store(true)
	}
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
