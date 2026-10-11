package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// throttleTakeTimeout bounds the throttle's own backend round-trip.
//
// The take ignores request cancellation (see [throttleContext]), so a wedged Redis would otherwise
// pin the request goroutine.
const throttleTakeTimeout = 5 * time.Second

// throttleContext derives the context for an abuse-prevention backend call (rate-limit take,
// [MonthlyQuota] read, failed-auth throttle): the request's values WITHOUT its cancellation,
// bounded by [throttleTakeTimeout] or the request's remaining deadline, whichever is sooner.
//
// A client abort must not reach the limiter as an error: the bucket cannot tell a cancelled call
// from a Redis outage (every take error arms its dwell clock), so connection resets would fail
// CLOSED with 503 for EVERY caller sharing that bucket. The request's server-set DEADLINE is
// re-applied so the pre-handler stack cannot outrun [RequestTimeout].
func throttleContext(r *http.Request) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(throttleTakeTimeout)
	if reqDeadline, ok := r.Context().Deadline(); ok && reqDeadline.Before(deadline) {
		deadline = reqDeadline
	}
	return context.WithDeadline(context.WithoutCancel(r.Context()), deadline)
}

// MaxRateLimitKeyLen caps the KeyFn output so a hostile header (multi-KB X-Forwarded-For)
// cannot bloat the Redis key space or the allocation inside Bucket.Take.
const MaxRateLimitKeyLen = 256

// RateLimit returns middleware enforcing bucket on every request whose keyFn yields a non-empty
// key, setting X-RateLimit-Limit/Remaining and, on 429, Retry-After plus a problem+json body.
//
// Redis failures follow the bucket's dwell-time policy: inside [ratelimit.DefaultDwellTime] fail
// OPEN; [ratelimit.ErrThrottleUnavailable] past it fails CLOSED with 503. Do not collapse the two.
//
// keyFn defaults to per-IP and "" means not limited; skip bypasses infra endpoints. Oversize keys
// are truncated to [MaxRateLimitKeyLen] and still bucketed.
func RateLimit(bucket *ratelimit.Bucket, keyFn func(*http.Request) string, skip func(*http.Request) bool, logger *slog.Logger) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	if keyFn == nil {
		// /64-masked like the production anon path: a full IPv6 /128 key lets a caller rotate
		// addresses within one delegated /64 to mint unlimited buckets.
		keyFn = func(r *http.Request) string { return remoteIPPrefixFor(r) }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if skip != nil && skip(r) {
				next.ServeHTTP(w, r)
				return
			}
			key := keyFn(r)
			if key == "" {
				// No identifiable subject — let through. This is rare
				// in practice since Logger populates remote_ip.
				next.ServeHTTP(w, r)
				return
			}
			if len(key) > MaxRateLimitKeyLen {
				key = key[:MaxRateLimitKeyLen]
			}

			charge := &rateLimitCharge{bucket: bucket, key: key, logger: logger, paid: 1}
			if !charge.spend(w, r, 1) { //nolint:contextcheck // intentional detach inside spend — see throttleContext
				return
			}
			next.ServeHTTP(w, r.WithContext(withRateLimitCharge(r.Context(), charge)))
		})
	}
}

// rateLimitCharge is one request's account with the limiter: the bucket, key and tokens paid so
// far. The middleware opens it with the one-token base charge; a handler whose work scales with
// a client-chosen parameter re-prices via [ChargeRateLimit]. The mutex covers worker goroutines.
type rateLimitCharge struct {
	bucket   *ratelimit.Bucket
	key      string
	override int
	logger   *slog.Logger

	mu   sync.Mutex
	paid int
}

type rateLimitChargeKey struct{}

func withRateLimitCharge(ctx context.Context, c *rateLimitCharge) context.Context {
	return context.WithValue(ctx, rateLimitChargeKey{}, c)
}

// effectiveMax is the ceiling this request's subject is held to: the
// per-subject override when one is set, else the bucket's own max.
func (c *rateLimitCharge) effectiveMax() int {
	if c.override > 0 {
		return c.override
	}
	return c.bucket.Max()
}

