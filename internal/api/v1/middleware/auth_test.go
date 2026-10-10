package middleware_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// stubAPIKeyValidator returns a fixed Subject for one specific
// known key; every other key triggers ErrUnauthorized. Lets us
// drive the apikey-mode middleware without needing a real Redis.
type stubAPIKeyValidator struct {
	knownKey string
	subject  auth.Subject
	err      error // set per-test to override the success path
}

func (s stubAPIKeyValidator) Lookup(_ context.Context, key string) (auth.Subject, error) {
	if s.err != nil {
		return auth.Subject{}, s.err
	}
	if key != s.knownKey {
		return auth.Subject{}, auth.ErrUnauthorized
	}
	return s.subject, nil
}

// stubSEP10Validator analogue for the JWT path. Only VerifyJWT is
// exercised by the middleware; the other methods land on the
// challenge/verify HTTP handlers (out of scope for this test).
type stubSEP10Validator struct {
	auth.NoopSEP10Validator // embed to inherit Challenge/Verify stubs

	knownJWT string
	subject  auth.Subject
}

func (s stubSEP10Validator) VerifyJWT(_ context.Context, jwt string) (auth.Subject, error) {
	if jwt != s.knownJWT {
		return auth.Subject{}, auth.ErrUnauthorized
	}
	return s.subject, nil
}

// captureSubject is the inner handler the middleware wraps; it
// records the Subject from context so tests can assert it.
func captureSubject(captured *auth.Subject) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, _ := auth.SubjectFrom(r.Context())
		*captured = s
		w.WriteHeader(http.StatusOK)
	}
}

// TestAuth_ModeNone is the default-on path: every request gets
// an anonymous Subject keyed by RemoteIP+UA hash. No 401s, no
// validator calls. The identifier is non-empty so the rate-limit
// middleware downstream has something to bucket against.
func TestAuth_ModeNone(t *testing.T) {
	var captured auth.Subject
	mw := middleware.Auth(middleware.AuthOptions{Mode: middleware.AuthModeNone})
	h := mw(captureSubject(&captured))

	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	r.RemoteAddr = "203.0.113.5:54321"
	r.Header.Set("User-Agent", "test-client/1.0")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if captured.Tier != auth.TierAnonymous {
		t.Errorf("tier = %q, want anonymous", captured.Tier)
	}
	if captured.Identifier == "" {
		t.Error("anonymous identifier is empty — rate-limit middleware needs a key")
	}
	// Identifier must be deterministic for the same (IP, UA): a
	// second request from the same caller hits the same bucket.
	r2 := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	r2.RemoteAddr = "203.0.113.5:54321"
	r2.Header.Set("User-Agent", "test-client/1.0")
	var captured2 auth.Subject
	h2 := mw(captureSubject(&captured2))
	h2.ServeHTTP(httptest.NewRecorder(), r2)
	if captured.Identifier != captured2.Identifier {
		t.Errorf("identifier non-deterministic: %q != %q", captured.Identifier, captured2.Identifier)
	}
}

func TestAuth_ModeNone_UntrustedPeerIgnoresXForwardedFor(t *testing.T) {
	var captured auth.Subject
	mw := middleware.Auth(middleware.AuthOptions{Mode: middleware.AuthModeNone})
	h := mw(captureSubject(&captured))

	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	r.RemoteAddr = "198.51.100.9:80"
	r.Header.Set("User-Agent", "test-client/1.0")
	r.Header.Set("X-Forwarded-For", "203.0.113.42")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var direct auth.Subject
	h2 := mw(captureSubject(&direct))
	r2 := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	r2.RemoteAddr = "198.51.100.9:80"
	r2.Header.Set("User-Agent", "test-client/1.0")
	h2.ServeHTTP(httptest.NewRecorder(), r2)

	if captured.Identifier != direct.Identifier {
		t.Fatalf("identifier changed based on untrusted XFF: %q != %q", captured.Identifier, direct.Identifier)
	}
}

