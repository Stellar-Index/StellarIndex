package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// AuthMode is the operator-configured authentication policy. Maps
// 1:1 to [config.APIConfig].AuthMode.
type AuthMode string

// HeaderAPIKey is the alternative to `Authorization: Bearer` for
// presenting an API key.
const HeaderAPIKey = "X-API-Key"

const (
	// AuthModeNone — no enforcement. The middleware attaches an
	// anonymous Subject to every request (keyed by RemoteIP+UA so
	// downstream rate-limit middleware buckets per client). Default.
	AuthModeNone AuthMode = "none"

	// AuthModeAPIKey — caller MUST present `Authorization: Bearer
	// <key>` or `X-API-Key: <key>`. Missing/invalid → 401.
	AuthModeAPIKey AuthMode = "apikey"

	// AuthModeAPIKeyOptional lets a caller present a key: without one the request is anonymous
	// (anonymous-tier limit), with a valid key it gets the per-key tier, and an invalid key is 401.
	// Endpoints that require auth still reject anonymous callers via their own Tier check.
	AuthModeAPIKeyOptional AuthMode = "apikey_optional"

	// AuthModeSEP10 — caller MUST present `Authorization: Bearer
	// <jwt>` issued by the SEP-10 verify exchange. Missing/invalid → 401.
	AuthModeSEP10 AuthMode = "sep10"
)

// AuthOptions configures the [Auth] middleware. Mode picks which
// validator runs; the validators themselves are interfaces so the
// middleware doesn't depend on the storage layer.
type AuthOptions struct {
	Mode AuthMode

	// APIKey validator. Required when Mode == AuthModeAPIKey.
	// Ignored otherwise.
	APIKey auth.APIKeyValidator

	// SEP10 validator. Required when Mode == AuthModeSEP10.
	// Ignored otherwise.
	SEP10 auth.SEP10Validator

	// FailedAuthLimiter, when non-nil, throttles INVALID-credential attempts per client IP and per
	// API-key prefix. Auth runs before RateLimit, so without it key-guessing would go unthrottled.
	// Valid and anonymous requests never touch it.
	FailedAuthLimiter *ratelimit.Bucket
}

// Auth returns a middleware that enforces the configured AuthMode.
//
// Stack position. Wire BETWEEN CORS and RateLimit:
//
//	stack := []Middleware{
//	    RequestID, HTTPMetrics, Logger, Recoverer, SecurityHeaders,
//	    CORS,             // CORS preflight short-circuits before auth
//	    Auth(opts),       // ← here
//	    RateLimit(...),   // sees the Subject in context for tier-based limits
//	}
//
// Behaviour by mode:
//
//   - none: attach anonymous Subject keyed by remote-IP+UA hash; pass.
//   - apikey: key from Authorization Bearer or X-API-Key, APIKey.Lookup;
//     errors map to 401/503.
//   - sep10: JWT from Authorization Bearer, SEP10.VerifyJWT; same mapping.
//
// Errors are bare text/plain 401 / 503; route-specific problem+json is the handler layer's job.
func Auth(opts AuthOptions) Middleware {
	mode := opts.Mode
	if mode == "" {
		mode = AuthModeNone
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isUnauthenticatedInfraPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			subject, err := authenticate(r, mode, opts)
			if err != nil {
				rejectAuth(w, r, mode, opts, err) //nolint:contextcheck // see takeFailedAuth's doc
				return
			}
			r = r.WithContext(auth.WithSubject(r.Context(), subject))
			next.ServeHTTP(w, r)
		})
	}
}

// rejectAuth writes the response for a failed authenticate call, throttling a CREDENTIAL FAILURE so
// a bad key/token cannot be retried without bound. Server-misconfig 503s do not count.
func rejectAuth(w http.ResponseWriter, r *http.Request, mode AuthMode, opts AuthOptions, err error) {
	rejected := isCredentialRejection(err)
	if opts.FailedAuthLimiter != nil && rejected {
		if throttled, retryAfter := takeFailedAuth(r, mode, opts.FailedAuthLimiter); throttled { //nolint:contextcheck // see its doc
			obs.FailedAuthTotal.WithLabelValues(obs.FailedAuthThrottled).Inc()
			writeAuthThrottleProblem(w, retryAfter)
			return
		}
	}
	if rejected {
		obs.FailedAuthTotal.WithLabelValues(obs.FailedAuthRejected).Inc()
	}
	writeAuthError(w, err)
}