// spend charges tokens and settles the outcome onto w: true to proceed, false once it has written
// a 429 or 503. Shared by the base charge and [ChargeRateLimit] so policy and headers cannot drift.
func (c *rateLimitCharge) spend(w http.ResponseWriter, r *http.Request, tokens int) bool {
	takeCtx, takeCancel := throttleContext(r)
	res, err := c.bucket.Charge(takeCtx, c.key, tokens, c.override)
	takeCancel()
	if err != nil {
		if errors.Is(err, ratelimit.ErrThrottleUnavailable) {
			// Sustained Redis outage: fail CLOSED rather than disable the limiter indefinitely.
			c.logger.Warn("ratelimit unavailable — failing closed (sustained Redis errors)",
				"err", err, "key", c.key, "request_id", RequestIDFrom(r))
			obs.RateLimitFailClosedTotal.WithLabelValues(obs.RateLimiterAPI).Inc()
			writeThrottleUnavailableProblem(w, r)
			return false
		}
		// Debug level so an outage does not flood the error log; the metric is the alert signal.
		c.logger.Debug("ratelimit redis error — failing open",
			"err", err, "key", c.key, "request_id", RequestIDFrom(r))
		obs.RateLimitFailOpenTotal.Inc()
		return true
	}

	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(c.effectiveMax()))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(res.Remaining))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(nextWindowResetUnix(c.bucket.Window()), 10))

	if !res.Allowed {
		retryAfter := int(res.RetryAfter.Seconds())
		if retryAfter < 1 {
			retryAfter = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeRateLimitProblem(w, r, retryAfter)
		return false
	}
	return true
}

// ChargeRateLimit re-prices the in-flight request at cost tokens IN TOTAL and charges the
// difference over what it has paid. False means it already wrote the 429 or 503; the handler must
// return without writing.
//
// The middleware charges one token before dispatch because that is all it can know. Where the
// client chooses the work (a 1000-id POST /v1/price/batch) the handler calls this AFTER validating
// and BEFORE the work; the middleware cannot, as pricing the POST means buffering up to 1 MiB for
// an unadmitted caller.
//
// The total is capped at the subject's ceiling so an over-ceiling request spends the whole window
// instead of being refused every window. A cost at or below what was paid charges nothing (no refund).
func ChargeRateLimit(w http.ResponseWriter, r *http.Request, cost int) bool {
	c, ok := r.Context().Value(rateLimitChargeKey{}).(*rateLimitCharge)
	if !ok || c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if ceiling := c.effectiveMax(); cost > ceiling {
		cost = ceiling
	}
	extra := cost - c.paid
	if extra <= 0 {
		return true
	}
	c.paid += extra
	return c.spend(w, r, extra) //nolint:contextcheck // intentional detach inside spend — see throttleContext
}

// nextWindowResetUnix returns when the fixed-window bucket's CURRENT window ends, so clients can
// back off before a 429. It must match the bucket's key derivation: ((unix()/window) + 1) * window.
func nextWindowResetUnix(window time.Duration) int64 {
	if window < time.Second {
		// The constructor rejects this; guard the divide-by-zero should a variant allow it.
		return time.Now().Unix()
	}
	windowSecs := int64(window.Seconds())
	now := time.Now().Unix()
	return ((now / windowSecs) + 1) * windowSecs
}

// RateLimitBySubject enforces separate anonymous and authenticated buckets. Authenticated callers
// key on the owner account ([authenticatedRateLimitKey]); anonymous ones on the per-IP bucket.
// A Subject.RateLimitPerMin > 0 replaces authBucket's max as the ceiling for THIS request while
// the counter stays the account's. Nil buckets disable limiting for that class.
func RateLimitBySubject(anonBucket, authBucket *ratelimit.Bucket, skip func(*http.Request) bool, logger *slog.Logger) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if skip != nil && skip(r) {
				next.ServeHTTP(w, r)
				return
			}

			bucket, key, override := bucketKeyAndOverrideForRequest(r, anonBucket, authBucket)
			if bucket == nil || key == "" {
				next.ServeHTTP(w, r)
				return
			}
			if len(key) > MaxRateLimitKeyLen {
				key = key[:MaxRateLimitKeyLen]
			}

			charge := &rateLimitCharge{bucket: bucket, key: key, override: override, logger: logger, paid: 1}
			if !charge.spend(w, r, 1) { //nolint:contextcheck // intentional detach inside spend — see throttleContext
				return
			}
			next.ServeHTTP(w, r.WithContext(withRateLimitCharge(r.Context(), charge)))
		})
	}
}

