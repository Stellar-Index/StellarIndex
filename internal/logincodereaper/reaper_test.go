package logincodereaper_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/logincodereaper"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// C3-032 follow-up — `login_code_lockouts` retention.
//
// The lockout table's primary key is attacker-chosen (POST
// /v1/auth/verify-code is unauthenticated and takes any well-formed
// address), and rows for synthetic addresses are never cleared by the
// success path because nobody can sign in as them. Without this reaper
// the table grows without bound at whatever rate the anonymous rate
// limit allows, on a disk-fixed host.
//
// These tests pin the worker's contract. The DELETE predicate itself —
// including the rule that a LIVE lock is never reaped — is proven
// against real Postgres in test/integration/login_code_lockout_test.go.

type sweepCall struct{ olderThan time.Time }

type fakeLockoutStore struct {
	mu        sync.Mutex
	calls     []sweepCall
	deleted   int64
	rows      int64
	sweepErr  error
	countErr  error
	countCall int
}

func (f *fakeLockoutStore) SweepLoginCodeLockouts(_ context.Context, olderThan time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sweepCall{olderThan: olderThan})
	if f.sweepErr != nil {
		return 0, f.sweepErr
	}
	return f.deleted, nil
}

func (f *fakeLockoutStore) CountLoginCodeLockouts(_ context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.countCall++
	if f.countErr != nil {
		return 0, f.countErr
	}
	return f.rows, nil
}

func (f *fakeLockoutStore) lastCall(t *testing.T) sweepCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("store was never swept")
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeLockoutStore) counts(t *testing.T) int {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.countCall
}

func silent() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func sweepErrors(t *testing.T) float64 {
	t.Helper()
	return testutil.ToFloat64(
		obs.LoginCodeLockoutErrorsTotal.WithLabelValues(obs.LoginCodeLockoutOpSweep))
}

// TestSweep_DeletesAndPublishesRowCount — the happy path: the reaper
// asks for rows older than `now - retention` and publishes the surviving
// row count on the gauge that makes growth visible before a disk alert.
func TestSweep_DeletesAndPublishesRowCount(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	store := &fakeLockoutStore{deleted: 7, rows: 12}
	r := logincodereaper.New(store, logincodereaper.Options{
		Retention: 48 * time.Hour,
		Logger:    silent(),
		Clock:     func() time.Time { return now },
	})

	before := testutil.ToFloat64(obs.LoginCodeLockoutRowsDeletedTotal)
	r.Sweep(context.Background())

	if got, want := store.lastCall(t).olderThan, now.Add(-48*time.Hour); !got.Equal(want) {
		t.Errorf("olderThan = %v, want %v", got, want)
	}
	if got, want := testutil.ToFloat64(obs.LoginCodeLockoutRowsDeletedTotal), before+7; got != want {
		t.Errorf("rows_deleted_total = %v, want %v", got, want)
	}
	if got := testutil.ToFloat64(obs.LoginCodeLockoutRows); got != 12 {
		t.Errorf("login_code_lockout_rows = %v, want 12 (the gauge is the growth signal)", got)
	}
}

// TestSweep_RetentionExceedsTheCountingWindow — a sweep must never be
// able to shorten a LIVE counting window, which is what would let a
// grinder reset their durable failure count by waiting.
// dashboardauth.durableCodeFailureWindow is 24h; this pins the default
// retention above it so the two cannot drift into each other.
func TestSweep_RetentionExceedsTheCountingWindow(t *testing.T) {
	const countingWindow = 24 * time.Hour // dashboardauth.durableCodeFailureWindow
	if logincodereaper.DefaultRetention <= countingWindow {
		t.Fatalf("DefaultRetention = %s, must exceed the %s counting window — "+
			"otherwise the sweep deletes rows whose window is still accumulating",
			logincodereaper.DefaultRetention, countingWindow)
	}
}

// TestSweep_FailureIsCountedNotSilent — a janitor that stops working is
// exactly how the table starts growing again, so a failed sweep must
// leave a metric behind.
func TestSweep_FailureIsCountedNotSilent(t *testing.T) {
	store := &fakeLockoutStore{sweepErr: errors.New("deadlock detected"), rows: 40000}
	r := logincodereaper.New(store, logincodereaper.Options{Logger: silent()})

	before := sweepErrors(t)
	r.Sweep(context.Background())

	if got, want := sweepErrors(t), before+1; got != want {
		t.Errorf("login_code_lockout_errors_total{op=sweep} = %v, want %v", got, want)
	}
	// The gauge is still refreshed — a failing sweep is precisely when
	// an operator needs the row count.
	if got := testutil.ToFloat64(obs.LoginCodeLockoutRows); got != 40000 {
		t.Errorf("gauge = %v, want 40000 (refreshed even when the DELETE failed)", got)
	}
	if store.counts(t) != 1 {
		t.Errorf("count queries = %d, want 1", store.counts(t))
	}
}