// isUnauthenticatedInfraPath reports whether a path must answer WITHOUT credentials whatever
// `auth_mode` is, or an apikey cutover would 401 health probes and the scrape. /metrics is listed
// because loopbackOnly() is its real gate. Matching is exact so an exemption cannot silently
// widen; the one prefix, /errors/, fronts a static docs handler.
func isUnauthenticatedInfraPath(path string) bool {
	switch path {
	case "/v1/healthz", "/v1/readyz", "/v1/livez/lake", "/v1/version", "/metrics", "/robots.txt", "/":
		return true
	}
	// /errors/{slug} and /errors/ — the RFC 9457 problem-type documentation
	// every error body links to.
	return strings.HasPrefix(path, "/errors/")
}

// PublicRoutes is the set of route patterns that are unauthenticated by design: credential
// bootstraps and public status surfaces. Mounting through [PublicRoutes.Handle] exempts a route
// from [Auth] in the same call, so "mounted public" and "exempt from auth" are one fact.
//
// Unlike [isUnauthenticatedInfraPath], a public route is credential-OPTIONAL: a presented key or
// JWT is still verified (a bad one rejected), so signup can refuse already-authenticated callers.
type PublicRoutes struct {
	match    *http.ServeMux
	patterns []string
}

// NewPublicRoutes returns an empty public-route set.
func NewPublicRoutes() *PublicRoutes {
	return &PublicRoutes{match: http.NewServeMux()}
}

// Handle mounts h on mux under pattern and records pattern as public.
func (p *PublicRoutes) Handle(mux *http.ServeMux, pattern string, h http.Handler) {
	mux.Handle(pattern, h)
	// A second mux reuses ServeMux's matching, so the exemption covers exactly what the route serves.
	p.match.Handle(pattern, h)
	p.patterns = append(p.patterns, pattern)
}

// Patterns returns the recorded public patterns in registration order.
func (p *PublicRoutes) Patterns() []string {
	return append([]string(nil), p.patterns...)
}

// Mark returns a middleware that flags a request whose method and path
// match a public pattern, so [Auth] admits it without a credential. Wire
// it immediately outside Auth.
func (p *PublicRoutes) Mark() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, pattern := p.match.Handler(r); pattern != "" {
				r = r.WithContext(context.WithValue(r.Context(), publicRouteKey{}, true))
			}
			next.ServeHTTP(w, r)
		})
	}
}

type publicRouteKey struct{}

// isPublicRoute reports whether [PublicRoutes.Mark] flagged r.
func isPublicRoute(r *http.Request) bool {
	v, _ := r.Context().Value(publicRouteKey{}).(bool)
	return v
}

// isCredentialRejection reports whether err is a caller-supplied
// bad-credential outcome (as opposed to a server-side misconfiguration).
// Only these count against the failed-auth budgets: a 503 "validator not wired" is the
// operator's fault, and throttling on it would let a misconfig masquerade as abuse.
func isCredentialRejection(err error) bool {
	switch {
	case errors.Is(err, auth.ErrUnauthorized),
		errors.Is(err, auth.ErrForbidden),
		errors.Is(err, auth.ErrTokenExpired),
		errors.Is(err, auth.ErrTokenMalformed):
		return true
	default:
		return false
	}
}

// takeFailedAuth consumes one failed-auth token per dimension and reports whether the caller is
// over budget, with the Retry-After seconds. The client IP ("failauth:", /64 for IPv6) bounds one
// source across every key; the key prefix ("failauth-key:") bounds guessing at ONE key from many
// IPs. Only failures reach here, so filling a victim's key-prefix bucket can 429 other bad guesses
// but never the victim's valid key.
func takeFailedAuth(r *http.Request, mode AuthMode, limiter *ratelimit.Bucket) (throttled bool, retryAfter int) {
	ip := remoteIPPrefixFor(r)
	if ip == "" {
		// No resolvable IP: share one bucket rather than skip the throttle.
		ip = "unknown"
	}
	// Detached from request cancellation (see [throttleContext]): the bucket cannot tell a client
	// abort from a Redis outage, so an RST mid-take must not arm the fail-closed dwell clock.
	takeCtx, takeCancel := throttleContext(r)
	defer takeCancel()
	if throttled, ra := takeFailedAuthBucket(takeCtx, limiter, "failauth:"+ip); throttled {
		return true, ra
	}
	if prefix := failedAuthKeyPrefix(r, mode); prefix != "" {
		return takeFailedAuthBucket(takeCtx, limiter, "failauth-key:"+prefix)
	}
	return false, 0
}

