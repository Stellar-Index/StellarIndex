// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// MonthToDateReader is the storage-side primitive [MonthlyQuota] uses (production: `*usage.Counter.MonthToDate`), a narrow interface so tests can fake it.
type MonthToDateReader interface {
	MonthToDate(ctx context.Context, subject string) (int64, error)
}

// DefaultMonthlyQuotaDwellTime is how long [MonthlyQuota] may fail OPEN on month-to-date read errors before flipping to fail-CLOSED (429 + Retry-After); tune via api.monthly_quota_dwell.
const DefaultMonthlyQuotaDwellTime = 30 * time.Second

// MonthlyQuotaOption tunes the [MonthlyQuota] middleware; variadic so the two-argument call site stays source-compatible.
type MonthlyQuotaOption func(*monthlyQuotaGate)

// WithMonthlyQuotaClock overrides the dwell-time clock so tests can drive transitions without time.Sleep; nil is ignored (keeps [time.Now]).
func WithMonthlyQuotaClock(now func() time.Time) MonthlyQuotaOption {
	return func(g *monthlyQuotaGate) {
		if now != nil {
			g.nowFn = now
		}
	}
}

// WithMonthlyQuotaDwellTime overrides the fail-open dwell window; a negative value disables the inversion, reopening a billing-overage vector (a capped key bills unmetered through a counter outage).
func WithMonthlyQuotaDwellTime(d time.Duration) MonthlyQuotaOption {
	return func(g *monthlyQuotaGate) { g.dwellTime = d }
}

// monthlyQuotaGate holds the dwell-time fail-open to fail-closed state shared across every
// request through one [MonthlyQuota] instance. The read errors are Redis transport failures, not
// per-key, so a single process-wide clock is the right granularity.
type monthlyQuotaGate struct {
	dwellTime time.Duration
	nowFn     func() time.Time

	mu              sync.Mutex
	redisErrorSince time.Time
	// healthySince starts the CURRENT unbroken run of counter successes; any failure resets it.
	// redisErrorSince clears only once this streak lasts dwellTime, so one stray success under a
	// flapping counter cannot reopen the unmetered window.
	healthySince time.Time
	// lastFailure lets a success after a failure-free dwellTime clear the clock, so a long-ago
	// outage cannot make the next blip fail closed.
	lastFailure time.Time
}

// observeReadFailure stamps the dwell-time clock and reports whether the window has elapsed:
// true means fail CLOSED (429), false means fail OPEN. Any failure breaks the recovery streak,
// and a negative dwellTime always returns false.
func (g *monthlyQuotaGate) observeReadFailure() bool {
	if g.dwellTime < 0 {
		return false
	}
	now := g.nowFn()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.healthySince = time.Time{} // any failure breaks the recovery streak
	g.lastFailure = now
	if g.redisErrorSince.IsZero() {
		g.redisErrorSince = now
		return false
	}
	return now.Sub(g.redisErrorSince) > g.dwellTime
}

// observeReadSuccess advances the recovery streak; see healthySince and lastFailure for why the
// fail-closed clock clears only after dwellTime of UNBROKEN successes or of no failures.
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
	if now.Sub(g.healthySince) >= g.dwellTime || now.Sub(g.lastFailure) > g.dwellTime {
		g.redisErrorSince = time.Time{}
		g.healthySince = time.Time{}
	}
}

