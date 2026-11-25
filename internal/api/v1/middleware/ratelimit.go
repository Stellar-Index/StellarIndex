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

// throttleTakeTimeout bounds the throttle's own backend round-trip; the take ignores request cancellation (see [throttleContext]),
// so a wedged Redis would otherwise pin the request goroutine.
const throttleTakeTimeout = 5 * time.Second

// throttleContext derives the context for an abuse-prevention backend call: request values WITHOUT cancellation, bounded by
// [throttleTakeTimeout]. A client abort must not reach the limiter: it reads as a Redis outage and fails CLOSED for every caller.
func throttleContext(r *http.Request) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(throttleTakeTimeout)
	if reqDeadline, ok := r.Context().Deadline(); ok && reqDeadline.Before(deadline) {
		deadline = reqDeadline
	}
	return context.WithDeadline(context.WithoutCancel(r.Context()), deadline)
}

// MaxRateLimitKeyLen caps the KeyFn output so a hostile header (multi-KB X-Forwarded-For) cannot bloat the Redis key space or
// Bucket.Take allocation.
const MaxRateLimitKeyLen = 256

// RateLimit enforces bucket per keyFn key (429 with Retry-After); Redis failures fail OPEN inside [ratelimit.DefaultDwellTime] and
// CLOSED with 503 past it, and the two must not be collapsed.
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

			charge := &rateLimitCharge{bucket: bucket, key: key, logger: logger, paid: 1, tier: rateLimitTier(r, 0)}
			defer charge.record()
			if !charge.spend(w, r, 1) { //nolint:contextcheck // intentional detach inside spend — see throttleContext
				return
			}
			next.ServeHTTP(w, r.WithContext(withRateLimitCharge(r.Context(), r, charge)))
		})
	}
}

// rateLimitCharge is one request's account with the limiter: bucket, key and tokens paid. The middleware opens it at one token;
// a handler whose work scales with a client parameter re-prices via [ChargeRateLimit]. The mutex covers worker goroutines.
type rateLimitCharge struct {
	bucket   *ratelimit.Bucket
	key      string
	override int
	logger   *slog.Logger
	// perIP is a second charge every token also pays (see [sep10PerIPCharge]); nil for other subjects.
	perIP *rateLimitCharge

	// tier labels the request's usage metrics (see [rateLimitTier]); empty on a per-IP charge, which its parent records.
	tier string

	mu   sync.Mutex
	paid int
	// result is the most severe outcome of any charge so far and used the highest share of the budget spent, both read by
	// [rateLimitCharge.record]; neither touches the limiter.
	result string
	used   float64
}

// rateLimitResultRank orders outcomes so a request is recorded by its worst charge.
var rateLimitResultRank = map[string]int{
	obs.RateLimitResultAllowed: 1, obs.RateLimitResultFailOpen: 2, obs.RateLimitResultLimited: 3, obs.RateLimitResultUnavailable: 4,
}

// note folds one charge's outcome into the request's; fraction is the budget share spent after it.
func (c *rateLimitCharge) note(result string, fraction float64) {
	if rateLimitResultRank[result] > rateLimitResultRank[c.result] {
		c.result = result
	}
	c.used = max(c.used, min(fraction, 1))
}

// record counts the request once, by its worst outcome across every charge (its own and its per-IP charge's), and for an allowed
// request observes the share of the binding budget it left spent. Runs after the handler, so it covers re-prices and cold fills.
func (c *rateLimitCharge) record() {
	c.mu.Lock()
	defer c.mu.Unlock()
	result, used := c.result, c.used
	if c.perIP != nil {
		if rateLimitResultRank[c.perIP.result] > rateLimitResultRank[result] {
			result = c.perIP.result
		}
		used = max(used, c.perIP.used)
	}
	if result == "" {
		return
	}
	obs.RateLimitDecisionsTotal.WithLabelValues(c.tier, result).Inc()
	if result == obs.RateLimitResultAllowed {
		obs.RateLimitUtilisationRatio.WithLabelValues(c.tier).Observe(used)
	}
}

// rateLimitTier is the bounded tier label for r's usage metrics: the subject's auth tier, [obs.RateLimitTierCustom] when it carries
// its own ceiling, anonymous when there is no subject. Never a client identity.
func rateLimitTier(r *http.Request, override int) string {
	if override > 0 {
		return obs.RateLimitTierCustom
	}
	subject, ok := auth.SubjectFrom(r.Context())
	if !ok || subject.Identifier == "" || subject.Tier == "" {
		return auth.TierAnonymous.String()
	}
	switch subject.Tier {
	case auth.TierAnonymous, auth.TierAPIKey, auth.TierSEP10, auth.TierOperator:
		return subject.Tier.String()
	}
	return obs.RateLimitTierOther
}

type rateLimitChargeKey struct{}