// failedAuthKeyPrefix returns the display prefix ([auth.KeyPrefix]) of
// the API key r presents, or "" when there is none to key on. Only the API-key modes qualify:
// a SEP-10 JWT's leading bytes are a constant header, so keying on them would pool every JWT
// failure into one bucket.
func failedAuthKeyPrefix(r *http.Request, mode AuthMode) string {
	if mode != AuthModeAPIKey && mode != AuthModeAPIKeyOptional {
		return ""
	}
	return auth.KeyPrefix(bearerOrXKey(r))
}

// takeFailedAuthBucket takes one token from limiter under key.
//
// Fails OPEN on a transient backend error so a Redis blip does not 429 every failed login, but
// CLOSED once [ratelimit.ErrThrottleUnavailable] reports an outage past the dwell time, or a
// sustained outage would silently disable brute-force protection.
func takeFailedAuthBucket(ctx context.Context, limiter *ratelimit.Bucket, key string) (throttled bool, retryAfter int) {
	res, err := limiter.Take(ctx, key)
	if err != nil {
		if errors.Is(err, ratelimit.ErrThrottleUnavailable) {
			// Sustained outage: fail CLOSED. Retry-After matches [ratelimit.DefaultDwellTime] so
			// obedient clients space retries past a typical Redis fail-over.
			obs.RateLimitFailClosedTotal.WithLabelValues(obs.RateLimiterFailedAuth).Inc()
			return true, int(ratelimit.DefaultDwellTime.Seconds())
		}
		return false, 0
	}
	if res.Allowed {
		return false, 0
	}
	ra := int(res.RetryAfter.Seconds())
	if ra < 1 {
		ra = 1
	}
	return true, ra
}

// writeAuthThrottleProblem is the 429 for an exceeded failed-auth budget. Its problem type differs
// from the ordinary rate-limit 429 so logs separate credential-stuffing defence from throttling.
func writeAuthThrottleProblem(w http.ResponseWriter, retryAfter int) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(authProblem{
		Type:   "https://api.stellarindex.io/errors/too-many-failed-auth",
		Title:  "Too many failed authentication attempts",
		Status: http.StatusTooManyRequests,
		Detail: "too many invalid-credential attempts from this client; retry after " + strconv.Itoa(retryAfter) + "s",
	})
}

// authenticate runs the per-mode credential check + returns the
// resulting Subject (or an error). Pure dispatch; the heavy lifting
// is in the validator implementations.
func authenticate(r *http.Request, mode AuthMode, opts AuthOptions) (auth.Subject, error) {
	switch mode {
	case AuthModeNone:
		return auth.Anonymous(anonymousIdentifier(r)), nil

	case AuthModeAPIKey:
		key := bearerOrXKey(r)
		if key == "" {
			return missingCredential(r)
		}
		if opts.APIKey == nil {
			// Misconfiguration: fail loud rather than silently demote to anonymous, the wrong
			// default for a deployment that enabled apikey.
			return auth.Subject{}, auth.ErrNotImplemented
		}
		return opts.APIKey.Lookup(r.Context(), key)

	case AuthModeAPIKeyOptional:
		key := bearerOrXKey(r)
		if key == "" {
			// No key: anonymous. Auth-requiring endpoints gate on the subject's Tier.
			return auth.Anonymous(anonymousIdentifier(r)), nil
		}
		if opts.APIKey == nil {
			return auth.Subject{}, auth.ErrNotImplemented
		}
		// A supplied key must be valid: 401 beats silent anonymous downgrade because the caller
		// is asserting credentials.
		return opts.APIKey.Lookup(r.Context(), key)

	case AuthModeSEP10:
		jwt := bearerOnly(r)
		if jwt == "" {
			return missingCredential(r)
		}
		if opts.SEP10 == nil {
			return auth.Subject{}, auth.ErrNotImplemented
		}
		return opts.SEP10.VerifyJWT(r.Context(), jwt)
	}

	// Unknown mode: fail loud rather than treat as none. Config validation rejects it at startup.
	return auth.Subject{}, auth.ErrNotImplemented
}

// missingCredential answers a credential-required mode's empty-credential
// case: anonymous on a public route, 401 everywhere else.
func missingCredential(r *http.Request) (auth.Subject, error) {
	if isPublicRoute(r) {
		return auth.Anonymous(anonymousIdentifier(r)), nil
	}
	return auth.Subject{}, auth.ErrUnauthorized
}