func TestAuth_ModeNone_TrustedProxyUsesXForwardedFor(t *testing.T) {
	if err := middleware.SetTrustedProxyCIDRs([]string{"10.0.0.0/8"}); err != nil {
		t.Fatalf("SetTrustedProxyCIDRs: %v", err)
	}
	t.Cleanup(func() {
		if err := middleware.SetTrustedProxyCIDRs(nil); err != nil {
			t.Fatalf("reset trusted proxies: %v", err)
		}
	})

	var captured auth.Subject
	mw := middleware.Auth(middleware.AuthOptions{Mode: middleware.AuthModeNone})
	h := mw(captureSubject(&captured))

	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	r.RemoteAddr = "10.0.0.1:80"
	r.Header.Set("User-Agent", "test-client/1.0")
	r.Header.Set("X-Forwarded-For", "203.0.113.42, 10.0.0.1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var direct auth.Subject
	h2 := mw(captureSubject(&direct))
	r2 := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	r2.RemoteAddr = "203.0.113.42:80"
	r2.Header.Set("User-Agent", "test-client/1.0")
	h2.ServeHTTP(httptest.NewRecorder(), r2)

	if captured.Identifier != direct.Identifier {
		t.Fatalf("trusted XFF should resolve to client identity: %q != %q", captured.Identifier, direct.Identifier)
	}
}

// TestAuth_ModeAPIKey_HappyPath confirms a valid key passes auth
// + the validator's Subject reaches the handler.
func TestAuth_ModeAPIKey_HappyPath(t *testing.T) {
	want := auth.Subject{Identifier: "acct-42", Tier: auth.TierAPIKey}
	var captured auth.Subject
	mw := middleware.Auth(middleware.AuthOptions{
		Mode:   middleware.AuthModeAPIKey,
		APIKey: stubAPIKeyValidator{knownKey: "k1", subject: want},
	})
	h := mw(captureSubject(&captured))

	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	r.Header.Set("Authorization", "Bearer k1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if captured.Identifier != want.Identifier || captured.Tier != want.Tier {
		t.Errorf("captured = %+v, want %+v", captured, want)
	}
}

// TestAuth_ModeAPIKey_XAPIKeyHeader covers the alt header. Some
// SDKs / curl users prefer X-API-Key; we accept both. Authorization
// wins when both are present (tested in HappyPath above).
func TestAuth_ModeAPIKey_XAPIKeyHeader(t *testing.T) {
	want := auth.Subject{Identifier: "acct-42", Tier: auth.TierAPIKey}
	var captured auth.Subject
	mw := middleware.Auth(middleware.AuthOptions{
		Mode:   middleware.AuthModeAPIKey,
		APIKey: stubAPIKeyValidator{knownKey: "k1", subject: want},
	})
	h := mw(captureSubject(&captured))

	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	r.Header.Set("X-API-Key", "k1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if captured.Identifier != want.Identifier {
		t.Errorf("X-API-Key path didn't authenticate: %+v", captured)
	}
}

// TestAuth_ModeAPIKey_RejectsMissing: 401 with WWW-Authenticate
// hint when no credential is present + apikey mode is on.
func TestAuth_ModeAPIKey_RejectsMissing(t *testing.T) {
	mw := middleware.Auth(middleware.AuthOptions{
		Mode:   middleware.AuthModeAPIKey,
		APIKey: stubAPIKeyValidator{knownKey: "k1"},
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("inner handler should not run on missing-credential path")
	}))

	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !contains(w.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("WWW-Authenticate missing Bearer hint: %q", w.Header().Get("WWW-Authenticate"))
	}
}

