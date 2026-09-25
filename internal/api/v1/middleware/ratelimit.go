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
// The take deliberately does NOT inherit the request's cancellation —
// see [throttleContext] — so, exactly like the post-response
// bookkeeping writes, it needs a bound of its own or a wedged Redis
// would pin the request goroutine forever. 5 s matches
// [postResponseWriteTimeout] and is generous relative to go-redis's 3 s
// default: a backstop, not the usual limiter.
const throttleTakeTimeout = 5 * time.Second

// throttleContext derives the context an abuse-prevention seam's
// backend call runs under: the request's values, WITHOUT its
// cancellation, bounded by [throttleTakeTimeout]. Shared by the two
// pre-dispatch seams in this package — the rate-limit take and the
// [MonthlyQuota] month-to-date read — because the hazard below is a
// property of the dwell-clock design both mirror, not of either seam.
//
// A client abort must not reach the limiter as an error. The bucket
// cannot tell a caller-cancelled call from a Redis outage — every error
// out of the take arms its dwell clock — so a client that opens a
// connection, sends a request and immediately RSTs, a few times a
// second, kept `redisErrorSince` armed and `healthySince` reset for as
// long as it cared to, and the 30 s unbroken-success streak needed to
// disarm could never accumulate. The limiter then answered
// ErrThrottleUnavailable and the middleware failed CLOSED with 503 for
// EVERY caller sharing that bucket (the whole anonymous tier, or the
// whole authenticated tier) while Redis was perfectly healthy: a remote
// kill switch for the API, costing an attacker one TCP handshake per
// tick (REL-06 F059, reverification-2026-09-18).
//
// Detaching is also the correct charge semantics, and closes the
// mirror-image hole: an aborted request had its take error out and the
// middleware fall OPEN, so the token was never spent. The request
// consumed the connection and the dispatch either way — the same
// reasoning [postResponseWriteTimeout]'s call site records for usage
// counters, where not counting an aborted request is a quota-evasion
// vector.
//
// The bound is only as hard as the backend's context honouring; go-redis
// respects ctx cancellation on the wire, so it holds for the bucket as
// wired today.
func throttleContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), throttleTakeTimeout)
}

// MaxRateLimitKeyLen caps the caller-supplied KeyFn output so a
// hostile header (multi-KB X-Forwarded-For) can't blow up the
// Redis key space or the url.QueryEscape allocation inside
// Bucket.Take. 256 bytes fits any legitimate IP (including
// IPv6 + zone) and any realistic API-key / SEP-10 account id
// with headroom.
const MaxRateLimitKeyLen = 256

