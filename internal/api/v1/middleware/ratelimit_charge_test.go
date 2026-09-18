package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// chargingHandler is the shape a cost-scaled route takes: re-price the
// request once its cost is known, and do the work only if that clears.
// worked counts how often the work ran.
func chargingHandler(cost int, worked *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !middleware.ChargeRateLimit(w, r, cost) {
			return
		}
		*worked++
		w.WriteHeader(http.StatusOK)
	})
}

// TestChargeRateLimit_PricesTheRequestInTotal pins the contract: cost
// is the request's TOTAL price, the base token the middleware took
// before dispatch counts toward it, and the response restates
// X-RateLimit-Remaining to the post-charge figure.
func TestChargeRateLimit_PricesTheRequestInTotal(t *testing.T) {
	rdb, _ := newRLRedis(t)
	b := ratelimit.New(rdb, 100, time.Minute)
	var worked int
	h := middleware.RateLimitBySubject(b, nil, nil, nil)(chargingHandler(30, &worked))

	for i, want := range []string{"70", "40", "10"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, w.Code)
		}
		if got := w.Header().Get("X-RateLimit-Remaining"); got != want {
			t.Fatalf("request %d: X-RateLimit-Remaining = %q, want %q (a cost-30 request spends 30 in total, not 31)",
				i+1, got, want)
		}
	}

	// 90 spent. The fourth passes the base charge (91) and is denied by
	// its own re-pricing (91 + 29 > 100): the handler must not work.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget request: status = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After")
	}
	if got := w.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("X-RateLimit-Remaining on denial = %q, want 0", got)
	}
	if worked != 3 {
		t.Fatalf("handler worked %d times, want 3 — a denied charge must stop the work", worked)
	}
}

// TestChargeRateLimit_CapsAtTheSubjectsCeiling: a request priced above
// the caller's whole budget spends the window rather than being refused
// in every window — and the cap is on the TOTAL, so the base token
// already taken does not push it one over. The ceiling is the
// per-subject override when the key record carries one.
func TestChargeRateLimit_CapsAtTheSubjectsCeiling(t *testing.T) {
	rdb, _ := newRLRedis(t)
	anon := ratelimit.New(rdb, 60, time.Minute)
	authB := ratelimit.New(rdb, 60, time.Minute)
	var worked int
	h := middleware.RateLimitBySubject(anon, authB, nil, nil)(chargingHandler(1000, &worked))

	// Anonymous: ceiling 60.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("cost-1000 request into a fresh 60/min window: status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 0", got)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request in the spent window: status = %d, want 429", w.Code)
	}

	// Paid key with a 5000/min override: the same request costs 1000 of
	// 5000, not the bucket's 60.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(auth.WithSubject(r.Context(), auth.Subject{
		Identifier: "owner-paid", Tier: auth.TierAPIKey, KeyID: "kid_paid", RateLimitPerMin: 5000,
	}))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("paid key: status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("X-RateLimit-Limit"); got != "5000" {
		t.Errorf("paid key: X-RateLimit-Limit = %q, want 5000", got)
	}
	if got := w.Header().Get("X-RateLimit-Remaining"); got != "4000" {
		t.Fatalf("paid key: X-RateLimit-Remaining = %q, want 4000", got)
	}
}

// TestChargeRateLimit_NeverRefundsOrDoubleCharges: a cost at or below
// what the request has paid charges nothing, and re-pricing twice at the
// same figure charges once.
func TestChargeRateLimit_NeverRefundsOrDoubleCharges(t *testing.T) {
	rdb, _ := newRLRedis(t)
	b := ratelimit.New(rdb, 100, time.Minute)
	h := middleware.RateLimitBySubject(b, nil, nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, cost := range []int{0, -7, 1, 20, 20, 5} {
			if !middleware.ChargeRateLimit(w, r, cost) {
				t.Errorf("ChargeRateLimit(%d) denied inside budget", cost)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := w.Header().Get("X-RateLimit-Remaining"); got != "80" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 80 (the request's price is its highest quote: 20)", got)
	}
}

// TestChargeRateLimit_NoLimiterMeansNoCharge: a request that never
// crossed a rate-limit middleware — limiting disabled for its class, a
// skipped path — has no account, and the handler proceeds untouched.
func TestChargeRateLimit_NoLimiterMeansNoCharge(t *testing.T) {
	rdb, _ := newRLRedis(t)
	b := ratelimit.New(rdb, 1, time.Minute)
	var worked int

	cases := map[string]http.Handler{
		"no middleware": chargingHandler(500, &worked),
		"skipped path":  middleware.RateLimitBySubject(b, nil, func(*http.Request) bool { return true }, nil)(chargingHandler(500, &worked)),
		"nil buckets":   middleware.RateLimitBySubject(nil, nil, nil, nil)(chargingHandler(500, &worked)),
	}
	for name, h := range cases {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", name, w.Code)
		}
		if got := w.Header().Get("X-RateLimit-Remaining"); got != "" {
			t.Errorf("%s: X-RateLimit-Remaining = %q, want it absent", name, got)
		}
	}
	if worked != len(cases) {
		t.Fatalf("worked = %d, want %d", worked, len(cases))
	}
}

// TestChargeRateLimit_SingleBucketMiddlewareChargesToo covers the
// sibling constructor. RateLimit shares the primitive, so a handler
// behind it must be re-priceable on the same terms.
func TestChargeRateLimit_SingleBucketMiddlewareChargesToo(t *testing.T) {
	rdb, _ := newRLRedis(t)
	b := ratelimit.New(rdb, 50, time.Minute)
	var worked int
	h := middleware.RateLimit(b, fixedKeyFn("k-charge"), nil, nil)(chargingHandler(20, &worked))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := w.Header().Get("X-RateLimit-Remaining"); got != "30" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 30", got)
	}
}

// TestChargeRateLimit_FollowsTheLimitersFailurePolicy: the re-pricing
// shares the base charge's failure policy rather than inventing one.
// Inside the dwell window a Redis error fails OPEN; past it the throttle
// layer is declared unavailable and the re-pricing fails CLOSED with the
// same 503 the middleware writes.
//
// Both halves run inside ONE request so that it is the handler-side
// charge, not the middleware's base charge, that meets each state.
func TestChargeRateLimit_FollowsTheLimitersFailurePolicy(t *testing.T) {
	// A Redis that is down before the client ever dials it — see
	// TestRateLimit_FailsOpenOnRedisError for why it is built this way.
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	clock := newManualClock()
	b := ratelimit.New(rdb, 100, time.Minute, ratelimit.WithClock(clock.now))

	var openedInsideDwell, closedPastDwell bool
	h := middleware.RateLimitBySubject(b, nil, nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The base charge already failed open and armed the dwell clock.
		openedInsideDwell = middleware.ChargeRateLimit(w, r, 10)
		if !openedInsideDwell {
			return
		}
		clock.advance(ratelimit.DefaultDwellTime + time.Second)
		closedPastDwell = !middleware.ChargeRateLimit(w, r, 40)
		if !closedPastDwell {
			w.WriteHeader(http.StatusOK)
		}
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if !openedInsideDwell {
		t.Fatal("a Redis error inside the dwell window must fail OPEN")
	}
	if !closedPastDwell {
		t.Fatal("a Redis error past the dwell window must fail CLOSED")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
}