// TestAuth_ModeAPIKey_RejectsInvalid: 401 when the key bytes don't
// match the validator. Confirms the validator's ErrUnauthorized
// reaches the response.
func TestAuth_ModeAPIKey_RejectsInvalid(t *testing.T) {
	mw := middleware.Auth(middleware.AuthOptions{
		Mode:   middleware.AuthModeAPIKey,
		APIKey: stubAPIKeyValidator{knownKey: "k1"},
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("inner handler ran with invalid key")
	}))

	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	r.Header.Set("Authorization", "Bearer wrong-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// TestAuth_ModeAPIKey_NotImplemented503: a deployment that enabled
// apikey mode but didn't wire a validator returns 503 — fail-loud.
// Operator sees the misconfiguration on the first request rather
// than discovering it from a security audit.
func TestAuth_ModeAPIKey_NotImplemented503(t *testing.T) {
	mw := middleware.Auth(middleware.AuthOptions{
		Mode:   middleware.AuthModeAPIKey,
		APIKey: nil, // misconfiguration
	})
	h := mw(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))

	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	r.Header.Set("Authorization", "Bearer anything")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestAuth_ModeSEP10_HappyPath confirms a valid JWT passes auth +
// the validator's Subject reaches the handler.
func TestAuth_ModeSEP10_HappyPath(t *testing.T) {
	want := auth.Subject{Identifier: "GAB123", Tier: auth.TierSEP10}
	var captured auth.Subject
	mw := middleware.Auth(middleware.AuthOptions{
		Mode:  middleware.AuthModeSEP10,
		SEP10: stubSEP10Validator{knownJWT: "good.jwt.string", subject: want},
	})
	h := mw(captureSubject(&captured))

	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	r.Header.Set("Authorization", "Bearer good.jwt.string")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if captured.Identifier != want.Identifier {
		t.Errorf("captured = %+v, want %+v", captured, want)
	}
}

// TestAuth_ModeSEP10_RejectsXAPIKey: SEP-10 mode does NOT accept
// X-API-Key (that's apikey-mode). A client mixing modes should
// get 401 cleanly rather than silently demoting.
func TestAuth_ModeSEP10_RejectsXAPIKey(t *testing.T) {
	mw := middleware.Auth(middleware.AuthOptions{
		Mode:  middleware.AuthModeSEP10,
		SEP10: stubSEP10Validator{knownJWT: "good"},
	})
	h := mw(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("inner handler ran on cross-mode credential")
	}))

	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	r.Header.Set("X-API-Key", "good") // wrong header for sep10 mode
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (X-API-Key not honoured under sep10)", w.Code)
	}
}

// TestAuth_AccountStatusUnavailableIs503Retryable pins the auth-ks-1
// fix at the HTTP boundary: when the validator reports the account-
// status kill-switch read is degraded (auth.ErrAccountStatusUnavailable),
// the middleware answers 503 + Retry-After, NOT the 401 the default
// branch would emit — so a transient Postgres blip reads as a
// retryable "auth layer degraded" rather than "your credential is
// invalid" (which drives clients to rotate keys during a server-side
// outage).
func TestAuth_AccountStatusUnavailableIs503Retryable(t *testing.T) {
	opts := middleware.AuthOptions{
		Mode: middleware.AuthModeAPIKey,
		APIKey: stubAPIKeyValidator{
			knownKey: "k1",
			// Wrapped like the validator wraps it, to prove the
			// middleware unwraps via errors.Is rather than ==.
			err: fmt.Errorf("auth: apikey account status %q: %w: %w",
				"acme", auth.ErrAccountStatusUnavailable, errors.New("postgres unreachable")),
		},
	}
	mw := middleware.Auth(opts)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // must never run on the error path
	}))

	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	r.Header.Set("Authorization", "Bearer k1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — a degraded account-status read is a server outage, not a bad credential", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
	var body struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Type != "https://api.stellarindex.io/errors/auth-status-unavailable" {
		t.Errorf("type = %q, want .../errors/auth-status-unavailable", body.Type)
	}
	if body.Status != http.StatusServiceUnavailable {
		t.Errorf("body.status = %d, want 503", body.Status)
	}
}