// withRateLimitCharge carries c in r's context for [ChargeRateLimit] and, below the api layer, for [ratelimit.ChargeColdFill].
func withRateLimitCharge(ctx context.Context, r *http.Request, c *rateLimitCharge) context.Context {
	ctx = context.WithValue(ctx, rateLimitChargeKey{}, c)
	return ratelimit.WithColdFillCharge(ctx, func(tokens int) error { return c.chargeColdFill(r, tokens) })
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
// With a per-IP charge, the headers describe whichever bucket binds: the refusing one, else the one with fewer tokens left.
func (c *rateLimitCharge) spend(w http.ResponseWriter, r *http.Request, tokens int) bool {
	var ipRes *ratelimit.Result
	if c.perIP != nil {
		res, ok := c.perIP.take(w, r, tokens)
		if !ok {
			return false
		}
		ipRes = res
	}
	res, ok := c.take(w, r, tokens)
	if !ok {
		return false
	}
	switch {
	case ipRes != nil && (res == nil || ipRes.Remaining < res.Remaining):
		c.perIP.setHeaders(w, *ipRes)
	case res != nil:
		c.setHeaders(w, *res)
	}
	return true
}

// take charges tokens against this bucket alone. It returns false once it has written a 429 (with this bucket's headers)
// or a fail-closed 503; a nil result with true means the limiter failed open.
func (c *rateLimitCharge) take(w http.ResponseWriter, r *http.Request, tokens int) (*ratelimit.Result, bool) {
	res, refusal := c.bill(r, tokens)
	if refusal != nil {
		refusal.write(w, r)
		return nil, false
	}
	return res, true
}

// bill is take without writing: a refusal carries what its 429/503 needs; a nil result and nil refusal means the limiter failed open.
func (c *rateLimitCharge) bill(r *http.Request, tokens int) (*ratelimit.Result, *rateLimitRefusal) {
	takeCtx, takeCancel := throttleContext(r)
	res, err := c.bucket.Charge(takeCtx, c.key, tokens, c.override)
	takeCancel()
	if err != nil {
		if errors.Is(err, ratelimit.ErrThrottleUnavailable) {
			// Sustained Redis outage: fail CLOSED rather than disable the limiter indefinitely.
			c.logger.Warn("ratelimit unavailable — failing closed (sustained Redis errors)",
				"err", err, "key", c.key, "request_id", RequestIDFrom(r))
			obs.RateLimitFailClosedTotal.WithLabelValues(obs.RateLimiterAPI).Inc()
			c.note(obs.RateLimitResultUnavailable, 0)
			return nil, &rateLimitRefusal{c: c, unavailable: true}
		}
		// Debug level so an outage does not flood the error log; the metric is the alert signal.
		c.logger.Debug("ratelimit redis error — failing open",
			"err", err, "key", c.key, "request_id", RequestIDFrom(r))
		obs.RateLimitFailOpenTotal.Inc()
		c.note(obs.RateLimitResultFailOpen, 0)
		return nil, nil
	}
	// Count, not Remaining: a denied charge still counts and Remaining clamps at zero.
	fraction := float64(res.Count) / float64(c.effectiveMax())
	if !res.Allowed {
		c.note(obs.RateLimitResultLimited, fraction)
		return nil, &rateLimitRefusal{c: c, res: res}
	}
	c.note(obs.RateLimitResultAllowed, fraction)
	return &res, nil
}

// rateLimitRefusal is a denied charge: over budget (429) or the limiter failed closed (503).
type rateLimitRefusal struct {
	c           *rateLimitCharge
	res         ratelimit.Result
	unavailable bool
}

func (e *rateLimitRefusal) Error() string { return ratelimit.ErrColdFillRefused.Error() }

func (e *rateLimitRefusal) Unwrap() error { return ratelimit.ErrColdFillRefused }

func (e *rateLimitRefusal) write(w http.ResponseWriter, r *http.Request) {
	if e.unavailable {
		writeThrottleUnavailableProblem(w, r)
		return
	}
	e.c.setHeaders(w, e.res)
	retryAfter := max(int(e.res.RetryAfter.Seconds()), 1)
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	writeRateLimitProblem(w, r, retryAfter)
}

// chargeColdFill bills tokens for a cold fill without writing (it may run on a handler's worker goroutines); a refusal is returned
// for the handler to answer through [WriteRateLimitRefusal].
func (c *rateLimitCharge) chargeColdFill(r *http.Request, tokens int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.perIP != nil {
		if _, refusal := c.perIP.bill(r, tokens); refusal != nil {
			return refusal.counted()
		}
	}
	if _, refusal := c.bill(r, tokens); refusal != nil {
		return refusal.counted()
	}
	return nil
}

func (e *rateLimitRefusal) counted() error {
	if !e.unavailable {
		obs.RateLimitColdFillRefusedTotal.Inc()
	}
	return e
}

// WriteRateLimitRefusal answers err with the limiter's 429 (or fail-closed 503) when it is a refused cold-fill charge
// ([ratelimit.ChargeColdFill]) and reports whether it wrote.
func WriteRateLimitRefusal(w http.ResponseWriter, r *http.Request, err error) bool {
	var refusal *rateLimitRefusal
	if !errors.As(err, &refusal) {
		return false
	}
	refusal.write(w, r)
	return true
}

func (c *rateLimitCharge) setHeaders(w http.ResponseWriter, res ratelimit.Result) {
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(c.effectiveMax()))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(res.Remaining))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(nextWindowResetUnix(c.bucket.Window()), 10))
}

