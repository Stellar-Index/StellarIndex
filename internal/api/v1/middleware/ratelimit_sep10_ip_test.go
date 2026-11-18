package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// A SEP-10 account is a free keypair: two of them from one IP share one authenticated allowance, while another IP,
// and an API key held by a SEP-10 identifier, keep their own.
func TestRateLimitBySubject_SEP10AccountsFromOneIPShareTheAllowance(t *testing.T) {
	rdb, _ := newRLRedis(t)
	anonBucket := ratelimit.New(rdb, 1, time.Minute, frozenClock)
	authBucket := ratelimit.New(rdb, 2, time.Minute, frozenClock)
	h := middleware.RateLimitBySubject(anonBucket, authBucket, nil, nil)(okHandler())

	serve := func(remote string, sub auth.Subject) int {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		r = r.WithContext(auth.WithSubject(r.Context(), sub))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	jwt := func(acct string) auth.Subject { return auth.Subject{Identifier: acct, Tier: auth.TierSEP10} }

	if c := serve("198.51.100.7:1", jwt("GAAA")); c != http.StatusOK {
		t.Fatalf("first account: %d, want 200", c)
	}
	if c := serve("198.51.100.7:2", jwt("GBBB")); c != http.StatusOK {
		t.Fatalf("second account: %d, want 200", c)
	}
	if c := serve("198.51.100.7:3", jwt("GCCC")); c != http.StatusTooManyRequests {
		t.Errorf("third fresh account from the same IP: %d, want 429 — each keypair minted a new allowance", c)
	}
	if c := serve("203.0.113.5:1", jwt("GDDD")); c != http.StatusOK {
		t.Errorf("another IP: %d, want 200", c)
	}
	key := auth.Subject{Identifier: "GEEE", Tier: auth.TierSEP10, KeyID: "kid_e"}
	if c := serve("198.51.100.7:4", key); c != http.StatusOK {
		t.Errorf("API key on a SEP-10 identifier from the spent IP: %d, want 200", c)
	}
}