// TestAuth_ErrorBodyIsProblemJSON pins the wire shape every auth
// failure path emits — application/problem+json per
// docs/reference/api-design.md §11. Emitting
// text/plain bodies on these paths would break the "every 4xx/5xx is
// problem+json" client contract.
func TestAuth_ErrorBodyIsProblemJSON(t *testing.T) {
	cases := []struct {
		name       string
		opts       middleware.AuthOptions
		setHeader  func(*http.Request)
		wantStatus int
		wantType   string
	}{
		{
			name: "missing credential — 401 unauthorized",
			opts: middleware.AuthOptions{
				Mode:   middleware.AuthModeAPIKey,
				APIKey: stubAPIKeyValidator{knownKey: "k1"},
			},
			setHeader:  func(*http.Request) {},
			wantStatus: http.StatusUnauthorized,
			wantType:   "https://api.stellarindex.io/errors/unauthorized",
		},
		{
			name: "no validator wired — 503 not configured",
			opts: middleware.AuthOptions{
				Mode:   middleware.AuthModeAPIKey,
				APIKey: nil,
			},
			setHeader: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer x")
			},
			wantStatus: http.StatusServiceUnavailable,
			wantType:   "https://api.stellarindex.io/errors/auth-not-configured",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mw := middleware.Auth(tc.opts)
			h := mw(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))

			r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
			tc.setHeader(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if got := w.Header().Get("Content-Type"); got != "application/problem+json" {
				t.Errorf("Content-Type = %q, want application/problem+json", got)
			}
			var body struct {
				Type   string `json:"type"`
				Title  string `json:"title"`
				Status int    `json:"status"`
				Detail string `json:"detail"`
			}
			if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Type != tc.wantType {
				t.Errorf("type = %q, want %q", body.Type, tc.wantType)
			}
			if body.Status != tc.wantStatus {
				t.Errorf("body.status = %d, want %d", body.Status, tc.wantStatus)
			}
			if body.Title == "" {
				t.Error("title is empty; should be human-readable")
			}
			if body.Detail == "" {
				t.Error("detail is empty; should explain the failure")
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestAuth_InfraPathsBypassCredentials pins that operational
// plumbing must answer WITHOUT credentials whatever auth_mode is set to.
//
// Auth() wraps the whole mux and every infra route is registered on that same
// mux, so before this, flipping auth_mode from `none` to `apikey` made
// liveness probes, readiness probes, the Prometheus scrape, robots.txt and the
// error-documentation pages all return 401.
//
// That flip is the DOCUMENTED production cutover
// (docs/operations/launch-day-checklist.md: --extra-vars 'auth_mode=apikey'),
// so the failure fires at the moment of going live: load-balancer health checks
// start failing, the orchestrator concludes the API is unhealthy, and
// monitoring goes blind exactly when an operator most needs it.
//
// The validator here rejects EVERY key, so any request that reaches
// authenticate() 401s. An infra path returning 200 therefore proves the bypass
// ran, not that some credential happened to work.
//
// Proven red: without isUnauthenticatedInfraPath every infra subtest returns
// 401.
func TestAuth_InfraPathsBypassCredentials(t *testing.T) {
	mw := middleware.Auth(middleware.AuthOptions{
		Mode:   middleware.AuthModeAPIKey,
		APIKey: stubAPIKeyValidator{knownKey: "no-key-matches-this"},
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	infra := []string{
		"/v1/healthz", "/v1/readyz", "/v1/version",
		"/v1/livez/lake", // ADR-0050 lake-route LB probe; openapi declares security: [] (api-security-3)
		"/metrics",       // loopbackOnly() is the real gate; Auth 401'd before it ran
		"/robots.txt",    //nolint:misspell // filename
		"/",
		"/errors/not-found", // RFC 9457 type URIs point here from every error body
		"/errors/",
	}
	for _, path := range infra {
		t.Run("open "+path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, path, nil) // no credentials at all
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Errorf("GET %s with no credentials = %d, want 200 — infra plumbing must "+
					"answer regardless of auth_mode, or the documented production cutover "+
					"breaks health checks and monitoring (SEC-01)", path, w.Code)
			}
		})
	}

	// The exemption must NOT leak to real API surface.
	for _, path := range []string{
		"/v1/price", "/v1/assets", "/v1/accounts/GABC/movements",
		"/v1/healthz/../price", // no prefix-walking into the API
		"/errors",              // no trailing slash: NOT the docs handler, must stay protected
	} {
		t.Run("still protected "+path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code == http.StatusOK {
				t.Errorf("GET %s with no credentials = 200 — the infra exemption leaked "+
					"onto authenticated API surface", path)
			}
		})
	}
}

// TestAuth_PublicRouteIsCredentialOptional pins that a route mounted via
// PublicRoutes.Handle answers an uncredentialed caller under the
// credential-required modes, still verifies a presented credential, and the
// exemption does not widen to another method or a neighbouring path.
func TestAuth_PublicRouteIsCredentialOptional(t *testing.T) {
	modes := map[middleware.AuthMode]middleware.AuthOptions{
		middleware.AuthModeAPIKey: {Mode: middleware.AuthModeAPIKey, APIKey: stubAPIKeyValidator{knownKey: "sip_good"}},
		middleware.AuthModeSEP10:  {Mode: middleware.AuthModeSEP10, SEP10: stubSEP10Validator{knownJWT: "good.jwt"}},
	}
	for mode, opts := range modes {
		t.Run(string(mode), func(t *testing.T) {
			var captured auth.Subject
			mux := http.NewServeMux()
			pub := middleware.NewPublicRoutes()
			pub.Handle(mux, "POST /v1/signup", captureSubject(&captured))
			mux.Handle("GET /v1/signup", captureSubject(&captured))
			mux.Handle("POST /v1/signupx", captureSubject(&captured))
			h := middleware.Chain(mux, pub.Mark(), middleware.Auth(opts))

			serve := func(method, path, bearer string) int {
				r := httptest.NewRequest(method, path, nil)
				if bearer != "" {
					r.Header.Set("Authorization", "Bearer "+bearer)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w.Code
			}

			if code := serve(http.MethodPost, "/v1/signup", ""); code != http.StatusOK {
				t.Fatalf("public route, no credential: status = %d, want 200", code)
			}
			if captured.Tier != auth.TierAnonymous || captured.Identifier == "" {
				t.Errorf("public route subject = %+v, want an identified anonymous subject", captured)
			}
			if code := serve(http.MethodPost, "/v1/signup", "wrong"); code != http.StatusUnauthorized {
				t.Errorf("public route, bad credential: status = %d, want 401 (a presented credential is still verified)", code)
			}
			if code := serve(http.MethodGet, "/v1/signup", ""); code != http.StatusUnauthorized {
				t.Errorf("other method on a public path: status = %d, want 401", code)
			}
			if code := serve(http.MethodPost, "/v1/signupx", ""); code != http.StatusUnauthorized {
				t.Errorf("neighbouring path: status = %d, want 401", code)
			}
			if got := pub.Patterns(); len(got) != 1 || got[0] != "POST /v1/signup" {
				t.Errorf("Patterns() = %v, want [POST /v1/signup]", got)
			}
		})
	}
}

// TestAuth_BearerSchemeIsCaseInsensitive pins that RFC 7235 §2.1 makes
// the auth-scheme token case-insensitive, so `bearer <key>` must
// authenticate exactly like `Bearer <key>` under apikey and sep10.
func TestAuth_BearerSchemeIsCaseInsensitive(t *testing.T) {
	want := auth.Subject{Identifier: "k1", Tier: auth.TierAPIKey}
	modes := map[middleware.AuthMode]middleware.AuthOptions{
		middleware.AuthModeAPIKey: {Mode: middleware.AuthModeAPIKey, APIKey: stubAPIKeyValidator{knownKey: "sip_good", subject: want}},
		middleware.AuthModeSEP10:  {Mode: middleware.AuthModeSEP10, SEP10: stubSEP10Validator{knownJWT: "sip_good", subject: want}},
	}
	for mode, opts := range modes {
		for _, scheme := range []string{"bearer", "BEARER", "BeArEr"} {
			var captured auth.Subject
			h := middleware.Auth(opts)(captureSubject(&captured))
			r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
			r.Header.Set("Authorization", scheme+" sip_good")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK || captured.Identifier != want.Identifier {
				t.Errorf("%s: %q scheme: status %d subject %+v, want 200 as %q", mode, scheme, w.Code, captured, want.Identifier)
			}
		}
	}
}

// TestAuth_FailedAuthThrottle_FailsClosedOnSustainedOutage pins the
// fail-closed behaviour: once the failed-auth Bucket has been erroring for
// longer than its dwell-time (Take returns
// [ratelimit.ErrThrottleUnavailable]), the credential-stuffing throttle
// must fail CLOSED (still block the request) instead of silently
// disabling brute-force protection for the rest of the outage. On the
// unfixed code takeFailedAuth returns (false, 0) for ANY limiter error
// — including ErrThrottleUnavailable — so a bad key keeps returning a
// plain 401 forever during a sustained Redis outage.
func TestAuth_FailedAuthThrottle_FailsClosedOnSustainedOutage(t *testing.T) {
	if err := middleware.SetTrustedProxyCIDRs([]string{}); err != nil {
		t.Fatalf("SetTrustedProxyCIDRs: %v", err)
	}

	rdb, mr := newRLRedis(t)
	fakeNow := time.Unix(1_750_000_000, 0)
	limiter := ratelimit.New(rdb, 3, time.Minute,
		ratelimit.WithClock(func() time.Time { return fakeNow }),
		ratelimit.WithDwellTime(30*time.Second),
	)

	mw := middleware.Auth(middleware.AuthOptions{
		Mode:              middleware.AuthModeAPIKey,
		APIKey:            stubAPIKeyValidator{knownKey: "good-key"},
		FailedAuthLimiter: limiter,
	})
	h := mw(okHandler())

	badReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
		r.RemoteAddr = "203.0.113.55:44440"
		r.Header.Set("X-API-Key", "wrong-key")
		return r
	}

	// Blow up the backing miniredis — every future Take() call errors.
	mr.Kill()

	// First failure only ARMS the dwell-time clock (elapsed=0 < 30s):
	// the bucket still returns a plain wrapped error, so the throttle
	// stays fail-open for this one request — matches the main
	// RateLimit middleware's grace window.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, badReq())
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("first failure inside dwell window: status = %d, want 401", w.Code)
	}

	// Advance the fake clock past the dwell-time: Take() now returns
	// ErrThrottleUnavailable. The failed-auth throttle must fail CLOSED
	// (block with 429 + Retry-After) rather than let credential
	// guessing continue unbounded for the rest of the outage.
	fakeNow = fakeNow.Add(31 * time.Second)
	closed := obs.RateLimitFailClosedTotal.WithLabelValues(obs.RateLimiterFailedAuth)
	before := testutil.ToFloat64(closed)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, badReq())
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("sustained outage: status = %d, want 429 (fail-closed, not a bare 401)", w2.Code)
	}
	if got := testutil.ToFloat64(closed) - before; got != 1 {
		t.Errorf("fail_closed_total{limiter=failed_auth} delta = %v, want 1", got)
	}
	if ra := w2.Header().Get("Retry-After"); ra == "" {
		t.Error("fail-closed response during sustained outage must carry Retry-After")
	}
}

