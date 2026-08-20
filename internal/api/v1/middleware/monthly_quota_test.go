// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// fakeMTDReader implements middleware.MonthToDateReader with a
// canned per-subject month-to-date count + an optional error.
type fakeMTDReader struct {
	counts map[string]int64
	err    error
}

func (f *fakeMTDReader) MonthToDate(_ context.Context, subject string) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.counts[subject], nil
}

func runWithSubject(t *testing.T, mw middleware.Middleware, sub auth.Subject) (status int, headers http.Header, body string) {
	t.Helper()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/price?asset=native&quote=fiat:USD", nil)
	req = req.WithContext(auth.WithSubject(req.Context(), sub))
	w := httptest.NewRecorder()
	mw(next).ServeHTTP(w, req)
	if w.Code == http.StatusOK && !called {
		t.Fatal("status 200 reported but next handler never ran")
	}
	return w.Code, w.Header(), w.Body.String()
}

// TestMonthlyQuota_PassThroughWhenQuotaUnset — Subject with
// MonthlyQuota == 0 must skip the check entirely (the cap is
// opt-in per key).
func TestMonthlyQuota_PassThroughWhenQuotaUnset(t *testing.T) {
	reader := &fakeMTDReader{counts: map[string]int64{"key:K1": 999_999}}
	mw := middleware.MonthlyQuota(reader, nil)
	sub := auth.Subject{Tier: auth.TierAPIKey, KeyID: "K1", MonthlyQuota: 0}
	status, _, _ := runWithSubject(t, mw, sub)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200 (quota unset → no check)", status)
	}
}

// TestMonthlyQuota_PassThroughBelowCap — Subject with quota 100,
// MTD 50 → request should succeed.
func TestMonthlyQuota_PassThroughBelowCap(t *testing.T) {
	reader := &fakeMTDReader{counts: map[string]int64{"key:K1": 50}}
	mw := middleware.MonthlyQuota(reader, nil)
	sub := auth.Subject{Tier: auth.TierAPIKey, KeyID: "K1", MonthlyQuota: 100}
	status, _, _ := runWithSubject(t, mw, sub)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200 (50 < 100)", status)
	}
}

// TestMonthlyQuota_RejectsAtCap — when MTD reaches the quota, the
// middleware returns 429 with the documented Problem+JSON shape +
// the X-StellarIndex-Monthly-* observability headers.
func TestMonthlyQuota_RejectsAtCap(t *testing.T) {
	reader := &fakeMTDReader{counts: map[string]int64{"key:K1": 100}}
	mw := middleware.MonthlyQuota(reader, nil)
	sub := auth.Subject{Tier: auth.TierAPIKey, KeyID: "K1", MonthlyQuota: 100}
	status, headers, body := runWithSubject(t, mw, sub)
	if status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", status)
	}
	if got := headers.Get("X-StellarIndex-Monthly-Quota"); got != "100" {
		t.Errorf("X-StellarIndex-Monthly-Quota = %q, want 100", got)
	}
	if got := headers.Get("X-StellarIndex-Monthly-Used"); got != "100" {
		t.Errorf("X-StellarIndex-Monthly-Used = %q, want 100", got)
	}
	if !strings.Contains(body, `"monthly_quota":100`) {
		t.Errorf("body missing monthly_quota: %s", body)
	}
	if !strings.Contains(body, `"month_to_date":100`) {
		t.Errorf("body missing month_to_date: %s", body)
	}
}

// TestMonthlyQuota_RejectsAboveCap — used > quota also rejects
// (a delayed counter increment after a previous tick at-cap can
// land here).
func TestMonthlyQuota_RejectsAboveCap(t *testing.T) {
	reader := &fakeMTDReader{counts: map[string]int64{"key:K1": 250}}
	mw := middleware.MonthlyQuota(reader, nil)
	sub := auth.Subject{Tier: auth.TierAPIKey, KeyID: "K1", MonthlyQuota: 100}
	status, _, _ := runWithSubject(t, mw, sub)
	if status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 (over-cap)", status)
	}
}

// TestMonthlyQuota_FailOpenOnReaderError — usage caps must NEVER
// 500 paying customers when the underlying counter is briefly
// unavailable. Reader errors log + pass through.
func TestMonthlyQuota_FailOpenOnReaderError(t *testing.T) {
	reader := &fakeMTDReader{err: errors.New("redis blip")}
	mw := middleware.MonthlyQuota(reader, nil)
	sub := auth.Subject{Tier: auth.TierAPIKey, KeyID: "K1", MonthlyQuota: 1}
	status, _, _ := runWithSubject(t, mw, sub)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200 (fail-open on reader error)", status)
	}
}

// TestMonthlyQuota_NilReader_PassThrough — operator deployment
// without a usage counter (Redis-less) must pass through cleanly.
func TestMonthlyQuota_NilReader_PassThrough(t *testing.T) {
	mw := middleware.MonthlyQuota(nil, nil)
	sub := auth.Subject{Tier: auth.TierAPIKey, KeyID: "K1", MonthlyQuota: 100}
	status, _, _ := runWithSubject(t, mw, sub)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200 (nil reader → no check)", status)
	}
}

