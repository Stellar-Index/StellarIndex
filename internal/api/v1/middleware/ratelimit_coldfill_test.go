package middleware_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// coldFillHandler is a cold read in production shape: pay the surcharge, answer a refusal with the limiter's response, else work.
func coldFillHandler(worked *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := ratelimit.ChargeColdFill(r.Context()); err != nil {
			if !middleware.WriteRateLimitRefusal(w, r, err) {
				w.WriteHeader(http.StatusInternalServerError)
			}
			return
		}
		*worked++
		w.WriteHeader(http.StatusOK)
	})
}

// TestChargeColdFill_AddsToTheRequestAndRefusesWith429: each cold fill costs 1 + ColdFillCost against the same bucket the base
// charge uses; the refusal is the limiter's 429 envelope with Retry-After and the rate-limit headers.
func TestChargeColdFill_AddsToTheRequestAndRefusesWith429(t *testing.T) {
	rdb, _ := newRLRedis(t)
	const budget = 20
	b := ratelimit.New(rdb, budget, time.Minute, frozenClock)
	var worked int
	h := middleware.RateLimitBySubject(b, nil, nil, nil)(coldFillHandler(&worked))

	perRequest := 1 + ratelimit.ColdFillCost
	for i := range budget / perRequest {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200", i+1, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget cold fill: status %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" || w.Header().Get("X-RateLimit-Limit") != "20" ||
		w.Header().Get("X-RateLimit-Remaining") != "0" {
		t.Fatalf("429 headers = %v, want Retry-After and X-RateLimit-Limit 20 / Remaining 0", w.Header())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want the problem envelope", ct)
	}
	if worked != budget/perRequest {
		t.Fatalf("worked %d times, want %d: a refused cold fill ran", worked, budget/perRequest)
	}
}

// TestWriteRateLimitRefusal_IgnoresOtherErrors: only a refused charge is the limiter's to answer.
func TestWriteRateLimitRefusal_IgnoresOtherErrors(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, err := range []error{nil, errors.New("boom"), ratelimit.ErrColdFillRefused} {
		if middleware.WriteRateLimitRefusal(w, r, err) {
			t.Fatalf("wrote a refusal for %v", err)
		}
	}
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("recorder written: %d %q", w.Code, w.Body.String())
	}
}