// ChargeRateLimit re-prices the in-flight request at cost tokens IN TOTAL (capped at the subject's ceiling, no refund); false means it
// already wrote the 429/503 and the handler must return without writing.
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

// RateLimitBySubject enforces separate anonymous (per-IP) and authenticated (owner-account) buckets; Subject.RateLimitPerMin > 0
// replaces the ceiling for THIS request, and nil buckets disable that class.
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

			charge := &rateLimitCharge{
				bucket: bucket, key: key, override: override, logger: logger, paid: 1,
				tier: rateLimitTier(r, override),
			}
			charge.perIP = sep10PerIPCharge(r, bucket, authBucket, logger)
			defer charge.record()
			if !charge.spend(w, r, 1) { //nolint:contextcheck // intentional detach inside spend — see throttleContext
				return
			}
			next.ServeHTTP(w, r.WithContext(withRateLimitCharge(r.Context(), r, charge)))
		})
	}
}

// bucketKeyAndOverrideForRequest picks the bucket, Redis key and per-subject limit override (only for an authenticated APIKey with
// RateLimitPerMin). Anonymous keying is the client IP ALONE: Subject.Identifier hashes the client-controlled User-Agent.
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

// sep10PerIPCharge also holds a SEP-10 JWT caller's IP to one subject's authenticated budget: the JWT's account is a keypair
// anyone mints for free, so keyed on the account alone one IP would get a fresh budget per keypair. API keys are exempt.
func sep10PerIPCharge(r *http.Request, bucket, authBucket *ratelimit.Bucket, logger *slog.Logger) *rateLimitCharge {
	key, ok := sep10PerIPKey(r)
	if !ok || bucket != authBucket {
		return nil
	}
	return &rateLimitCharge{bucket: authBucket, key: key, logger: logger, paid: 1}
}

// sep10PerIPKey is the per-IP key a SEP-10 JWT subject (no key id) is held to; false for every other subject.
func sep10PerIPKey(r *http.Request) (string, bool) {
	subject, ok := auth.SubjectFrom(r.Context())
	if !ok || !subject.IsSEP10Session() {
		return "", false
	}
	return "sep10ip:" + anonymousRateLimitKey(r), true
}

// RateLimitCallerKey is the identity a per-caller cap charges: the client IP for a SEP-10 JWT subject (a free keypair),
// the owner account for any other authenticated subject, else the throttle IP.
func RateLimitCallerKey(r *http.Request) string {
	if key, ok := sep10PerIPKey(r); ok {
		return key
	}
	if subject, ok := auth.SubjectFrom(r.Context()); ok && subject.Identifier != "" &&
		subject.Tier != auth.TierAnonymous && subject.Tier != "" {
		return authenticatedRateLimitKey(subject)
	}
	return anonymousRateLimitKey(r)
}

// anonymousRateLimitKey derives the IP-ONLY throttle key for an anonymous caller via [remoteIPPrefixFor] (works before Logger runs),
// falling back to one shared "anon" bucket when no IP resolves.
func anonymousRateLimitKey(r *http.Request) string {
	ip := remoteIPPrefixFor(r)
	if ip == "" {
		return "anon"
	}
	return "anon:" + ip
}

// authenticatedRateLimitKey counts every credential an account holds against ONE bucket via [UsageKeyForSubject], shared with the
// monthly quota so budgets cannot drift apart. Keyed per credential, the ceiling would multiply by keys held.
func authenticatedRateLimitKey(subject auth.Subject) string {
	return "auth:" + UsageKeyForSubject(subject)
}

// SkipHealthAndMetrics is a Skip predicate exempting liveness probes and prometheus scrapes; keep in lockstep with
// isUnauthenticatedInfraPath (auth.go) or a monitor behind a shared NAT flaps on 429.
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

// writeThrottleUnavailableProblem is the fail-closed 503, with a type URL and status distinct from writeRateLimitProblem.
// Retry-After is set BEFORE WriteHeader because net/http drops later headers.
func writeThrottleUnavailableProblem(w http.ResponseWriter, r *http.Request) {
	p := rlProblem{
		Type:     "https://api.stellarindex.io/errors/throttle-unavailable",
		Title:    "Throttle layer unavailable",
		Status:   http.StatusServiceUnavailable,
		Detail:   "the abuse-prevention layer has been unreachable for an extended period; retry in a moment",
		Instance: RequestInstance(r),
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
		Instance: RequestInstance(r),
	}
	w.Header().Set("Content-Type", "application/problem+json")
	// Override the per-route cache directive: a cached 429 would replay the denial to other
	// anonymous clients on the same CDN key past their own budget reset.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(p)
}