// writeAuthError translates a sentinel auth error to an RFC 9457
// problem+json response, the same wire shape as handlers and RateLimit so clients have one
// error-decoding path. WWW-Authenticate is still set on 401 per RFC 6750 section 3.
func writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrTokenExpired):
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="token expired"`)
		writeAuthProblem(w, http.StatusUnauthorized,
			"https://api.stellarindex.io/errors/token-expired",
			"Token expired",
			"Your authentication token has expired; refresh and retry.")
	case errors.Is(err, auth.ErrTokenMalformed):
		writeAuthProblem(w, http.StatusBadRequest,
			"https://api.stellarindex.io/errors/malformed-credential",
			"Malformed credential",
			"The supplied credential could not be parsed.")
	case errors.Is(err, auth.ErrForbidden):
		writeAuthProblem(w, http.StatusForbidden,
			"https://api.stellarindex.io/errors/forbidden",
			"Forbidden",
			"The authenticated subject is not permitted to access this resource.")
	case errors.Is(err, auth.ErrNotImplemented):
		// An auth mode enabled with no validator wired: 503 naming the problem so an operator
		// sees it on the first failed request.
		writeAuthProblem(w, http.StatusServiceUnavailable,
			"https://api.stellarindex.io/errors/auth-not-configured",
			"Auth validator not configured",
			"This deployment enabled an auth mode but no validator was wired into the binary.")
	case errors.Is(err, auth.ErrAccountStatusUnavailable):
		// The account-status read is degraded with no last-known status: a server outage, NOT a
		// bad credential. Answer 503 + Retry-After so clients retry instead of rotating keys;
		// 30s mirrors writeThrottleUnavailableProblem.
		w.Header().Set("Retry-After", "30")
		writeAuthProblem(w, http.StatusServiceUnavailable,
			"https://api.stellarindex.io/errors/auth-status-unavailable",
			"Account status check unavailable",
			"The account-status service is temporarily unavailable; retry shortly.")
	default:
		// ErrUnauthorized + everything else fall here.
		w.Header().Set("WWW-Authenticate", `Bearer realm="stellarindex"`)
		writeAuthProblem(w, http.StatusUnauthorized,
			"https://api.stellarindex.io/errors/unauthorized",
			"Unauthorized",
			"Authentication is required to access this resource.")
	}
}

// authProblem is a minimised RFC 9457 body, duplicated from the envelope's Problem type because
// importing internal/api/v1 would create a cycle.
type authProblem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func writeAuthProblem(w http.ResponseWriter, status int, typeURL, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	// Override the route's CacheControl: a 401/403 on a publicly-cacheable route would otherwise
	// inherit `public` and a shared cache could store the denial under the success key.
	w.Header().Set("Cache-Control", "no-store")
	// RFC 7235 section 3.1: every 401 must advertise a challenge; the cookie path has none.
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="stellarindex.io"`)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(authProblem{
		Type:   typeURL,
		Title:  title,
		Status: status,
		Detail: detail,
	})
}

// bearerOrXKey extracts the API key from either of:
//
//	Authorization: Bearer <key>
//	X-API-Key: <key>
//
// Authorization wins when both are present (closer to the standard
// HTTP idiom). Returns "" if neither header is set.
func bearerOrXKey(r *http.Request) string {
	if k := bearerOnly(r); k != "" {
		return k
	}
	return strings.TrimSpace(r.Header.Get(HeaderAPIKey))
}

// bearerOnly extracts the token from `Authorization: Bearer <token>`, or "" when the header is
// missing or not Bearer. The scheme is case-insensitive (RFC 7235 §2.1).
func bearerOnly(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// anonymousIdentifier builds a stable SHA-256(remoteIP|userAgent) label for the anonymous
// [auth.Subject], for log/metric correlation only; the port is excluded because it rotates.
//
// SECURITY: the rate-limit bucket MUST NOT be keyed on this value. The client-controlled
// User-Agent would let a caller mint a fresh bucket per request; that key comes from the resolved
// client IP alone, in [bucketKeyAndOverrideForRequest] / anonymousRateLimitKey.
func anonymousIdentifier(r *http.Request) string {
	ip := remoteIPFor(r)
	ua := r.Header.Get("User-Agent")
	h := sha256.Sum256([]byte(ip + "|" + ua))
	return "anon-" + hex.EncodeToString(h[:8]) // 64-bit prefix is plenty for bucketing
}
