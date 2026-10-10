package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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
// whole authenticated tier) while Redis is perfectly healthy.
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
