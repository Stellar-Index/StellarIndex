package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// A client abort must never reach the limiter as a backend error.
//
// ratelimit.Bucket cannot tell a caller-cancelled call from a Redis
// outage: every error out of the take arms its dwell clock and resets
// the recovery streak. Passing the REQUEST's context into the take
// therefore handed any client a remote kill switch — open a connection,
// send a request, RST, repeat a few times a second, and the 30 s
// unbroken-success streak needed to disarm can never accumulate, so the
// bucket answers ErrThrottleUnavailable and the middleware fails CLOSED
// with 503 for EVERY caller sharing it (the whole anonymous tier, or the
// whole authenticated tier) while Redis is perfectly healthy
// (REL-06 F059, reverification-2026-09-18).
//
// Redis is HEALTHY in both tests below. Every 503 they could produce is
// manufactured entirely by aborted requests.

// abortedRequest is a request whose context is already cancelled — what
// the middleware sees once the client has gone away.
func abortedRequest(t *testing.T) *http.Request {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return httptest.NewRequest(http.MethodGet, "/v1/price?asset=native", nil).WithContext(ctx)
}

func TestRateLimitBySubject_ClientAbortsDoNotArmFailClosed(t *testing.T) {
	rdb, _ := newRLRedis(t)
	clock := newManualClock()
	b := ratelimit.New(rdb, 100, time.Minute, ratelimit.WithClock(clock.now))

	h := middleware.RateLimitBySubject(b, nil, nil, nil)(okHandler())

	// First abort: arms the dwell clock pre-fix.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, abortedRequest(t))
	if got := w.Header().Get("X-RateLimit-Remaining"); got != "99" {
		t.Errorf("X-RateLimit-Remaining after an aborted request = %q, want %q — the take must still "+
			"reach a HEALTHY Redis and charge the token; an empty header means it errored out and the "+
			"middleware fell open, which is also unmetered traffic for anyone who aborts", got, "99")
	}

	// Past the 30 s dwell window, with nothing but aborts in between.
	clock.advance(ratelimit.DefaultDwellTime + time.Second)

	w = httptest.NewRecorder()
	h.ServeHTTP(w, abortedRequest(t))
	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("status = 503 after two aborted requests %v apart — client aborts armed the "+
			"fail-closed dwell clock, so any client can take the whole bucket's tier offline while "+
			"Redis is healthy", ratelimit.DefaultDwellTime+time.Second)
	}

	// The decisive one: a WELL-BEHAVED caller sharing the bucket.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/price?asset=native", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("an innocent caller got %d, want 200 — the abort flood must not fail the throttle "+
			"CLOSED for everyone else on this bucket", w.Code)
	}
}

// Same property on the single-bucket [middleware.RateLimit] entry point,
// which shares the take-with-request-context shape.
func TestRateLimit_ClientAbortsDoNotArmFailClosed(t *testing.T) {
	rdb, _ := newRLRedis(t)
	clock := newManualClock()
	b := ratelimit.New(rdb, 100, time.Minute, ratelimit.WithClock(clock.now))

	h := middleware.RateLimit(b, fixedKeyFn("abort-k"), nil, nil)(okHandler())

	w := httptest.NewRecorder()
	h.ServeHTTP(w, abortedRequest(t))
	if got := w.Header().Get("X-RateLimit-Remaining"); got != "99" {
		t.Errorf("X-RateLimit-Remaining after an aborted request = %q, want %q", got, "99")
	}

	clock.advance(ratelimit.DefaultDwellTime + time.Second)

	w = httptest.NewRecorder()
	h.ServeHTTP(w, abortedRequest(t))
	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("status = 503 — client aborts armed the fail-closed dwell clock on a healthy Redis")
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/price?asset=native", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("an innocent caller got %d, want 200", w.Code)
	}
}

// Blast-radius guard: detaching from the client's cancellation must not
// detach from the BACKEND's failure. A genuinely broken Redis still has
// to arm the dwell clock and fail closed past the window — that
// inversion (F-0050 / F-0150) is the reason the clock exists.
func TestRateLimitBySubject_RealRedisOutageStillFailsClosed(t *testing.T) {
	rdb, mr := newRLRedis(t)
	clock := newManualClock()
	b := ratelimit.New(rdb, 100, time.Minute, ratelimit.WithClock(clock.now))

	h := middleware.RateLimitBySubject(b, nil, nil, nil)(okHandler())

	mr.Close() // every take from here on is a real transport failure

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/price?asset=native", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("first outage request = %d, want 200 (fail OPEN inside the dwell window)", w.Code)
	}

	clock.advance(ratelimit.DefaultDwellTime + time.Second)

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/price?asset=native", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("sustained-outage request = %d, want 503 — a real Redis outage past the dwell "+
			"window must still fail CLOSED", w.Code)
	}
}