// RateLimit returns middleware that enforces the given Bucket on
// every request whose KeyFn produces a non-empty key.
//
// Headers added on every response:
//
//	X-RateLimit-Limit:     <bucket max>
//	X-RateLimit-Remaining: <after this request>
//
// On 429, additionally:
//
//	Retry-After: <seconds until window resets>
//
// plus an RFC 9457 problem+json body.
//
// Redis failures follow the bucket's dwell-time policy (the
// internal/ratelimit package doc, "Failure mode"): a transient error
// inside the bucket's dwell-time (default [ratelimit.DefaultDwellTime])
// fails OPEN — logged at debug, the request goes through — and
// [ratelimit.ErrThrottleUnavailable], once the outage outlasts the
// dwell-time, fails CLOSED with 503 +
// Retry-After. Do not collapse the two branches into one fail-open.
//
// KeyFn decides the key: per-IP (default if nil), per-API-key, etc.
// If KeyFn returns "" the request is not rate-limited. Skip allows
// full bypass for infra endpoints (health probes, metrics).
//
// Key length is capped at [MaxRateLimitKeyLen]. A hostile
// X-Forwarded-For can grow up to the server's MaxHeaderBytes
// (1 MB default in net/http); without a cap here that would
// propagate through url.QueryEscape into the Redis key space.
// Oversize keys get truncated to the cap — still bucketed, just
// not uniquely per-caller past the first N bytes.
func RateLimit(bucket *ratelimit.Bucket, keyFn func(*http.Request) string, skip func(*http.Request) bool, logger *slog.Logger) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	if keyFn == nil {
		// Default key: the forge-resistant, /64-masked throttle identity
		// (remoteIPPrefixFor — the SAME resolver the production anon path
		// uses, F-1338 / SEC-15) rather than the raw RemoteIPFrom context
		// value. Keying on the full IPv6 /128 lets a caller rotate
		// addresses within a single delegated /64 to mint unlimited
		// distinct buckets and bypass the per-IP limit; masking to /64
		// keys on the block an attacker actually controls. This middleware
		// is test-only today, but the default MUST be bypass-resistant in
		// case it is ever wired live with a nil keyFn.
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

// rateLimitCharge is one request's account with the limiter: which
// bucket and key it is being charged against, and how many tokens it
// has paid so far. The middleware opens it with the one-token base
// charge every request pays before dispatch and plants it on the
// request context; a handler whose work scales with a client-chosen
// parameter then re-prices the request through [ChargeRateLimit] once
// it has parsed that parameter.
//
// It is per-request state. The mutex is there because nothing stops a
// handler calling [ChargeRateLimit] from a worker goroutine, not
// because any does today.
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

// spend charges tokens against the bucket and settles the outcome onto
// w. It returns true when the request may proceed, and false once it
// has written the response — a 429 (over budget) or a 503 (the throttle
// layer has been unreachable past its dwell-time). Shared by the
// pre-dispatch base charge and by [ChargeRateLimit] so the two cannot
// drift on the failure policy or the header set.
func (c *rateLimitCharge) spend(w http.ResponseWriter, r *http.Request, tokens int) bool {
	takeCtx, takeCancel := throttleContext(r)
	res, err := c.bucket.Charge(takeCtx, c.key, tokens, c.override)
	takeCancel()
	if err != nil {
		if errors.Is(err, ratelimit.ErrThrottleUnavailable) {
			// Dwell-time exceeded: sustained Redis outage —
			// fail-CLOSED with 503 + Retry-After rather than
			// disabling the rate limiter indefinitely. F-0050 /
			// F-0150 (audit-2026-05-27).
			c.logger.Warn("ratelimit unavailable — failing closed (sustained Redis errors)",
				"err", err, "key", c.key, "request_id", RequestIDFrom(r))
			writeThrottleUnavailableProblem(w, r)
			return false
		}
		// Log at debug so a Redis outage doesn't flood the error
		// log — the metric below is the alertable signal, the log
		// is for post-mortem detail.
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

// ChargeRateLimit re-prices the in-flight request at cost tokens IN
// TOTAL and charges the difference over what it has already paid. It
// returns true when the handler may go on, and false once it has
// written the 429 (or the fail-closed 503) itself — the handler must
// then return without writing anything.
//
// The limiter charges one token before dispatch because that is all it
// can know there. For most routes that is the right price. It is wrong
// for a route whose server-side work is chosen by the client: one token
// bought a 1000-id POST /v1/price/batch — a thousand alias-looped price
// resolutions, sixteen at a time against a 25-connection pool — so the
// deployed 6000/min anonymous budget was really six million resolutions
// a minute (F035 / F046 / K009, reverification-2026-09-18). Such a
// handler calls this AFTER it has validated the parameter and BEFORE it
// does the work, so the budget is denominated in work rather than in
// HTTP requests.
//
// Why here and not in the middleware: the cost of the POST variant is
// the length of an array in its JSON body. Pricing it before dispatch
// would mean buffering and decoding up to 1 MiB of body for a caller
// the limiter has not admitted yet — the limiter would be doing the
// expensive thing in order to decide whether to allow the expensive
// thing. The handler has to parse the body anyway, behind the base
// charge, and its count is the deduplicated one that matches the work.
//
// The total is capped at the subject's ceiling, so a request priced
// above it spends the whole window rather than being refused in every
// window ([ratelimit.Bucket.Charge] explains the cap; applying it to
// the TOTAL here is what keeps the base token from making such a
// request one token too expensive to ever fit). On success the
// X-RateLimit-Remaining header is restated to the post-charge value.
//
// A request that never crossed a rate-limit middleware (limiting
// disabled for its class, a skipped path, a handler under test) has no
// account to charge and proceeds. A cost at or below what has been paid
// charges nothing: this can raise a request's price, never refund it.
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

// nextWindowResetUnix returns the Unix-epoch seconds of when the
// fixed-window bucket's CURRENT window ends. Mirrors GitHub /
// Twitter's `X-RateLimit-Reset` semantics: clients can compute
// `seconds_until_reset = X-RateLimit-Reset - now` to back off
// proactively without waiting for a 429. See:
// https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-ratelimit-headers
//
// Implementation matches the bucket's key derivation:
//
//	bucket key = unix() / window.Seconds()
//	current window ends at = ((unix()/window) + 1) * window
func nextWindowResetUnix(window time.Duration) int64 {
	if window < time.Second {
		// Bucket constructor rejects this; defensive zero stops the
		// formula's divide-by-zero in case a future bucket variant
		// permits sub-second windows without rejecting at New().
		return time.Now().Unix()
	}
	windowSecs := int64(window.Seconds())
	now := time.Now().Unix()
	return ((now / windowSecs) + 1) * windowSecs
}

// RateLimitBySubject enforces separate anonymous and authenticated
// buckets. When an auth middleware has attached a Subject, anonymous
// callers use anonBucket and authenticated callers use authBucket.
//
// Keying semantics:
//   - authenticated with KeyID: per-key bucket
//   - authenticated without KeyID: per-subject Identifier bucket
//   - anonymous with Subject: anonymous Identifier bucket
//   - no Subject attached: fallback to RemoteIPFrom(r)
//
// Per-subject overrides: an authenticated subject with
// `Subject.RateLimitPerMin > 0` (a paid-tier override sourced from
// the APIKey record) replaces authBucket's default max for THIS
// caller's bucket. The override has no effect on anonymous callers —
// anonRateLimitPerMin is a deployment knob, not a per-IP override.
//
// Nil buckets disable rate limiting for that class.
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

// bucketKeyAndOverrideForRequest picks the right bucket + Redis key
// for the inbound request, plus any per-subject limit override.
//
// Override is non-zero ONLY for an authenticated subject whose
// APIKey record carries an explicit `RateLimitPerMin` (paid-tier
// custom plan). Anonymous callers always get `0` (the bucket's
// default applies). Operators can still floor the bucket via
// `cfg.API.KeyRateLimitPerMin` — the override only raises (or
// lowers) the per-key budget, never the global default.
//
// Anonymous keying is the resolved client IP ALONE (F-1335). The
// anonymous Subject.Identifier is a sha256(IP|User-Agent) hash that
// stays useful as a log/metric label, but it MUST NOT key the
// throttle bucket: a client can rotate its User-Agent on every
// request to mint unlimited distinct hashes, each its own bucket,
// trivially bypassing the per-IP anonymous floor. We key on
// anonymousRateLimitKey(r), which resolves the client IP through the
// trusted-proxy XFF logic (forge-resistant per F-1338) directly from
// the request — independent of whether the Logger middleware has
// populated the remote-IP context value yet.
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
		// Anonymous: key on resolved IP ALONE, not subject.Identifier
		// (which folds in the User-Agent — a client-rotatable value).
		return anonBucket, anonymousRateLimitKey(r), 0
	}
	if anonBucket == nil {
		return nil, "", 0
	}
	return anonBucket, anonymousRateLimitKey(r), 0
}

// anonymousRateLimitKey derives the per-IP throttle key for an
// anonymous caller. IP-ONLY by design (F-1335) — see
// [bucketKeyAndOverrideForRequest]. Uses [remoteIPPrefixFor] (the
// forge-resistant XFF resolver, F-1338, aggregated to a /64 network
// prefix for IPv6 per SEC-15 — see its doc) rather than [RemoteIPFrom]
// so the key is populated even when the Logger middleware that
// caches remote_ip in context hasn't run. Falls back to "anon" only
// when no IP can be resolved at all — collapsing such requests into a
// single shared bucket is the safe (fail-closed) choice for a
// throttle.
func anonymousRateLimitKey(r *http.Request) string {
	ip := remoteIPPrefixFor(r)
	if ip == "" {
		return "anon"
	}
	return "anon:" + ip
}

func authenticatedRateLimitKey(subject auth.Subject) string {
	if subject.KeyID != "" {
		return "auth:" + subject.Tier.String() + ":key:" + subject.KeyID
	}
	return "auth:" + subject.Tier.String() + ":id:" + subject.Identifier
}

// SkipHealthAndMetrics is a convenience Skip predicate for operators
// who don't want liveness probes or prometheus scrapes counted.
//
// Keep in lockstep with isUnauthenticatedInfraPath (auth.go): a probe the
// LB may send unauthenticated must not spend the anonymous per-IP bucket
// either, or a monitor behind a shared NAT flaps the region on 429.
func SkipHealthAndMetrics(r *http.Request) bool {
	switch r.URL.Path {
	case "/v1/healthz", "/v1/readyz", "/v1/livez/lake", "/v1/version", "/metrics", "/robots.txt", "/":
		return true
	}
	return strings.HasPrefix(r.URL.Path, "/errors/")
}

// rlProblem is a minimised RFC 9457 body duplicated here so the
// middleware package doesn't import internal/api/v1 (which would
// create a cycle — v1 imports middleware).
type rlProblem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance"`
}

// writeThrottleUnavailableProblem is the 503 + Retry-After response
// for the F-0050 / F-0150 dwell-time fail-closed branch. Mirrors
// writeRateLimitProblem's shape but carries a different error-type
// URL + status so operators reading access logs (and clients
// parsing the type tag) can tell "you were rate-limited" apart
// from "the rate limiter itself is offline."
//
// Retry-After is set BEFORE WriteHeader; net/http silently drops
// headers added after the status line is committed.
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
	// 30s matches ratelimit.DefaultDwellTime; clients that obey
	// Retry-After will naturally space retries far enough apart to
	// ride out a typical Redis fail-over.
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
	// Override the cache-control middleware's per-route directive:
	// never cache a 429. A 429 says "this caller is over budget right
	// now" — caching it would replay the denial to other anonymous
	// clients on the same CDN key well past their own budget reset.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(p)
}
