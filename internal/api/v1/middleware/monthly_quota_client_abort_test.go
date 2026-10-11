// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// The monthly-quota gate mirrors ratelimit.Bucket's dwell clock and its blind spot: the
// month-to-date read runs on the REQUEST's context, and the gate cannot tell a caller-cancelled
// read from a counter outage. The clock is process-wide, so a client that connects and
// immediately RSTs PRE-ARMS `redisErrorSince` for everyone.
//
// That defeats the gate's invariant "a single blip must fail OPEN so a transient Redis hiccup
// does not 429 paying customers": with the clock pre-armed and the dwell window elapsed, the
// first genuine blip becomes a fail-CLOSED 429 for whichever metered customer hits it.
//
// The counter is HEALTHY except where a blip is injected explicitly.

// abortAwareMTDReader answers 0 (well under any cap) on a live context.
// It returns ctx.Err() on a dead one — what go-redis and database/sql
// both do — and `blip` when set, to inject one genuine transient error.
type abortAwareMTDReader struct {
	blip      error
	liveReads int
}

func (r *abortAwareMTDReader) MonthToDate(ctx context.Context, _ string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if r.blip != nil {
		return 0, r.blip
	}
	r.liveReads++
	return 0, nil
}

// runAbortedWithSubject drives one request whose client has already gone
// away.
func runAbortedWithSubject(t *testing.T, mw middleware.Middleware, sub auth.Subject) int {
	t.Helper()
	ctx, cancel := context.WithCancel(auth.WithSubject(context.Background(), sub))
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/price?asset=native&quote=fiat:USD", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(w, req)
	return w.Code
}
