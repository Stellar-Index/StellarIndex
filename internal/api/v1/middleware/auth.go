package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
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
	// attempts PER CLIENT IP (C3-5). Auth deliberately runs before the
	// main rate-limit middleware so per-tier limits key off the
	// authenticated subject — but that means a rejected credential
	// (401/403/expired/malformed) never reaches the limiter, leaving
	// credential-stuffing / key-guessing unthrottled. This bucket closes
	// that gap: every credential FAILURE consumes a per-IP token, and
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
			subject, err := authenticate(r, mode, opts)
			if err != nil {
				// C3-5: throttle per-IP on a CREDENTIAL FAILURE so a bad
				// key/token can't be retried without bound. Server-misconfig
				// 503s (ErrNotImplemented) don't count against the caller.
				if opts.FailedAuthLimiter != nil && isCredentialRejection(err) {
					if throttled, retryAfter := takeFailedAuth(r, opts.FailedAuthLimiter); throttled {
						writeAuthThrottleProblem(w, retryAfter)
						return
					}
				}
				writeAuthError(w, err)
				return
			}
			r = r.WithContext(auth.WithSubject(r.Context(), subject))
			next.ServeHTTP(w, r)
		})
	}
}

// isCredentialRejection reports whether err is a caller-supplied
// bad-credential outcome (as opposed to a server-side misconfiguration).
// Only these count against the per-IP failed-auth budget — a 503
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

// takeFailedAuth consumes one token from the per-IP failed-auth bucket
// and reports whether the caller is now over budget (and the
// Retry-After seconds to advertise). Keyed on the resolved client IP
// (forge-resistant XFF, F-1338) under a "failauth:" prefix so it shares
// no key space with the main per-IP request limiter. Fails OPEN on a
// limiter/backend error — a Redis blip must not convert every failed
// login into a 429; the auth error itself still returns.
func takeFailedAuth(r *http.Request, limiter *ratelimit.Bucket) (throttled bool, retryAfter int) {
	ip := remoteIPFor(r)
	if ip == "" {
		// No resolvable IP → collapse into one shared bucket rather than
		// skip the throttle (fail-closed for the throttle itself).
		ip = "unknown"
	}
	res, err := limiter.Take(r.Context(), "failauth:"+ip)
	if err != nil {
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

// writeAuthThrottleProblem is the 429 returned when an IP exceeds its
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
			return auth.Subject{}, auth.ErrUnauthorized
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
			return auth.Subject{}, auth.ErrUnauthorized
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
// Empty string if the header is missing or doesn't start with
// "Bearer ". Trims surrounding whitespace from the token.
func bearerOnly(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
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
