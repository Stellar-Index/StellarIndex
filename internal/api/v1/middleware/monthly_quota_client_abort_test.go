// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// The monthly-quota gate mirrors ratelimit.Bucket's dwell clock
// (REL-06) and inherited its blind spot with it: the month-to-date read
// ran on the REQUEST's context, and the gate cannot tell a
// caller-cancelled read from a counter outage. Its clock is
// process-wide — one instance for the whole binary — so a client that
// connects and immediately RSTs PRE-ARMS `redisErrorSince` for everyone
// (REL-06 F059, reverification-2026-09-18).
//
// What that buys an attacker is precisely the invariant this gate
// documents: "a single blip must fail OPEN so a transient Redis hiccup
// does not 429 paying customers". With the clock pre-armed and the dwell
// window already elapsed, the very first genuine blip lands as a
// fail-CLOSED 429 for whichever metered customer happens to hit it.
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

func TestMonthlyQuota_ClientAbortsDoNotArmFailClosed(t *testing.T) {
	clock := newManualClock()
	reader := &abortAwareMTDReader{}
	mw := middleware.MonthlyQuota(reader, nil, middleware.WithMonthlyQuotaClock(clock.now))
	attacker := auth.Subject{Tier: auth.TierAPIKey, KeyID: "K-abort", MonthlyQuota: 1_000_000}

	// An abort flood: nothing but cancelled requests, spanning more than
	// the dwell window. Pre-fix this arms the process-wide clock and
	// keeps it armed.
	runAbortedWithSubject(t, mw, attacker)
	clock.advance(middleware.DefaultMonthlyQuotaDwellTime + time.Second)
	runAbortedWithSubject(t, mw, attacker)
	if reader.liveReads != 2 {
		t.Fatalf("the abort flood reached the counter on a live context %d times, want 2: "+
			"the month-to-date read is still bound to the client's cancellation", reader.liveReads)
	}

	// A DIFFERENT, well-behaved metered customer now hits one genuine
	// transient blip. The gate's documented posture for a single blip is
	// fail OPEN — the cap is billing fairness, not a security boundary.
	reader.blip = errors.New("redis MISCONF")
	victim := auth.Subject{Tier: auth.TierAPIKey, KeyID: "K-victim", MonthlyQuota: 1_000_000}
	status, _, body := runWithSubject(t, mw, victim)
	if status != http.StatusOK {
		t.Fatalf("an innocent metered customer got %d on their FIRST blip, want 200 (fail open). "+
			"Client aborts pre-armed the process-wide fail-closed clock, so an attacker can convert "+
			"every customer's next transient hiccup into a 429. Body: %s", status, body)
	}
	if strings.Contains(body, "monthly-quota-unavailable") {
		t.Errorf("fail-closed problem body served on a first blip: %s", body)
	}

	// And with the counter healthy again, metering is ordinary.
	reader.blip = nil
	readsBefore := reader.liveReads
	if status, _, _ := runWithSubject(t, mw, victim); status != http.StatusOK {
		t.Fatalf("healthy read status = %d, want 200", status)
	}
	if got := reader.liveReads - readsBefore; got != 1 {
		t.Fatalf("the healthy request read the counter %d times, want 1 — the gate is not actually metering", got)
	}
}

// Blast-radius guard: a genuine SUSTAINED outage must still fail closed
// past the dwell window (W1-flow-register-4). Detaching from the
// client's cancellation must not detach from the counter's failure.
func TestMonthlyQuota_RealOutageStillFailsClosedAfterAbortFix(t *testing.T) {
	clock := newManualClock()
	reader := &abortAwareMTDReader{blip: errors.New("redis down")}
	mw := middleware.MonthlyQuota(reader, nil, middleware.WithMonthlyQuotaClock(clock.now))
	sub := auth.Subject{Tier: auth.TierAPIKey, KeyID: "K1", MonthlyQuota: 1_000_000}

	if status, _, _ := runWithSubject(t, mw, sub); status != http.StatusOK {
		t.Fatalf("first outage request = %d, want 200 (fail open inside the dwell window)", status)
	}
	clock.advance(middleware.DefaultMonthlyQuotaDwellTime + time.Second)
	status, _, body := runWithSubject(t, mw, sub)
	if status != http.StatusTooManyRequests {
		t.Fatalf("sustained-outage request = %d, want 429 (fail CLOSED past the window)", status)
	}
	if !strings.Contains(body, "monthly-quota-unavailable") {
		t.Errorf("body missing the fail-closed problem type: %s", body)
	}
}
