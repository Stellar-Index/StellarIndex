package timescale

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestCAGGRefreshTimeout_DerivedFromWindow pins the W8-19 bound: five
// minutes of refresh per hour of window, floored at ten minutes and
// capped at four hours. The rows are the windows the backfill actually
// asks for — a 10k-ledger chunk (~4h), the documented `-parallel 4`
// weekly slice, the SDEX history driver's 40k-ledger chunk (~55h) and
// the padded MinWindow of each coarse rung — so a change to any
// constant shows up as a changed expectation here, not as a surprise
// on r1.
func TestCAGGRefreshTimeout_DerivedFromWindow(t *testing.T) {
	cases := []struct {
		name   string
		window time.Duration
		want   time.Duration
	}{
		{"zero window gets the floor", 0, CAGGRefreshTimeoutFloor},
		{"negative window gets the floor", -time.Hour, CAGGRefreshTimeoutFloor},
		{"prices_1m MinWindow (2m) is floored", 2 * time.Minute, CAGGRefreshTimeoutFloor},
		{"prices_15m MinWindow (30m) is floored", 30 * time.Minute, CAGGRefreshTimeoutFloor},
		{"one hour computes below the floor", time.Hour, CAGGRefreshTimeoutFloor},
		{"exactly two hours is the floor by arithmetic", 2 * time.Hour, 10 * time.Minute},
		{"prices_1h MinWindow (3h)", 3 * time.Hour, 15 * time.Minute},
		{"a 10k-ledger chunk (~4h)", 4 * time.Hour, 20 * time.Minute},
		{"prices_4h MinWindow (12h)", 12 * time.Hour, time.Hour},
		{"one day", 24 * time.Hour, 2 * time.Hour},
		{"a 40k-ledger SDEX history chunk (~55h)", 55 * time.Hour, CAGGRefreshTimeoutCeiling},
		{"prices_1d MinWindow (3d) is capped", 3 * 24 * time.Hour, CAGGRefreshTimeoutCeiling},
		{"a weekly slice is capped", 7 * 24 * time.Hour, CAGGRefreshTimeoutCeiling},
		{"prices_1mo MinWindow (93d) is capped", 93 * 24 * time.Hour, CAGGRefreshTimeoutCeiling},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CAGGRefreshTimeout(tc.window); got != tc.want {
				t.Fatalf("CAGGRefreshTimeout(%s) = %s, want %s", tc.window, got, tc.want)
			}
		})
	}
	// The constants themselves, so a drive-by edit to one of them is a
	// deliberate change to this file too.
	if CAGGRefreshTimeoutPerWindowHour != 5*time.Minute {
		t.Errorf("CAGGRefreshTimeoutPerWindowHour = %s, want 5m (12x real time)", CAGGRefreshTimeoutPerWindowHour)
	}
	if CAGGRefreshTimeoutFloor != 10*time.Minute {
		t.Errorf("CAGGRefreshTimeoutFloor = %s, want 10m", CAGGRefreshTimeoutFloor)
	}
	if CAGGRefreshTimeoutCeiling != 4*time.Hour {
		t.Errorf("CAGGRefreshTimeoutCeiling = %s, want 4h", CAGGRefreshTimeoutCeiling)
	}
	if CAGGRefreshTimeoutFloor >= CAGGRefreshTimeoutCeiling {
		t.Errorf("floor %s must be below ceiling %s", CAGGRefreshTimeoutFloor, CAGGRefreshTimeoutCeiling)
	}
}

// TestCAGGRefreshTimeout_EveryLiveRungIsBounded: every view the
// backfill walks, at its own MinWindow, gets a bound inside
// [floor, ceiling] — no rung computes to zero or escapes the cap.
func TestCAGGRefreshTimeout_EveryLiveRungIsBounded(t *testing.T) {
	for _, spec := range CAGGsLiveForever {
		got := CAGGRefreshTimeout(spec.MinWindow)
		if got < CAGGRefreshTimeoutFloor || got > CAGGRefreshTimeoutCeiling {
			t.Errorf("%s: CAGGRefreshTimeout(MinWindow=%s) = %s, outside [%s, %s]",
				spec.Name, spec.MinWindow, got, CAGGRefreshTimeoutFloor, CAGGRefreshTimeoutCeiling)
		}
	}
}

// TestCAGGRefreshTimeoutError_NamesViewAndWindow: the typed error the
// caller logs carries the view, the window and the bound in its
// message, and unwraps to the driver error so SQLSTATE matching on the
// chain still works.
func TestCAGGRefreshTimeoutError_NamesViewAndWindow(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(4 * time.Hour)
	cause := &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}
	var err error = &CAGGRefreshTimeoutError{View: "prices_1m", From: from, To: to, Timeout: 20 * time.Minute, Err: cause}

	msg := err.Error()
	for _, want := range []string{"prices_1m", "2026-09-01T00:00:00Z", "2026-09-01T04:00:00Z", "20m0s", "57014"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message lacks %q: %s", want, msg)
		}
	}
	var tErr *CAGGRefreshTimeoutError
	if !errors.As(err, &tErr) || tErr.View != "prices_1m" {
		t.Fatalf("errors.As(*CAGGRefreshTimeoutError) failed on %v", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("typed error must unwrap to the driver's PgError; got %v", err)
	}
	if !isStatementTimeoutErr(fmt.Errorf("wrapped: %w", cause)) {
		t.Error("isStatementTimeoutErr must see 57014 through a wrap")
	}
	// The shape a real refresh_continuous_aggregate produces: the
	// procedure re-throws the cancellation as internal_error, so the
	// code is XX000 and only the message says what happened (observed
	// on timescaledb 2.26.4-pg15 by the integration test).
	if !isStatementTimeoutErr(&pgconn.PgError{Code: "XX000", Message: "canceling statement due to statement timeout"}) {
		t.Error("isStatementTimeoutErr must match the XX000 + message shape the refresh procedure emits")
	}
	if isStatementTimeoutErr(&pgconn.PgError{Code: "XX000", Message: "some other internal error"}) {
		t.Error("isStatementTimeoutErr must not match an unrelated XX000")
	}
	if isStatementTimeoutErr(&pgconn.PgError{Code: "55P03"}) {
		t.Error("isStatementTimeoutErr must not match 55P03 (concurrent refresh) — that one is retried")
	}
	if isStatementTimeoutErr(context.DeadlineExceeded) {
		t.Error("isStatementTimeoutErr is the SQL-side arm only; the ctx arm is matched separately")
	}
}

// TestRefreshContinuousAggregateWithTimeout_RejectsNonPositive: there is
// deliberately no "0 disables the bound" arm — an unbounded refresh is
// the defect. No DB is touched: the guard runs before any connection
// is taken, so a nil-pool Store proves the ordering.
func TestRefreshContinuousAggregateWithTimeout_RejectsNonPositive(t *testing.T) {
	s := &Store{}
	now := time.Now()
	for _, timeout := range []time.Duration{0, -time.Second} {
		err := s.RefreshContinuousAggregateWithTimeout(context.Background(), "prices_1m", now.Add(-time.Hour), now, timeout)
		if err == nil || !strings.Contains(err.Error(), "non-positive timeout") {
			t.Errorf("timeout %s: err = %v, want non-positive-timeout rejection", timeout, err)
		}
	}
	if err := s.RefreshContinuousAggregateWithTimeout(context.Background(), "not_a_view", now.Add(-time.Hour), now, time.Minute); err == nil ||
		!strings.Contains(err.Error(), "unknown view") {
		t.Errorf("unknown view must still be rejected first: %v", err)
	}
}
