package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
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
	jwt := func(acct string) auth.Subject { return auth.NewSEP10Session(acct, time.Time{}) }

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

// When both charges pass, X-RateLimit-* must report the bucket that binds (fewer tokens left), on the base charge and on re-pricing.
func TestRateLimitBySubject_SEP10HeadersReportTheBindingBucket(t *testing.T) {
	rdb, _ := newRLRedis(t)
	anonBucket := ratelimit.New(rdb, 1, time.Minute, frozenClock)
	authBucket := ratelimit.New(rdb, 10, time.Minute, frozenClock)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cost, err := strconv.Atoi(r.URL.Query().Get("cost")); err == nil && !middleware.ChargeRateLimit(w, r, cost) {
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h := middleware.RateLimitBySubject(anonBucket, authBucket, nil, nil)(inner)

	serve := func(target, acct string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r.RemoteAddr = "198.51.100.7:1"
		r = r.WithContext(auth.WithSubject(r.Context(), auth.NewSEP10Session(acct, time.Time{})))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for i := range 7 {
		if w := serve("/", "GAAA"); w.Code != http.StatusOK {
			t.Fatalf("account A call %d: %d, want 200", i+1, w.Code)
		}
	}
	// IP has 3 left, account B has 10: B's call leaves IP 2, account 9.
	w := serve("/", "GBBB")
	if got := w.Header().Get("X-RateLimit-Remaining"); w.Code != http.StatusOK || got != "2" {
		t.Errorf("account B first call: %d Remaining %q, want 200 Remaining 2 (the per-IP bucket)", w.Code, got)
	}
	if got := w.Header().Get("X-RateLimit-Limit"); got != "10" {
		t.Errorf("account B first call: Limit %q, want 10", got)
	}
	// Re-priced to 2 in total: the base token leaves IP 1, the extra one IP 0, while account C has 8.
	w = serve("/?cost=2", "GCCC")
	if got := w.Header().Get("X-RateLimit-Remaining"); w.Code != http.StatusOK || got != "0" {
		t.Errorf("account C re-priced call: %d Remaining %q, want 200 Remaining 0 (the per-IP bucket)", w.Code, got)
	}
}
