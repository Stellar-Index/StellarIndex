// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// MonthToDateReader is the storage-side primitive the
// [MonthlyQuota] middleware uses. Production implementation:
// `*usage.Counter.MonthToDate`. Declared as a narrow interface
// so tests can substitute fakes without standing up Redis.
type MonthToDateReader interface {
	MonthToDate(ctx context.Context, subject string) (int64, error)
}

// DefaultMonthlyQuotaDwellTime is the W1-flow-register-4 dwell-time
// window: how long the [MonthlyQuota] middleware may keep failing OPEN
// on month-to-date read errors before it flips to fail-CLOSED (429 +
// Retry-After).
//
// 30s deliberately matches [ratelimit.DefaultDwellTime] and
// [auth.DefaultSignupThrottleDwellTime] — the three fairness/abuse
// seams share one operator-facing threshold. Long enough that a single
// Redis blip (MISCONF, brief partition, fail-over) does not 429 paying
// customers; short enough that a sustained counter outage cannot become
// an indefinite unmetered billing window for a key already at its cap.
// Tune via [WithMonthlyQuotaDwellTime].
const DefaultMonthlyQuotaDwellTime = 30 * time.Second

// MonthlyQuotaOption tunes the [MonthlyQuota] middleware. Variadic so
// the two-argument production call site stays source-compatible.
type MonthlyQuotaOption func(*monthlyQuotaGate)

// WithMonthlyQuotaClock overrides the dwell-time clock source. Tests
// inject a synthetic clock to drive dwell transitions deterministically
// without time.Sleep; nil is ignored (keeps [time.Now]). Mirrors
// [ratelimit.WithClock].
func WithMonthlyQuotaClock(now func() time.Time) MonthlyQuotaOption {
	return func(g *monthlyQuotaGate) {
		if now != nil {
			g.nowFn = now
		}
	}
}

// WithMonthlyQuotaDwellTime overrides the fail-open dwell window
// (default [DefaultMonthlyQuotaDwellTime], 30s). A negative value
// disables the inversion (legacy fail-open-always) — operators should
// NOT reach for this without understanding the billing-overage vector
// W1-flow-register-4. Mirrors [ratelimit.WithDwellTime].
func WithMonthlyQuotaDwellTime(d time.Duration) MonthlyQuotaOption {
	return func(g *monthlyQuotaGate) { g.dwellTime = d }
}

// monthlyQuotaGate holds the dwell-time fail-open→fail-closed state
// shared across every request through one [MonthlyQuota] instance
// (constructed once at wire-up; safe for concurrent use). The state
// machine is the exact mirror of [auth.RedisSignupIPThrottle] and
// [ratelimit.Bucket] (REL-06) — the read errors it observes are Redis
// transport failures, not per-key, so a single process-wide clock is
// the right granularity.
type monthlyQuotaGate struct {
	dwellTime time.Duration
	nowFn     func() time.Time

	mu              sync.Mutex
	redisErrorSince time.Time
	// healthySince marks the start of the CURRENT unbroken run of counter
	// successes; any failure resets it. redisErrorSince (the fail-closed
	// clock) is cleared only once this streak has lasted dwellTime — a
	// single stray success under a flapping counter must NOT reopen the
	// unmetered window (REL-06).
	healthySince time.Time
}

// observeReadFailure stamps the dwell-time clock and reports whether
// the window has elapsed. True → caller fails CLOSED (429); false →
// caller fails OPEN (pass through). Any failure breaks the recovery
// streak. Disabled (negative dwellTime) always returns false. Mirror
// of [ratelimit.Bucket.observeRedisFailure].
func (g *monthlyQuotaGate) observeReadFailure() bool {
	if g.dwellTime < 0 {
		return false
	}
	now := g.nowFn()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.healthySince = time.Time{} // any failure breaks the recovery streak
	if g.redisErrorSince.IsZero() {
		g.redisErrorSince = now
		return false
	}
	return now.Sub(g.redisErrorSince) > g.dwellTime
}

// observeReadSuccess advances the recovery streak. The fail-closed
// clock is cleared only after dwellTime of UNBROKEN successes — never
// on a single success, which under a flapping counter would keep the
// ceiling fail-open indefinitely (REL-06). Mirror of
// [ratelimit.Bucket.observeRedisSuccess].
func (g *monthlyQuotaGate) observeReadSuccess() {
	now := g.nowFn()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.redisErrorSince.IsZero() {
		return // not in the error state; nothing to recover
	}
	if g.healthySince.IsZero() {
		g.healthySince = now
	}
	if now.Sub(g.healthySince) >= g.dwellTime {
		g.redisErrorSince = time.Time{}
		g.healthySince = time.Time{}
	}
}