// TestAuth_FailedAuthThrottle is the regression: invalid-credential
// attempts must be throttled PER IP. Auth runs before the main rate
// limiter, so without this a wrong key is rejected (401) before reaching
// any limiter and can be retried without bound (credential stuffing). On
// the unfixed code the (N+1)th bad key from one IP is still a plain 401;
// with the fix it becomes 429.
func TestAuth_FailedAuthThrottle(t *testing.T) {
	// Direct-peer keying: no trusted proxies, so remoteIPFor uses
	// RemoteAddr's host deterministically.
	if err := middleware.SetTrustedProxyCIDRs([]string{}); err != nil {
		t.Fatalf("SetTrustedProxyCIDRs: %v", err)
	}

	const budget = 3
	// nil rdb → in-process fallback (no Redis needed for the test).
	limiter := ratelimit.New(nil, budget, time.Minute)

	mw := middleware.Auth(middleware.AuthOptions{
		Mode:              middleware.AuthModeAPIKey,
		APIKey:            stubAPIKeyValidator{knownKey: "good-key"},
		FailedAuthLimiter: limiter,
	})
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := mw(inner)

	badReq := func(remoteAddr string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
		r.RemoteAddr = remoteAddr
		r.Header.Set("X-API-Key", "wrong-key")
		return r
	}

	const ip = "203.0.113.7:44444"

	// First `budget` bad attempts get the ordinary 401.
	for i := 1; i <= budget; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, badReq(ip))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("bad attempt %d: status = %d, want 401", i, w.Code)
		}
	}

	// The (budget+1)th bad attempt from the SAME ip is throttled: 429.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, badReq(ip))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget bad attempt: status = %d, want 429 (per-IP failed-auth throttle)", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Error("429 failed-auth response must carry Retry-After")
	}

	// A DIFFERENT ip still has its full budget — the throttle is per-IP.
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, badReq("198.51.100.20:5555"))
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("different IP first attempt: status = %d, want 401 (per-IP isolation)", w2.Code)
	}

	// A VALID key from the throttled ip still passes: successful auth
	// never consumes the failed-auth budget, so the Auth-before-RateLimit
	// ordering for legitimate callers is preserved.
	good := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	good.RemoteAddr = ip
	good.Header.Set("X-API-Key", "good-key")
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, good)
	if w3.Code != http.StatusOK {
		t.Fatalf("valid key from throttled IP: status = %d, want 200 (valid requests are never failed-auth-throttled)", w3.Code)
	}
}