// TestMonthlyQuota_AnonymousPassThrough — anonymous Subjects
// don't carry quotas; the middleware short-circuits.
func TestMonthlyQuota_AnonymousPassThrough(t *testing.T) {
	reader := &fakeMTDReader{counts: map[string]int64{"key:K1": 10000}}
	mw := middleware.MonthlyQuota(reader, nil)
	sub := auth.Subject{Tier: auth.TierAnonymous, Identifier: "ip:1.2.3.4"}
	status, _, _ := runWithSubject(t, mw, sub)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200 (anon has no quota)", status)
	}
}

// TestMonthlyQuota_FailOpenIncrementsCounter is the C3-082 regression
// (audit-2026-07-23).
//
// The fail-open above is correct and stays — but before this counter the
// only trace that the metered-spend ceiling had switched itself OFF was a
// `logger.Debug` line, i.e. nothing at all at the API's production log
// level. A metered key can bill past its agreed cap for the whole window,
// and the overage is unrecoverable once the responses are served.
//
// Asserts the exact delta (1 per bypassed request) and, crucially, that a
// HEALTHY read does not touch the counter — a counter that ticks on the
// success path would make the alert permanently firing and worthless.
func TestMonthlyQuota_FailOpenIncrementsCounter(t *testing.T) {
	sub := auth.Subject{Tier: auth.TierAPIKey, KeyID: "K1", MonthlyQuota: 100}

	// Healthy read: under cap, no bypass.
	healthy := &fakeMTDReader{counts: map[string]int64{"key:K1": 1}}
	before := testutil.ToFloat64(obs.MonthlyQuotaFailOpenTotal)
	if status, _, _ := runWithSubject(t, middleware.MonthlyQuota(healthy, nil), sub); status != http.StatusOK {
		t.Fatalf("healthy read status = %d, want 200", status)
	}
	if got := testutil.ToFloat64(obs.MonthlyQuotaFailOpenTotal); got != before {
		t.Errorf("healthy read moved the fail-open counter: %v → %v", before, got)
	}

	// Backing-store error: the ceiling is bypassed and must be counted.
	broken := &fakeMTDReader{err: errors.New("redis blip")}
	mw := middleware.MonthlyQuota(broken, nil)
	for i := 0; i < 2; i++ {
		if status, _, _ := runWithSubject(t, mw, sub); status != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200 (fail-open)", i, status)
		}
	}
	if got, want := testutil.ToFloat64(obs.MonthlyQuotaFailOpenTotal), before+2; got != want {
		t.Errorf("stellarindex_monthly_quota_fail_open_total = %v, want %v (one per bypassed request)", got, want)
	}
}

// mutableMTDReader is a MonthToDateReader whose error / count can be
// flipped between requests, so one test can walk a counter through
// outage → recovery.
type mutableMTDReader struct {
	mu    sync.Mutex
	err   error
	count int64
}

func (m *mutableMTDReader) MonthToDate(context.Context, string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count, m.err
}

func (m *mutableMTDReader) fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

func (m *mutableMTDReader) heal(count int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = nil
	m.count = count
}

// manualClock is the injectable dwell-time clock. Starts at a fixed
// non-zero instant (a zero time.Time would collide with the gate's
// IsZero() "unarmed" sentinel) and only advances when the test says so.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func newManualClock() *manualClock {
	return &manualClock{t: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)}
}

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// TestMonthlyQuota_TransientBlipFailsOpen (W1-flow-register-4, scenario
// 1): a SINGLE month-to-date read error — the momentary Redis blip case
// — must still pass through (fail OPEN) so a paying customer is not
// 429'd on a transient hiccup. GREEN on both the unfixed and fixed
// middleware; it pins the "we did not over-correct into always-closed"
// invariant.
func TestMonthlyQuota_TransientBlipFailsOpen(t *testing.T) {
	clock := newManualClock()
	reader := &mutableMTDReader{err: errors.New("redis blip")}
	mw := middleware.MonthlyQuota(reader, nil, middleware.WithMonthlyQuotaClock(clock.now))
	sub := auth.Subject{Tier: auth.TierAPIKey, KeyID: "K1", MonthlyQuota: 1}

	failClosedBefore := testutil.ToFloat64(obs.MonthlyQuotaFailClosedTotal)
	failOpenBefore := testutil.ToFloat64(obs.MonthlyQuotaFailOpenTotal)

	status, _, _ := runWithSubject(t, mw, sub)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200 (single blip must fail OPEN)", status)
	}
	if got := testutil.ToFloat64(obs.MonthlyQuotaFailClosedTotal); got != failClosedBefore {
		t.Errorf("fail-closed counter moved on a transient blip: %v → %v", failClosedBefore, got)
	}
	if got, want := testutil.ToFloat64(obs.MonthlyQuotaFailOpenTotal), failOpenBefore+1; got != want {
		t.Errorf("fail-open counter = %v, want %v", got, want)
	}
}

