package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A client abort must never reach the limiter as a backend error.
//
// ratelimit.Bucket cannot tell a caller-cancelled call from a Redis outage: every error out of
// the take arms its dwell clock and resets the recovery streak. Passing the REQUEST's context
// into the take would hand any client a remote kill switch: repeated aborted requests keep the
// 30 s unbroken-success streak from ever accumulating, so the bucket answers
// ErrThrottleUnavailable and the middleware fails CLOSED with 503 for EVERY caller sharing it
// while Redis is healthy.
//
// Redis is HEALTHY in both tests, so any 503 they could produce comes from aborted requests.

// abortedRequest is a request whose context is already cancelled — what
// the middleware sees once the client has gone away.
func abortedRequest(t *testing.T) *http.Request {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return httptest.NewRequest(http.MethodGet, "/v1/price?asset=native", nil).WithContext(ctx)
}