// TestSweep_CountFailureIsCounted — the gauge going stale is itself a
// blind spot; it gets the same op label (one janitor pass, one signal).
func TestSweep_CountFailureIsCounted(t *testing.T) {
	store := &fakeLockoutStore{countErr: errors.New("statement timeout")}
	r := logincodereaper.New(store, logincodereaper.Options{Logger: silent()})

	before := sweepErrors(t)
	r.Sweep(context.Background())
	if got, want := sweepErrors(t), before+1; got != want {
		t.Errorf("errors_total{op=sweep} = %v, want %v", got, want)
	}
}

// TestSweep_HealthyPassIsSilentOnTheErrorCounter — or the metric is
// permanently non-zero and tells an operator nothing.
func TestSweep_HealthyPassIsSilentOnTheErrorCounter(t *testing.T) {
	store := &fakeLockoutStore{deleted: 0, rows: 3}
	r := logincodereaper.New(store, logincodereaper.Options{Logger: silent()})

	before := sweepErrors(t)
	r.Sweep(context.Background())
	if got := sweepErrors(t); got != before {
		t.Errorf("errors_total{op=sweep} moved on a healthy sweep: %v → %v", before, got)
	}
}

// TestSweep_ShutdownIsNotAFailure — a cancelled context during graceful
// shutdown must not be recorded as a janitor failure (it would page for
// every deploy).
func TestSweep_ShutdownIsNotAFailure(t *testing.T) {
	store := &fakeLockoutStore{sweepErr: context.Canceled}
	r := logincodereaper.New(store, logincodereaper.Options{Logger: silent()})

	before := sweepErrors(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Sweep(ctx)
	if got := sweepErrors(t); got != before {
		t.Errorf("errors_total{op=sweep} moved on a cancelled context: %v → %v", before, got)
	}
}

// TestRun_SweepsImmediately — a process that has just started may be
// inheriting a table that grew while it was down; waiting a full
// interval to find out is the wrong default.
func TestRun_SweepsImmediately(t *testing.T) {
	store := &fakeLockoutStore{}
	r := logincodereaper.New(store, logincodereaper.Options{
		Interval: time.Hour, Logger: silent(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Run(ctx)
	}()

	deadline := time.After(2 * time.Second)
	for {
		store.mu.Lock()
		n := len(store.calls)
		store.mu.Unlock()
		if n > 0 {
			cancel()
			<-done
			return
		}
		select {
		case <-deadline:
			t.Fatal("Run did not sweep within 2s of starting")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestNew_DefaultsAreProduction — a zero Options must not yield a
// zero interval (a hot loop) or a zero retention (deleting everything,
// including live locks' rows the moment they settle).
func TestNew_DefaultsAreProduction(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	store := &fakeLockoutStore{}
	r := logincodereaper.New(store, logincodereaper.Options{
		Logger: silent(),
		Clock:  func() time.Time { return now },
	})
	r.Sweep(context.Background())
	if got, want := store.lastCall(t).olderThan, now.Add(-logincodereaper.DefaultRetention); !got.Equal(want) {
		t.Errorf("olderThan = %v, want %v (DefaultRetention must apply to a zero Options)", got, want)
	}
}

// TestSweep_MarksLiveness pins #368 M5: every COMPLETED sweep — including a
// failed one — stamps the liveness gauge, and construction publishes the
// configured interval the stalled alert scales its threshold by. A dead
// reaper is otherwise invisible: its rows gauge just freezes at a
// healthy-looking number.
func TestSweep_MarksLiveness(t *testing.T) {
	g := obs.AuthReaperLastSweepUnix.WithLabelValues(obs.AuthReaperLoginCode)
	start := float64(time.Now().Unix())

	logincodereaper.New(&fakeLockoutStore{}, logincodereaper.Options{Logger: silent()}).Sweep(context.Background())
	if got := testutil.ToFloat64(g); got < start {
		t.Fatalf("liveness gauge after ok sweep = %v, want >= %v", got, start)
	}
	if iv := testutil.ToFloat64(obs.AuthReaperIntervalSeconds.WithLabelValues(obs.AuthReaperLoginCode)); iv != logincodereaper.DefaultInterval.Seconds() {
		t.Fatalf("interval gauge = %v, want %v", iv, logincodereaper.DefaultInterval.Seconds())
	}

	// Failing is not dead: the errors counter reports the failure, the
	// liveness gauge reports the reaper is still running.
	g.Set(0)
	failing := &fakeLockoutStore{sweepErr: errors.New("deadlock detected")}
	logincodereaper.New(failing, logincodereaper.Options{Logger: silent()}).Sweep(context.Background())
	if got := testutil.ToFloat64(g); got < start {
		t.Fatalf("liveness gauge after failed sweep = %v, want >= %v (failed != dead)", got, start)
	}

	// A cancelled sweep is the reaper GOING AWAY — it must not read as alive.
	cancelled := &fakeLockoutStore{sweepErr: context.Canceled}
	before := testutil.ToFloat64(g)
	logincodereaper.New(cancelled, logincodereaper.Options{Logger: silent()}).Sweep(context.Background())
	if got := testutil.ToFloat64(g); got != before {
		t.Fatalf("cancelled sweep advanced liveness gauge: %v -> %v", before, got)
	}
}