// MonthlyQuota enforces `Subject.MonthlyQuota` against the ACCOUNT-wide month-to-date count of BILLABLE traffic (429 at the cap), failing OPEN inside the dwell window and CLOSED past it.
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
			// Detached from the client's cancellation, bounded by [throttleTakeTimeout], as the
			// rate-limit take is. The process-wide dwell clock cannot tell a caller-cancelled read
			// from a counter outage, so a client that connects and RSTs repeatedly would keep it
			// armed and, past the window, 429 EVERY metered customer while the counter is healthy.
			readCtx, readCancel := throttleContext(r)
			used, err := reader.MonthToDate(readCtx, id) //nolint:contextcheck // intentional detach — a client abort must not arm this gate's process-wide dwell clock; see throttleContext
			readCancel()
			if err != nil {
				if gate.observeReadFailure() {
					// Unreadable past the dwell window: failing open would let a key at its cap bill
					// unmetered for the whole outage, unrecoverable once served. Fail CLOSED.
					obs.MonthlyQuotaFailClosedTotal.Inc()
					logger.Warn("monthly-quota: read failing past dwell window; failing closed",
						"err", err, "subject", id)
					writeMonthlyQuotaUnavailable(w, r, subject.MonthlyQuota, retryAfterForDwell(gate.dwellTime))
					return
				}
				// Transient blip inside the dwell window: the ceiling is OFF for this request, an
				// overage unrecoverable once served. Counted so the signal survives the production
				// log level, where a Debug line would be invisible.
				obs.MonthlyQuotaFailOpenTotal.Inc()
				logger.Warn("monthly-quota: read failed; failing open",
					"err", err, "subject", id)
				next.ServeHTTP(w, r)
				return
			}
			gate.observeReadSuccess()
			if used >= subject.MonthlyQuota {
				writeMonthlyQuotaDenied(w, r, subject.MonthlyQuota, used, retryAfterForMonthBoundary(gate.nowFn()))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// retryAfterForDwell derives the fail-closed Retry-After (seconds) from the dwell window,
// floored at 1s. It is only reached with a positive dwellTime, since a negative one disables
// the inversion.
func retryAfterForDwell(d time.Duration) int {
	secs := int(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	return secs
}

// retryAfterForMonthBoundary derives the Retry-After (seconds) for a monthly-quota-exceeded 429:
// the gap to the 1st of the next UTC month, when [usage.Counter.MonthToDate] rolls over. It is
// floored at 1s so a request landing on the boundary still gets a positive value.
func retryAfterForMonthBoundary(now time.Time) int {
	now = now.UTC()
	year, month, _ := now.Date()
	next := time.Date(year, month+1, 1, 0, 0, 0, 0, time.UTC)
	secs := int(next.Sub(now).Seconds())
	if secs < 1 {
		secs = 1
	}
	return secs
}

// writeMonthlyQuotaDenied emits a Problem+JSON 429 carrying the cap and observed counter, distinct
// from the rate-limit 429 so dashboards can split the two. `month_to_date` is the ACCOUNT's count,
// so rotating the key does not clear it. json.Marshal escapes the caller-controlled r.URL.Path.
func writeMonthlyQuotaDenied(w http.ResponseWriter, r *http.Request, quota, used int64, retryAfter int) {
	payload := map[string]any{
		"type":          "https://api.stellarindex.io/errors/monthly-quota-exceeded",
		"title":         "Monthly quota exceeded",
		"status":        429,
		"detail":        "The account's monthly request quota has been reached (every API key on the account shares it). Reset on the 1st UTC.",
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
	// Per-key denial on a per-URL cache key: never shared-cacheable. This overrides the route
	// directive CacheControl pre-set, as the rate limiter's 429 does.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-StellarIndex-Monthly-Quota", strconv.FormatInt(quota, 10))
	w.Header().Set("X-StellarIndex-Monthly-Used", strconv.FormatInt(used, 10))
	// Retry-After BEFORE WriteHeader, as net/http drops later headers. Without it an SDK would
	// retry tightly against a key that cannot recover until the month rolls over.
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(body)
}

// writeMonthlyQuotaUnavailable is the fail-CLOSED response for a counter unreadable past the dwell
// window. It has a DISTINCT error type/title because it must not claim the quota was exceeded, and
// omits `month_to_date` since there is no honest value. Retry-After is set BEFORE WriteHeader.
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
	w.Header().Set("X-StellarIndex-Monthly-Quota", strconv.FormatInt(quota, 10))
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(body)
}