// TestAuth_FailedAuthThrottle_NilLimiterIsNoop confirms that with no
// limiter wired (e.g. failed_auth_rate_limit_per_min=0), bad keys keep
// returning 401 without bound — the throttle is purely additive.
func TestAuth_FailedAuthThrottle_NilLimiterIsNoop(t *testing.T) {
	mw := middleware.Auth(middleware.AuthOptions{
		Mode:   middleware.AuthModeAPIKey,
		APIKey: stubAPIKeyValidator{knownKey: "good-key"},
		// FailedAuthLimiter deliberately nil.
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 10; i++ {
		r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
		r.RemoteAddr = "203.0.113.9:1234"
		r.Header.Set("X-API-Key", "wrong-key")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d with nil limiter: status = %d, want 401 (no throttle)", i, w.Code)
		}
	}
}

// TestAuth_FailedAuthThrottle_PerKeyPrefix is the RSEC-R4 regression: a
// distributed guesser aiming at ONE key from many IPs must still be
// throttled. With an IP-only bucket every guess below lands in a fresh
// per-IP bucket and returns 401 forever; the key-prefix dimension turns
// the (budget+1)th guess at the same prefix into a 429.
func TestAuth_FailedAuthThrottle_PerKeyPrefix(t *testing.T) {
	if err := middleware.SetTrustedProxyCIDRs([]string{}); err != nil {
		t.Fatalf("SetTrustedProxyCIDRs: %v", err)
	}
	const budget = 3
	const targetPrefix = "sip_4f9c1d8b"
	validKey := targetPrefix + strings.Repeat("0", 56)
	guess := func(i int) string { return targetPrefix + strings.Repeat("f", 55) + strconv.Itoa(i%10) }

	for _, mode := range []middleware.AuthMode{middleware.AuthModeAPIKey, middleware.AuthModeAPIKeyOptional} {
		t.Run(string(mode), func(t *testing.T) {
			h := middleware.Auth(middleware.AuthOptions{
				Mode:              mode,
				APIKey:            stubAPIKeyValidator{knownKey: validKey},
				FailedAuthLimiter: ratelimit.New(nil, budget, time.Minute),
			})(okHandler())
			req := func(ip int, key string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
				r.RemoteAddr = "198.51.100." + strconv.Itoa(ip) + ":4000"
				r.Header.Set("X-API-Key", key)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}

			for i := 1; i <= budget; i++ {
				if w := req(i, guess(i)); w.Code != http.StatusUnauthorized {
					t.Fatalf("guess %d from a fresh IP: status = %d, want 401", i, w.Code)
				}
			}
			w := req(budget+1, guess(budget+1))
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("guess %d at the same key prefix from a fresh IP: status = %d, want 429 (per-key-prefix throttle)", budget+1, w.Code)
			}
			if w.Header().Get("Retry-After") == "" {
				t.Error("per-key-prefix 429 must carry Retry-After")
			}

			// A guess at a DIFFERENT prefix from a fresh IP is unaffected.
			if w := req(50, "sip_00000000"+strings.Repeat("f", 56)); w.Code != http.StatusUnauthorized {
				t.Fatalf("other prefix: status = %d, want 401 (per-prefix isolation)", w.Code)
			}
			// The key's holder is never locked out by guesses at their
			// prefix: a successful lookup never consults the budget.
			if w := req(60, validKey); w.Code != http.StatusOK {
				t.Fatalf("valid key under an exhausted prefix bucket: status = %d, want 200", w.Code)
			}
		})
	}
}

