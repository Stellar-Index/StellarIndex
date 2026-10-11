package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// CORSOptions configures [CORS]. Pass a zero-valued struct for the
// conservative default (no origins allowed — same-origin only).
type CORSOptions struct {
	// AllowedOrigins is the exact-match allow-list of `Origin:` values. `*` allows any origin
	// but browsers reject it with credentials, so the constructor panics on `*` plus
	// AllowCredentials. Empty means no cross-origin access.
	AllowedOrigins []string

	// AllowCredentials emits `Access-Control-Allow-Credentials: true` to CredentialedOrigins
	// only, never to the rest of AllowedOrigins. Needed for cookie-authenticated cross-origin
	// fetches (the magic-link session); off by default as a per-deployment policy decision.
	AllowCredentials bool

	// CredentialedOrigins is the subset of AllowedOrigins trusted for credentialed access: they
	// receive Allow-Credentials, and [RequireSameSiteWrite] accepts a plain Origin match
	// against it. AllowedOrigins is a public READ list, so a read-only docs subdomain must not
	// be trusted to drive a cookie-authenticated write. Ignored when AllowCredentials is false;
	// the constructor panics if it is not a subset of AllowedOrigins.
	CredentialedOrigins []string

	// AllowedMethods defaults to {GET, HEAD, OPTIONS, POST} to match the v1 surface; set it
	// explicitly for a stricter cross-origin posture.
	AllowedMethods []string

	// AllowedHeaders lists the non-safelisted request headers; defaults to [DefaultCORSAllowedHeaders].
	AllowedHeaders []string

	// ExposedHeaders lists the response headers scripts may read beyond the Fetch safelist;
	// defaults to [DefaultCORSExposedHeaders].
	ExposedHeaders []string

	// MaxAge is the preflight cache duration in seconds. Defaults to 600: long enough to amortise
	// preflights, short enough that a policy change lands quickly. Browsers cap it at 2h.
	MaxAge int
}

// DefaultCORSAllowedHeaders are the request headers the OpenAPI spec or auth middleware tell a
// caller to send; cors_test.go diffs it against the spec.
var DefaultCORSAllowedHeaders = []string{
	"Content-Type",
	"Authorization",
	HeaderAPIKey,
	HeaderRequestID,
	IdempotencyKeyHeader,
	"X-Reason",
	"Last-Event-ID",
}

// DefaultCORSExposedHeaders are the response headers a client must act on (429 back-off, quota,
// request id). Without Access-Control-Expose-Headers a browser hides them from cross-origin scripts.
var DefaultCORSExposedHeaders = []string{
	"Retry-After",
	"X-RateLimit-Limit",
	"X-RateLimit-Remaining",
	"X-RateLimit-Reset",
	HeaderRequestID,
	"X-StellarIndex-Monthly-Quota",
	"X-StellarIndex-Monthly-Used",
	"X-StellarIndex-Signup-Verify-Required",
	"Idempotency-Replayed",
	// The account export's file name (GET /v1/dashboard/account/export).
	"Content-Disposition",
	// A text/csv list page's next page and envelope fields.
	"Link",
	"X-StellarIndex-Flags",
	"X-StellarIndex-Through-Ledger",
	"X-StellarIndex-Coverage-Note",
	"X-StellarIndex-Coverage-From",
	"X-StellarIndex-Holder-Count",
	"X-StellarIndex-As-Of-Ledger",
}

