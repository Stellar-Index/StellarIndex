// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// GH-1144: the pre-handler throttle seams detach from the request's
// cancellation so a client abort cannot arm their dwell clocks, but
// context.WithoutCancel drops the DEADLINE too. With a flat 5 s per seam
// the MonthlyQuota read and the RateLimit take ran outside the request
// timeout they sit inside, pushing worst-case wall time past the
// server's WriteTimeout.

func TestThrottleContext_NeverOutlivesTheRequestDeadline(t *testing.T) {
	reqCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	reqDeadline, _ := reqCtx.Deadline()
	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil).WithContext(reqCtx)

	ctx, done := throttleContext(r)
	defer done()
	got, ok := ctx.Deadline()
	if !ok {
		t.Fatal("throttle context has no deadline")
	}
	if got.After(reqDeadline) {
		t.Fatalf("throttle deadline is %v past the request deadline — the seam can run after the "+
			"request has timed out", got.Sub(reqDeadline))
	}
}

func TestThrottleContext_StillDetachedFromClientAbort(t *testing.T) {
	reqCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil).WithContext(reqCtx)
	cancel()

	ctx, done := throttleContext(r)
	defer done()
	if err := ctx.Err(); err != nil {
		t.Fatalf("throttle context inherited the client abort (%v) — that arms the dwell clock", err)
	}
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > throttleTakeTimeout {
		t.Fatalf("deadline = %v (ok=%v), want within %v", dl, ok, throttleTakeTimeout)
	}
}

// ctxBoundMTDReader stands in for a wedged counter backend that honours
// its context, as the go-redis pool wait and the Postgres reader do.
type ctxBoundMTDReader struct{}

func (ctxBoundMTDReader) MonthToDate(ctx context.Context, _ string) (int64, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestMonthlyQuota_WedgedReadEndsAtTheRequestTimeout(t *testing.T) {
	sub := auth.Subject{Identifier: "acct:x", Tier: auth.TierAPIKey, KeyID: "kid_x", MonthlyQuota: 10}
	h := RequestTimeout(150 * time.Millisecond)(
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), sub)))
			})
		}(MonthlyQuota(ctxBoundMTDReader{}, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))))

	start := time.Now()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/price", nil))
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a wedged month-to-date read held the request %v under a 150ms request timeout — "+
			"the detached read must be bounded by what is left of the request deadline", elapsed)
	}
}
