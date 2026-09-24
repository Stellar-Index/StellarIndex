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

const (
	// AuthModeNone — no enforcement. The middleware attaches an
	// anonymous Subject to every request (keyed by RemoteIP+UA so
	// downstream rate-limit middleware buckets per client). Default.
	AuthModeNone AuthMode = "none"

	// AuthModeAPIKey — caller MUST present `Authorization: Bearer
	// <key>` or `X-API-Key: <key>`. Missing/invalid → 401.
	AuthModeAPIKey AuthMode = "apikey"

	// AuthModeAPIKeyOptional — caller MAY present a key. Without
	// one, the request is treated as anonymous (same as
	// AuthModeNone — anonymous Subject, anonymous-tier rate-limit).
	// With a valid key, the request is upgraded to apikey-tier
	// (validated Subject, per-key rate-limit). Invalid key → 401.
	//
	// This is the freemium-API shape: low rate-limit floor for
	// anyone hitting the public surface, higher ceiling for
	// signed-up customers. Endpoints that REQUIRE auth (e.g.
	// /v1/account/me) still 401 anonymous callers via their own
	// Tier check; this mode just doesn't make anonymous BLOCKED
	// at the middleware layer.
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

	// FailedAuthLimiter, when non-nil, throttles INVALID-credential
	// attempts per client IP and per presented API-key prefix (C3-5).
	// Auth deliberately runs before the
	// main rate-limit middleware so per-tier limits key off the
	// authenticated subject — but that means a rejected credential
	// (401/403/expired/malformed) never reaches the limiter, leaving
	// credential-stuffing / key-guessing unthrottled. This bucket closes
	// that gap: every credential FAILURE consumes a token on each
	// dimension (see takeFailedAuth), and
	// over the budget the middleware returns 429 instead of the auth
	// error. Successful auth and anonymous passes never touch it, so the
	// Auth-before-RateLimit ordering for VALID requests is preserved.
	// Nil disables the failed-auth throttle (e.g. auth_mode=none, which
	// never produces a credential failure anyway).
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
//   - apikey: extract key from Authorization Bearer or X-API-Key
//     header, call APIKey.Lookup. On success attach the returned
//     Subject; on error map to HTTP status (401/503).
//   - sep10: extract JWT from Authorization Bearer header, call
//     SEP10.VerifyJWT. Same error mapping.
//
// Errors are returned as bare-bones text/plain 401 / 503 — the
// problem+json wrapper happens upstream in the handler layer for
// route-specific errors. Auth is too generic to ship a problem URL
// per case.
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
				// C3-5: throttle per-IP and per-key-prefix on a CREDENTIAL
				// FAILURE so a bad key/token can't be retried without bound.
				// Server-misconfig 503s (ErrNotImplemented) don't count.
				rejected := isCredentialRejection(err)
				if opts.FailedAuthLimiter != nil && rejected {
					if throttled, retryAfter := takeFailedAuth(r, mode, opts.FailedAuthLimiter); throttled { //nolint:contextcheck // takeFailedAuth intentionally detaches via throttleContext(r) — see its doc (REL-06 F059/Q153)
						obs.FailedAuthTotal.WithLabelValues(obs.FailedAuthThrottled).Inc()
						writeAuthThrottleProblem(w, retryAfter)
						return
					}
				}
				if rejected {
					obs.FailedAuthTotal.WithLabelValues(obs.FailedAuthRejected).Inc()
				}
				writeAuthError(w, err)
				return
			}
			r = r.WithContext(auth.WithSubject(r.Context(), subject))
			next.ServeHTTP(w, r)
		})
	}
}

// isUnauthenticatedInfraPath reports whether a path is operational
// plumbing that must answer WITHOUT credentials, whatever `auth_mode` is.
//
// Auth() wraps the entire mux, and every one of these routes is registered
// on that same mux — so before this, flipping auth_mode from `none` to
// `apikey` made liveness probes, readiness probes, the Prometheus scrape,
// robots.txt and the public error-documentation pages all return 401.
//
// That is not a hypothetical configuration. docs/operations/
// launch-day-checklist.md documents the production cutover as exactly that
// flip (`--extra-vars 'auth_mode=apikey'`), so the failure fires at the
// moment of going live: load-balancer health checks start failing, the
// orchestrator concludes the API is unhealthy, and monitoring goes blind at
// precisely the point an operator most needs it. Audit SEC-01.
//
// /metrics is included deliberately. It is already protected by
// loopbackOnly(), which is the correct control for a scrape endpoint — but
// that guard was MOOT under auth, because Auth 401'd the request before it
// ever reached the handler. Even a local Prometheus on 127.0.0.1 was blocked.
// Exempting it here restores loopbackOnly as the actual gate rather than
// stacking a second one that breaks the legitimate caller.
//
// Nothing here reads user data or accepts input: healthz/readyz report
// process liveness, /metrics is loopback-only, and robots.txt and /errors/*
// are static public documentation the RFC 9457 `type` URIs point at — a 401
// on those makes every error response's own documentation link unreachable.
//
// /v1/livez/lake is the lake-critical LB probe (ADR-0050): the OpenAPI
// contract declares it `security: []` alongside healthz/readyz, and it reads
// no caller input, so it belongs here too. It was added (#119) after this
// list was written and missed it — under `apikey`/`sep10` mode every
// uncredentialed probe 401'd, which is exactly the SEC-01 class this list
// exists to prevent (api-security-3, audit 2026-08-28).
//
// Deliberately an exact-match list, not a prefix match: a prefix rule ("/v1/health…")
// is how an exemption silently widens to cover a route added next to it later.
// The one prefix here, /errors/, is scoped to a static docs handler.
func isUnauthenticatedInfraPath(path string) bool {
	switch path {
	case "/v1/healthz", "/v1/readyz", "/v1/livez/lake", "/v1/version", "/metrics", "/robots.txt", "/":
		return true
	}
	// /errors/{slug} and /errors/ — the RFC 9457 problem-type documentation
	// every error body links to.
	return strings.HasPrefix(path, "/errors/")
}