// CORS returns middleware that applies W3C CORS headers based on opts. Deliberately
// conservative: origins match exactly or by wildcard, with no pattern matching or per-route
// origins.
//
//   - A preflight (OPTIONS with an Origin) gets the preflight headers and a 204 without
//     calling next.
//   - Other requests get Allow-Origin and Expose-Headers iff the Origin is allowed, then next.
//   - With "*" the wildcard is echoed rather than reflecting the specific origin.
func CORS(opts CORSOptions) Middleware { //nolint:gocognit // origin allow-list + preflight branch + Vary handling are all part of one cohesive CORS contract; splitting would scatter the policy
	allowed := buildOriginSet(opts.AllowedOrigins)
	wildcard := allowed["*"]
	if wildcard && opts.AllowCredentials {
		// Browser rejects this combo at the parser. Fail at boot
		// rather than ship a CORS policy no browser will honour.
		panic("middleware.CORS: AllowedOrigins=[\"*\"] is incompatible with AllowCredentials=true")
	}
	credentialed := buildOriginSet(opts.CredentialedOrigins)
	for origin := range credentialed {
		if !allowed[origin] {
			// A credentialed origin that isn't even CORS-readable is
			// certainly a misconfiguration, not a narrower policy.
			panic("middleware.CORS: CredentialedOrigins contains an origin not in AllowedOrigins: " + origin)
		}
	}
	methods := strings.Join(defaultIfEmpty(opts.AllowedMethods,
		[]string{"GET", "HEAD", "OPTIONS", "POST"}), ", ")
	headers := strings.Join(defaultIfEmpty(opts.AllowedHeaders, DefaultCORSAllowedHeaders), ", ")
	exposed := strings.Join(defaultIfEmpty(opts.ExposedHeaders, DefaultCORSExposedHeaders), ", ")
	maxAge := opts.MaxAge
	if maxAge <= 0 {
		maxAge = 600
	}
	maxAgeStr := strconv.Itoa(maxAge)
	allowCredentials := opts.AllowCredentials
	// The write-guard policy is scoped to CredentialedOrigins, NOT the
	// full read allow-list. See [OriginPolicy.AllowsCredentialed].
	policy := &OriginPolicy{allowed: credentialed, credentials: allowCredentials}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			originAllowed := wildcard || (origin != "" && allowed[origin])

			// Vary: Origin on every response (except wildcard mode, where Allow-Origin is
			// origin-independent). Otherwise a CDN could replay a cached no-CORS response to a
			// later allowed browser origin, or one origin's Allow-Origin to another.
			if !wildcard {
				w.Header().Add("Vary", "Origin")
			}

			if originAllowed {
				if wildcard {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				} else {
					w.Header().Set("Access-Control-Allow-Origin", origin)
				}
				if allowCredentials && credentialed[origin] {
					w.Header().Set("Access-Control-Allow-Credentials", "true")
				}
				w.Header().Set("Access-Control-Expose-Headers", exposed)
			}

			// One increment per request, so operators can alert when a wildcard policy starts
			// handling real cross-origin traffic.
			switch {
			case origin == "":
				obs.APICORSDecisionsTotal.WithLabelValues("no_origin").Inc()
			case originAllowed && wildcard:
				obs.APICORSDecisionsTotal.WithLabelValues("allowed_wildcard").Inc()
			case originAllowed:
				obs.APICORSDecisionsTotal.WithLabelValues("allowed_origin").Inc()
			default:
				obs.APICORSDecisionsTotal.WithLabelValues("denied").Inc()
			}

			// Publish CredentialedOrigins (deliberately not the wider read allow-list) so
			// [RequireSameSiteWrite] reuses it instead of keeping a drift-prone second copy.
			r = r.WithContext(withOriginPolicy(r.Context(), policy))

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				// Preflight. Emit the allow-methods/headers/max-age
				// response and don't forward to the mux.
				if originAllowed {
					w.Header().Set("Access-Control-Allow-Methods", methods)
					w.Header().Set("Access-Control-Allow-Headers", headers)
					w.Header().Set("Access-Control-Max-Age", maxAgeStr)
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func buildOriginSet(origins []string) map[string]bool {
	m := make(map[string]bool, len(origins))
	for _, o := range origins {
		m[o] = true
	}
	return m
}

func defaultIfEmpty(v, fallback []string) []string {
	if len(v) == 0 {
		return fallback
	}
	return v
}
