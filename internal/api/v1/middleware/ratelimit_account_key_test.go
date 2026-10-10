package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// RateLimitCallerKey is the identity handler-level per-caller caps charge,
// so it must collapse exactly as the limiter does: every key on one account
// is one caller, and an anonymous caller is its IP, not its User-Agent hash.
func TestRateLimitCallerKey_MatchesTheLimiterIdentity(t *testing.T) {
	req := func(remote string, sub *auth.Subject) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
		r.RemoteAddr = remote
		if sub != nil {
			r = r.WithContext(auth.WithSubject(r.Context(), *sub))
		}
		return r
	}
	keyed := func(acct, keyID string) *auth.Subject {
		return &auth.Subject{Identifier: auth.AccountIdentifier(acct), Tier: auth.TierAPIKey, KeyID: keyID}
	}
	anon := &auth.Subject{Identifier: "ua-hash-1", Tier: auth.TierAnonymous}

	if a, b := middleware.RateLimitCallerKey(req("198.51.100.1:1", keyed("co", "kid_a"))),
		middleware.RateLimitCallerKey(req("203.0.113.9:1", keyed("co", "kid_b"))); a != b {
		t.Errorf("two keys on one account keyed %q and %q, want one caller", a, b)
	}
	if a, b := middleware.RateLimitCallerKey(req("198.51.100.1:1", keyed("co", "kid_a"))),
		middleware.RateLimitCallerKey(req("198.51.100.1:1", keyed("other", "kid_a"))); a == b {
		t.Errorf("two accounts share caller key %q", a)
	}
	if a, b := middleware.RateLimitCallerKey(req("198.51.100.1:1", anon)),
		middleware.RateLimitCallerKey(req("198.51.100.1:2", nil)); a != b {
		t.Errorf("one anonymous IP keyed %q and %q, want one caller", a, b)
	}
	if a, b := middleware.RateLimitCallerKey(req("198.51.100.1:1", nil)),
		middleware.RateLimitCallerKey(req("198.51.100.2:1", nil)); a == b {
		t.Errorf("two anonymous IPs share caller key %q", a)
	}
}