// TestAuth_FailedAuthTotal_CountsEveryRejection pins the fleet-wide
// tripwire counter behind stellarindex_failed_auth_rate_high: every
// credential rejection increments it by outcome, while a valid key and a
// server-side misconfiguration (503) do not.
func TestAuth_FailedAuthTotal_CountsEveryRejection(t *testing.T) {
	if err := middleware.SetTrustedProxyCIDRs([]string{}); err != nil {
		t.Fatalf("SetTrustedProxyCIDRs: %v", err)
	}
	rejected := obs.FailedAuthTotal.WithLabelValues(obs.FailedAuthRejected)
	throttled := obs.FailedAuthTotal.WithLabelValues(obs.FailedAuthThrottled)
	rej0, thr0 := testutil.ToFloat64(rejected), testutil.ToFloat64(throttled)

	h := middleware.Auth(middleware.AuthOptions{
		Mode:              middleware.AuthModeAPIKey,
		APIKey:            stubAPIKeyValidator{knownKey: "good-key"},
		FailedAuthLimiter: ratelimit.New(nil, 1, time.Minute),
	})(okHandler())
	misconfigured := middleware.Auth(middleware.AuthOptions{Mode: middleware.AuthModeAPIKey})(okHandler())
	serve := func(handler http.Handler, key string) int {
		r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
		r.RemoteAddr = "203.0.113.77:4000"
		r.Header.Set("X-API-Key", key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}

	for _, step := range []struct {
		handler http.Handler
		key     string
		want    int
	}{
		{h, "wrong-key", http.StatusUnauthorized},
		{h, "wrong-key", http.StatusTooManyRequests},
		{h, "good-key", http.StatusOK},
		{misconfigured, "wrong-key", http.StatusServiceUnavailable},
	} {
		if got := serve(step.handler, step.key); got != step.want {
			t.Fatalf("key %q: status = %d, want %d", step.key, got, step.want)
		}
	}
	if got := testutil.ToFloat64(rejected) - rej0; got != 1 {
		t.Errorf("rejected delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(throttled) - thr0; got != 1 {
		t.Errorf("throttled delta = %v, want 1", got)
	}
}

// TestAuth_FailedAuthThrottle_SEP10NotPooledByPrefix pins that the
// key-prefix dimension is API-key only. Every JWT starts with the same
// base64 header, so keying SEP-10 failures on their leading bytes would
// pool every bad token server-wide into one bucket.
func TestAuth_FailedAuthThrottle_SEP10NotPooledByPrefix(t *testing.T) {
	if err := middleware.SetTrustedProxyCIDRs([]string{}); err != nil {
		t.Fatalf("SetTrustedProxyCIDRs: %v", err)
	}
	const budget = 2
	h := middleware.Auth(middleware.AuthOptions{
		Mode:              middleware.AuthModeSEP10,
		SEP10:             stubSEP10Validator{knownJWT: "good"},
		FailedAuthLimiter: ratelimit.New(nil, budget, time.Minute),
	})(okHandler())
	for i := 1; i <= 3*budget; i++ {
		r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
		r.RemoteAddr = "198.51.100." + strconv.Itoa(i) + ":4000"
		r.Header.Set("Authorization", "Bearer eyJhbGciOiJFZERTQSJ9.e30.sig"+strconv.Itoa(i))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("bad JWT %d from a fresh IP: status = %d, want 401 (no cross-IP JWT pooling)", i, w.Code)
		}
	}
}

// TestAuth_FailedAuthThrottle_ClientAbortsDoNotArmFailClosed pins the
// hazard on the failed-auth (credential-stuffing) throttle: unlike
// [middleware.RateLimit] / [middleware.RateLimitBySubject], which detach
// from the request's cancellation via throttleContext before calling
// into [ratelimit.Bucket],
// takeFailedAuth passed r.Context() straight into limiter.Take. A
// client that opens a connection, sends a bad API key and RSTs before
// the Redis round-trip completes hands the bucket a context.Canceled
// error, which [ratelimit.Bucket] cannot distinguish from a Redis
// failure — every abort arms the dwell clock. Enough aborts in a row
// trip ErrThrottleUnavailable on a perfectly healthy Redis, and the
// failed-auth throttle fails CLOSED (429) for a well-behaved bad-key
// attempt that should just get a plain 401.
func TestAuth_FailedAuthThrottle_ClientAbortsDoNotArmFailClosed(t *testing.T) {
	if err := middleware.SetTrustedProxyCIDRs([]string{}); err != nil {
		t.Fatalf("SetTrustedProxyCIDRs: %v", err)
	}

	rdb, _ := newRLRedis(t)
	clock := newManualClock()
	limiter := ratelimit.New(rdb, 100, time.Minute, ratelimit.WithClock(clock.now))

	mw := middleware.Auth(middleware.AuthOptions{
		Mode:              middleware.AuthModeAPIKey,
		APIKey:            stubAPIKeyValidator{knownKey: "good-key"},
		FailedAuthLimiter: limiter,
	})
	h := mw(okHandler())

	abortedBadReq := func() *http.Request {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r := httptest.NewRequest(http.MethodGet, "/v1/price", nil).WithContext(ctx)
		r.RemoteAddr = "203.0.113.55:44440"
		r.Header.Set("X-API-Key", "wrong-key")
		return r
	}

	// First abort: without the detach, this is where the dwell clock gets armed.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, abortedBadReq())
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("first aborted bad-key attempt: status = %d, want 401 (Redis is healthy)", w.Code)
	}

	// Past the dwell window, with nothing but aborts in between.
	clock.advance(ratelimit.DefaultDwellTime + time.Second)

	// The decisive request: still just a bad key against a healthy
	// Redis. Must be a plain 401, never a 429/503 manufactured purely
	// by the aborts.
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, abortedBadReq())
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("bad-key attempt after abort-only dwell window: status = %d, want 401 — "+
			"client aborts armed the fail-closed dwell clock on the failed-auth throttle "+
			"while Redis was healthy", w2.Code)
	}
}