// MonthlyQuota returns a [Middleware] that enforces the per-key
// `Subject.MonthlyQuota` ceiling. F-1226 (codex audit-2026-05-12):
// the dashboard accepted a per-key `monthly_quota` value and the
// Postgres store persisted it, but no runtime middleware
// enforced it — paid customers on metered plans could spend
// indefinitely past the cap.
//
// Behaviour:
//
//   - Subject with `MonthlyQuota <= 0` (zero / negative): no
//     check; the middleware is a pass-through. Anonymous /
//     un-keyed callers fall in here.
//   - Subject with `MonthlyQuota > 0` and a non-nil reader: read
//     the month-to-date counter for the subject key (via
//     [UsageKeyForSubject], the same derivation `UsageTracker` writes
//     under, so the writer and reader stay in lock-step). When the
//     count >= quota, reject
//     with `429 Too Many Requests` + Problem-JSON body listing
//     the cap. The counter this reads is BILLABLE traffic only —
//     `UsageTracker` keeps 429s and 5xx out of it (see
//     [billableClass]), so neither our throttle nor our outage can
//     consume the customer's cap.
//   - Reader nil: no backing store, pass through (Redis-less
//     deployment).
//   - Read error, dwell-time NOT yet exceeded: count + log + pass
//     through (fail OPEN). The cap is not a security boundary — it's a
//     billing-fairness mechanism, so a transient Redis blip must not
//     429 paying customers. The bypass is counted on
//     [obs.MonthlyQuotaFailOpenTotal] (C3-082) because while it is
//     happening a metered key bills past its agreed cap and the
//     overage cannot be reclaimed after the response is served.
//   - Read error, dwell-time exceeded: reject with `429 Too Many
//     Requests` + Retry-After (fail CLOSED). Once the month-to-date
//     counter has been unreadable continuously for longer than the
//     dwell window a key at/past its cap would otherwise bill unmetered
//     for the entire outage, so the middleware stops serving on trust.
//     Counted on [obs.MonthlyQuotaFailClosedTotal] (W1-flow-register-4).
//
// # Dwell-time fail-open inversion (W1-flow-register-4)
//
// The fail-open→fail-closed state machine is the exact mirror of
// [auth.RedisSignupIPThrottle] and [ratelimit.Bucket] (REL-06): a
// read error inside the dwell window (default 30s, see
// [DefaultMonthlyQuotaDwellTime]) falls open; once errors have
// persisted past the window the middleware fails closed; and the
// clock is cleared only after dwellTime of UNBROKEN success, so a
// single stray success under a flapping counter cannot reopen an
// indefinite unmetered window.
//
// Wire AFTER [Auth] (so SubjectFrom returns) and BEFORE
// [RateLimit] so a request rejected by the monthly cap doesn't
// also spend a rate-limit token.
func MonthlyQuota(reader MonthToDateReader, logger *slog.Logger, opts ...MonthlyQuotaOption) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	gate := &monthlyQuotaGate{
		dwellTime: DefaultMonthlyQuotaDwellTime,
		nowFn:     time.Now,
	}
	for _, opt := range opts {
		opt(gate)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			subject, ok := auth.SubjectFrom(r.Context())
			if !ok || subject.MonthlyQuota <= 0 || reader == nil {
				next.ServeHTTP(w, r)
				return
			}
			id := UsageKeyForSubject(subject)
			if id == "" {
				next.ServeHTTP(w, r)
				return
			}
			used, err := reader.MonthToDate(r.Context(), id)
			if err != nil {
				if gate.observeReadFailure() {
					// W1-flow-register-4: the counter has been unreadable
					// continuously for longer than the dwell window. Continuing
					// to fail open would let a key already at/past its cap bill
					// unmetered for the ENTIRE outage — unrecoverable once the
					// responses are served. Fail CLOSED with 429 + Retry-After.
					obs.MonthlyQuotaFailClosedTotal.Inc()
					logger.Warn("monthly-quota: read failing past dwell window; failing closed",
						"err", err, "subject", id)
					writeMonthlyQuotaUnavailable(w, r, subject.MonthlyQuota, retryAfterForDwell(gate.dwellTime))
					return
				}
				// C3-082: transient blip inside the dwell window — the ceiling
				// is OFF for this request and the customer can bill past their
				// agreed cap, an outcome unrecoverable once the response is
				// served. Count it before logging so the signal exists even at
				// the API's production log level, where Warn is the floor and
				// the old Debug line was invisible.
				obs.MonthlyQuotaFailOpenTotal.Inc()
				logger.Warn("monthly-quota: read failed; failing open",
					"err", err, "subject", id)
				next.ServeHTTP(w, r)
				return
			}
			gate.observeReadSuccess()
			if used >= subject.MonthlyQuota {
				writeMonthlyQuotaDenied(w, r, subject.MonthlyQuota, used)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// retryAfterForDwell derives the fail-closed Retry-After (seconds) from
// the dwell window, floored at 1s. A client obeying it spaces retries
// far enough to ride out a typical counter fail-over. The call site
// only reaches here with a positive dwellTime (a negative one disables
// the inversion, so observeReadFailure never returns true).
func retryAfterForDwell(d time.Duration) int {
	secs := int(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	return secs
}

// writeMonthlyQuotaDenied emits a Problem+JSON 429 with enough
// detail for the customer's client to surface "you hit your
// monthly cap" plus the actual cap + observed counter. Kept
// separate from the rate-limit 429 so dashboards can split the
// two failure modes cleanly.
//
// The body is built via `encoding/json.Marshal` so the
// caller-controlled `r.URL.Path` is properly escaped (any quote
// / control char in a maliciously-crafted path can't break out
// of the JSON string).
func writeMonthlyQuotaDenied(w http.ResponseWriter, r *http.Request, quota, used int64) {
	payload := map[string]any{
		"type":          "https://api.stellarindex.io/errors/monthly-quota-exceeded",
		"title":         "Monthly quota exceeded",
		"status":        429,
		"detail":        "The API key's monthly request quota has been reached. Reset on the 1st UTC.",
		"instance":      r.URL.Path,
		"monthly_quota": quota,
		"month_to_date": used,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		// JSON marshal of a map[string]{string,int64} cannot fail
		// under any input the production path produces; treat as
		// a defence-in-depth fallback.
		body = []byte(`{"title":"Monthly quota exceeded","status":429}`)
	}
	w.Header().Set("Content-Type", "application/problem+json")
	// Per-key denial on a per-URL cache key — never shared-cacheable
	// (overrides the route directive CacheControl pre-set; matches
	// the rate limiter's 429 handling in ratelimit.go).
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-StellarIndex-Monthly-Quota", itoa(quota))
	w.Header().Set("X-StellarIndex-Monthly-Used", itoa(used))
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(body)
}

// writeMonthlyQuotaUnavailable is the W1-flow-register-4 fail-CLOSED
// response: the month-to-date counter has been unreadable for longer
// than the dwell window, so the middleware can no longer prove the
// caller is under cap and stops serving unmetered. Mirrors
// writeMonthlyQuotaDenied's Problem+JSON shape (same 429 status,
// Content-Type, and no-store Cache-Control) so a client handles it on
// the same path as a real over-cap denial — but carries a DISTINCT
// error `type`/`title` (it honestly says "temporarily unavailable",
// not "you exceeded your quota", because we could not read the counter)
// plus a Retry-After so a well-behaved client backs off until the
// counter recovers. `month_to_date` is omitted: the read failed, so we
// have no honest value to report.
//
// Retry-After is set BEFORE WriteHeader; net/http drops headers added
// after the status line is committed.
func writeMonthlyQuotaUnavailable(w http.ResponseWriter, r *http.Request, quota int64, retryAfter int) {
	payload := map[string]any{
		"type":          "https://api.stellarindex.io/errors/monthly-quota-unavailable",
		"title":         "Monthly quota temporarily unavailable",
		"status":        429,
		"detail":        "The monthly usage counter is temporarily unavailable and the cap cannot be verified; retry after the indicated delay.",
		"instance":      r.URL.Path,
		"monthly_quota": quota,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		// Same defence-in-depth fallback as writeMonthlyQuotaDenied: a
		// map[string]{string,int64} cannot fail to marshal in production.
		body = []byte(`{"title":"Monthly quota temporarily unavailable","status":429}`)
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-StellarIndex-Monthly-Quota", itoa(quota))
	w.Header().Set("Retry-After", itoa(int64(retryAfter)))
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(body)
}

// itoa is the tiny non-allocating int64-to-decimal-string helper
// the response-header values use. strconv is fine but the header
// is small + on the cap-hit path; this keeps the formatter
// trivial.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