// bucketKeyAndOverrideForRequest picks the bucket, Redis key and any per-subject limit override.
// The override is non-zero ONLY for an authenticated subject whose APIKey sets RateLimitPerMin.
//
// Anonymous keying is the resolved client IP ALONE: the anonymous Subject.Identifier hashes in
// the client-controlled User-Agent, so keying on it would let a caller mint a bucket per request.
func bucketKeyAndOverrideForRequest(r *http.Request, anonBucket, authBucket *ratelimit.Bucket) (*ratelimit.Bucket, string, int) {
	if subject, ok := auth.SubjectFrom(r.Context()); ok && subject.Identifier != "" {
		if subject.Tier != auth.TierAnonymous && subject.Tier != "" {
			if authBucket == nil {
				return nil, "", 0
			}
			return authBucket, authenticatedRateLimitKey(subject), subject.RateLimitPerMin
		}
		if anonBucket == nil {
			return nil, "", 0
		}
		// IP alone: subject.Identifier folds in the client-rotatable User-Agent.
		return anonBucket, anonymousRateLimitKey(r), 0
	}
	if anonBucket == nil {
		return nil, "", 0
	}
	return anonBucket, anonymousRateLimitKey(r), 0
}

// RateLimitCallerKey is the caller identity the rate limiter charges:
// the owner account for an authenticated subject, else the throttle IP.
func RateLimitCallerKey(r *http.Request) string {
	if subject, ok := auth.SubjectFrom(r.Context()); ok && subject.Identifier != "" &&
		subject.Tier != auth.TierAnonymous && subject.Tier != "" {
		return authenticatedRateLimitKey(subject)
	}
	return anonymousRateLimitKey(r)
}

// anonymousRateLimitKey derives the IP-ONLY throttle key for an anonymous caller. It uses
// [remoteIPPrefixFor] so it works before Logger has run, and falls back to one shared "anon"
// bucket when no IP resolves, the safe choice for a throttle.
func anonymousRateLimitKey(r *http.Request) string {
	ip := remoteIPPrefixFor(r)
	if ip == "" {
		return "anon"
	}
	return "anon:" + ip
}

// authenticatedRateLimitKey counts every credential an account holds against ONE bucket via
// [UsageKeyForSubject], which the monthly quota shares so the budgets cannot drift onto different
// identities. Keyed per credential, the ceiling would multiply by the number of keys held.
func authenticatedRateLimitKey(subject auth.Subject) string {
	return "auth:" + UsageKeyForSubject(subject)
}

// SkipHealthAndMetrics is a Skip predicate that exempts liveness probes and prometheus scrapes.
// Keep in lockstep with isUnauthenticatedInfraPath (auth.go): a probe the LB sends
// unauthenticated must not spend the anonymous per-IP bucket, or a monitor behind a shared NAT
// flaps the region on 429.
func SkipHealthAndMetrics(r *http.Request) bool {
	switch r.URL.Path {
	case "/v1/healthz", "/v1/readyz", "/v1/livez/lake", "/v1/version", "/metrics", "/robots.txt", "/":
		return true
	}
	return strings.HasPrefix(r.URL.Path, "/errors/")
}

// rlProblem is a minimal RFC 9457 body duplicated here because v1 imports middleware (cycle).
type rlProblem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance"`
}

// writeThrottleUnavailableProblem is the 503 for the fail-closed branch, with a type URL and status
// distinct from writeRateLimitProblem so "rate-limited" differs from "limiter offline".
// Retry-After is set BEFORE WriteHeader because net/http drops later headers.
func writeThrottleUnavailableProblem(w http.ResponseWriter, r *http.Request) {
	p := rlProblem{
		Type:     "https://api.stellarindex.io/errors/throttle-unavailable",
		Title:    "Throttle layer unavailable",
		Status:   http.StatusServiceUnavailable,
		Detail:   "the abuse-prevention layer has been unreachable for an extended period; retry in a moment",
		Instance: r.URL.RequestURI(),
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	// 30s matches ratelimit.DefaultDwellTime, enough to ride out a typical Redis fail-over.
	w.Header().Set("Retry-After", "30")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(p)
}

func writeRateLimitProblem(w http.ResponseWriter, r *http.Request, retryAfter int) {
	p := rlProblem{
		Type:     "https://api.stellarindex.io/errors/rate-limited",
		Title:    "Rate limit exceeded",
		Status:   http.StatusTooManyRequests,
		Detail:   "Retry after " + strconv.Itoa(retryAfter) + "s",
		Instance: r.URL.RequestURI(),
	}
	w.Header().Set("Content-Type", "application/problem+json")
	// Override the per-route cache directive: a cached 429 would replay the denial to other
	// anonymous clients on the same CDN key past their own budget reset.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(p)
}