// TestMonthlyQuota_SustainedFailureFailsClosed (W1-flow-register-4,
// scenario 2) is the PROVEN-RED assertion. Once the month-to-date
// counter has been erroring continuously for longer than the dwell
// window, the middleware must stop serving unmetered and reject with
// 429 + Retry-After — otherwise a key already at its cap bills free for
// the whole outage.
//
// On the UNFIXED middleware the read-error branch always fails open, so
// the second request below returns 200 and the `want 429` assertion
// FAILS — that is the redness this test guards.
func TestMonthlyQuota_SustainedFailureFailsClosed(t *testing.T) {
	clock := newManualClock()
	reader := &mutableMTDReader{err: errors.New("redis MISCONF")}
	mw := middleware.MonthlyQuota(reader, nil, middleware.WithMonthlyQuotaClock(clock.now))
	sub := auth.Subject{Tier: auth.TierAPIKey, KeyID: "K1", MonthlyQuota: 1_000_000}

	// First error arms the dwell clock and still falls open.
	if status, _, _ := runWithSubject(t, mw, sub); status != http.StatusOK {
		t.Fatalf("first error status = %d, want 200 (dwell clock arming, fail open)", status)
	}

	// Push time past the dwell window with the counter still erroring.
	clock.advance(middleware.DefaultMonthlyQuotaDwellTime + time.Second)

	failClosedBefore := testutil.ToFloat64(obs.MonthlyQuotaFailClosedTotal)
	status, headers, body := runWithSubject(t, mw, sub)
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (sustained outage past dwell must fail CLOSED)", status)
	}
	if got := headers.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30 (the dwell window)", got)
	}
	if got := headers.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if !strings.Contains(body, "monthly-quota-unavailable") {
		t.Errorf("body missing the distinct fail-closed problem type: %s", body)
	}
	if got, want := testutil.ToFloat64(obs.MonthlyQuotaFailClosedTotal), failClosedBefore+1; got != want {
		t.Errorf("stellarindex_monthly_quota_fail_closed_total = %v, want %v", got, want)
	}
}

// TestMonthlyQuota_RecoversToNormalMetering (W1-flow-register-4,
// scenario 3): after the counter recovers for a full dwell window of
// unbroken success, the middleware returns to ordinary metering — an
// under-cap read passes, an over-cap read gets the normal
// quota-exceeded 429, and a fresh blip fails OPEN again (proving the
// dwell clock reset rather than latching closed forever).
func TestMonthlyQuota_RecoversToNormalMetering(t *testing.T) {
	clock := newManualClock()
	reader := &mutableMTDReader{err: errors.New("redis down")}
	mw := middleware.MonthlyQuota(reader, nil, middleware.WithMonthlyQuotaClock(clock.now))
	sub := auth.Subject{Tier: auth.TierAPIKey, KeyID: "K1", MonthlyQuota: 100}

	// Drive into the fail-closed state.
	if status, _, _ := runWithSubject(t, mw, sub); status != http.StatusOK {
		t.Fatalf("arming error status = %d, want 200", status)
	}
	clock.advance(middleware.DefaultMonthlyQuotaDwellTime + time.Second)
	if status, _, _ := runWithSubject(t, mw, sub); status != http.StatusTooManyRequests {
		t.Fatalf("past-dwell error status = %d, want 429", status)
	}

	// Counter recovers under cap. The first success already meters
	// normally (200); the dwell clock has not yet fully cleared.
	reader.heal(5)
	if status, _, _ := runWithSubject(t, mw, sub); status != http.StatusOK {
		t.Fatalf("first healthy read status = %d, want 200 (metering resumes immediately)", status)
	}
	// A full dwell window of UNBROKEN success clears the fail-closed clock.
	clock.advance(middleware.DefaultMonthlyQuotaDwellTime)
	if status, _, _ := runWithSubject(t, mw, sub); status != http.StatusOK {
		t.Fatalf("recovered healthy read status = %d, want 200", status)
	}

	// Normal metering: an over-cap read now returns the ordinary
	// quota-exceeded 429 (distinct from the fail-closed "unavailable").
	reader.heal(100)
	status, headers, body := runWithSubject(t, mw, sub)
	if status != http.StatusTooManyRequests {
		t.Fatalf("over-cap read status = %d, want 429", status)
	}
	if got := headers.Get("X-StellarIndex-Monthly-Used"); got != "100" {
		t.Errorf("X-StellarIndex-Monthly-Used = %q, want 100 (normal metering)", got)
	}
	if !strings.Contains(body, "monthly-quota-exceeded") {
		t.Errorf("expected the normal over-cap problem type, got: %s", body)
	}

	// A fresh blip after recovery must fail OPEN again — the dwell clock
	// reset, so a single new error is treated as transient, not latched.
	reader.fail(errors.New("new blip"))
	if status, _, _ := runWithSubject(t, mw, sub); status != http.StatusOK {
		t.Errorf("post-recovery blip status = %d, want 200 (dwell clock must have reset)", status)
	}
}