// PublicRoutes is the set of route patterns that are unauthenticated by
// design: credential bootstraps (signup, register, SEP-10, dashboard login)
// and public status surfaces. Mounting a route through [PublicRoutes.Handle]
// exempts it from [Auth]'s credential requirement in the same call, so
// "mounted public" and "exempt from auth" are one fact — a hand-kept path
// list cannot notice the next bootstrap route.
//
// Unlike [isUnauthenticatedInfraPath], a public route is credential-OPTIONAL,
// not credential-blind: a presented key or JWT is still verified (and a bad
// one rejected), so signup can keep refusing already-authenticated callers.
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
	// A second mux reuses ServeMux's own method/wildcard/precedence
	// matching, so the exemption covers exactly what the route serves.
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
// Only these count against the failed-auth budgets — a 503
// "validator not wired" is the operator's fault, not an attacker's, and
// throttling on it would let a boot-time misconfig masquerade as abuse.
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

// takeFailedAuth consumes one failed-auth token on each dimension that
// applies to r and reports whether the caller is now over budget (and
// the Retry-After seconds to advertise). The dimensions are:
//
//   - the resolved client IP ("failauth:"; forge-resistant XFF, F-1338;
//     a /64 network prefix for IPv6 per SEC-15 — see [remoteIPPrefixFor]),
//     which bounds one source's guessing across every key;
//   - the presented API key's display prefix ("failauth-key:"; see
//     [failedAuthKeyPrefix]), which bounds guessing aimed at ONE key
//     from many IPs — rotating addresses resets only the IP dimension.
//
// Only credential FAILURES reach here, so a caller filling a victim's
// key-prefix bucket can 429 other bad guesses at that prefix but never
// the victim's valid key. Both key spaces are disjoint from the main
// per-IP request limiter's.
func takeFailedAuth(r *http.Request, mode AuthMode, limiter *ratelimit.Bucket) (throttled bool, retryAfter int) {
	ip := remoteIPPrefixFor(r)
	if ip == "" {
		// No resolvable IP → collapse into one shared bucket rather than
		// skip the throttle (fail-closed for the throttle itself).
		ip = "unknown"
	}
	// Detach from the request's cancellation — see [throttleContext]:
	// ratelimit.Bucket cannot tell a client abort from a Redis outage,
	// so a caller that RSTs mid-take must not arm the fail-closed dwell
	// clock for these shared buckets (REL-06 F059 / Q153).
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
// the API key r presents, or "" when there is none to key on. Only the
// API-key modes qualify: a SEP-10 JWT's leading bytes are its constant
// base64 header, so keying on them would pool every JWT failure
// server-wide into one bucket.
func failedAuthKeyPrefix(r *http.Request, mode AuthMode) string {
	if mode != AuthModeAPIKey && mode != AuthModeAPIKeyOptional {
		return ""
	}
	return auth.KeyPrefix(bearerOrXKey(r))
}

// takeFailedAuthBucket takes one token from limiter under key.
//
// Fails OPEN on a transient limiter/backend error — a brief Redis blip
// must not convert every failed login into a 429; the auth error itself
// still returns. But once [ratelimit.Bucket.Take] reports
// [ratelimit.ErrThrottleUnavailable] (sustained backend outage past the
// dwell-time), the credential-stuffing throttle must fail CLOSED like
// the main RateLimit middleware does (SEC-15 / C3-5): otherwise a
// sustained Redis outage silently disables brute-force protection for
// as long as it lasts, which is exactly when an attacker is most likely
// to be probing.
func takeFailedAuthBucket(ctx context.Context, limiter *ratelimit.Bucket, key string) (throttled bool, retryAfter int) {
	res, err := limiter.Take(ctx, key)
	if err != nil {
		if errors.Is(err, ratelimit.ErrThrottleUnavailable) {
			// Sustained outage: fail CLOSED, mirroring
			// writeThrottleUnavailableProblem's Retry-After — matches
			// [ratelimit.DefaultDwellTime] so clients that obey it
			// naturally space retries past a typical Redis fail-over.
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

// writeAuthThrottleProblem is the 429 returned when a caller exceeds a
// failed-auth budget (C3-5). Distinct problem type from the ordinary
// rate-limit 429 so operators reading access logs can tell
// credential-stuffing defence apart from ordinary request throttling.
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
			// Mis-configuration: mode says apikey but no validator
			// wired. Fail-loud rather than silently demoting to
			// anonymous (which would be the wrong default for a
			// deployment that intentionally enabled apikey).
			return auth.Subject{}, auth.ErrNotImplemented
		}
		return opts.APIKey.Lookup(r.Context(), key)

	case AuthModeAPIKeyOptional:
		key := bearerOrXKey(r)
		if key == "" {
			// No key → anonymous. Endpoints that require auth still
			// gate via subject.Tier check inside the handler.
			return auth.Anonymous(anonymousIdentifier(r)), nil
		}
		if opts.APIKey == nil {
			return auth.Subject{}, auth.ErrNotImplemented
		}
		// Key supplied → must be valid. Wrong-key 401 is
		// preferable to silent anonymous-downgrade because the
		// caller is asserting they have credentials.
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

	// Unknown mode — fail-loud rather than treat as none. Config
	// validation rejects unknown modes at startup so this branch
	// shouldn't fire in production.
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
// problem+json response. Matches docs/reference/api-design.md §11
// "every 4xx/5xx returns application/problem+json" — the auth
// middleware emits the same wire shape as handlers + the rate-limit
// middleware, so clients have a single error-decoding path.
//
// WWW-Authenticate is still set on 401 paths per RFC 6750 §3 so
// browser-side clients get the standard challenge.
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
		// Fail-loud on a deployment that enabled an auth mode but
		// didn't wire the validator. 503 + a body that names the
		// problem so an operator sees it on the first failed request.
		writeAuthProblem(w, http.StatusServiceUnavailable,
			"https://api.stellarindex.io/errors/auth-not-configured",
			"Auth validator not configured",
			"This deployment enabled an auth mode but no validator was wired into the binary.")
	case errors.Is(err, auth.ErrAccountStatusUnavailable):
		// auth-ks-1: the account-status kill-switch read (its Postgres
		// backend) is degraded and no last-known status is available to
		// ride the blip out. This is a server-side outage, NOT a bad
		// credential — answer 503 + Retry-After so clients retry rather
		// than treating it as "your key is invalid" and rotating keys.
		// Retry-After mirrors writeThrottleUnavailableProblem (30s ~
		// ratelimit.DefaultDwellTime).
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

// authProblem is a minimised RFC 9457 body. Duplicated from the
// envelope's Problem type so the middleware package doesn't import
// internal/api/v1 (which would create a cycle — v1 imports
// middleware). Matches the same pattern used by rlProblem in
// ratelimit.go.
type authProblem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func writeAuthProblem(w http.ResponseWriter, status int, typeURL, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	// Override the route directive the CacheControl middleware set
	// before auth ran. Without this a 401/403 on a publicly-cacheable
	// route (e.g. /v1/price) inherits `public, max-age, s-maxage` and
	// a shared cache may store the per-key denial against the same
	// key as the success response (see cachecontrol.go's invariant).
	w.Header().Set("Cache-Control", "no-store")
	// RFC 7235 §3.1: every 401 MUST advertise at least one
	// challenge so clients can discover the accepted auth scheme.
	// All authenticated v1 endpoints accept Bearer (API key +
	// SEP-10 token); the magic-link cookie path is parallel and
	// has no standard challenge token, so we advertise Bearer.
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
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

// bearerOnly extracts the token from `Authorization: Bearer <token>`.
// Empty string if the header is missing or its scheme is not Bearer.
// The scheme is matched case-insensitively (RFC 7235 §2.1). Trims
// surrounding whitespace from the token.
func bearerOnly(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// anonymousIdentifier builds a stable per-request identifier for
// anonymous callers, stored on the anonymous [auth.Subject]. Used as
// a log / metric correlation label — NOT as the rate-limit key.
// SHA-256(remoteIP + "|" + userAgent) — the hash keeps identifying
// details out of metrics labels (cardinality) while still
// distinguishing clients in logs.
//
// SECURITY (F-1335): the rate-limit bucket MUST NOT be keyed on this
// value. Because it folds in the (client-controlled) User-Agent, a
// caller could rotate its UA on every request to mint unlimited
// distinct identifiers — one bucket each — and bypass the per-IP
// anonymous throttle. The throttle key is derived separately from
// the resolved client IP alone in
// [bucketKeyAndOverrideForRequest] / anonymousRateLimitKey.
//
// We don't include port (RemoteAddr's :port slice) because that
// rotates on every connection; we want the same caller's requests
// to share an identifier.
func anonymousIdentifier(r *http.Request) string {
	ip := remoteIPFor(r)
	ua := r.Header.Get("User-Agent")
	h := sha256.Sum256([]byte(ip + "|" + ua))
	return "anon-" + hex.EncodeToString(h[:8]) // 64-bit prefix is plenty for bucketing
}
