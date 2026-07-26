// Package logincodereaper bounds the `login_code_lockouts` table
// (migration 0122) that the C3-032 durable login-code lockout writes to.
//
// # Why this exists
//
// The lockout counter is keyed by EMAIL, and that key is
// attacker-chosen. `POST /v1/auth/verify-code` is unauthenticated and
// accepts any well-formed address, so:
//
//	POST /v1/auth/verify-code {"email":"<random>@example.com","code":"000000"}
//	  → no lockout row              → not locked, proceed
//	  → no consumable candidates    → empty slice, nil error
//	  → no match                    → registerFailedLoginCode
//	  → INSERT INTO login_code_lockouts (email = <random>@example.com)
//
// That row is permanent. `ClearLoginCodeLockout` only fires on a
// SUCCESSFUL sign-in for that exact address, which can never happen for
// a synthetic one — nobody owns it. The only bound was the anonymous
// per-IP rate limit (default 60/min, and 0 is an accepted config value),
// which on a disk-fixed host is a slow, cheap, remote table-fill whose
// first alarm would otherwise be the volume-level disk page.
//
// Gating the INSERT on "this address has live tokens" would fix the fill
// and re-open the finding: a grinder targeting a REAL address always has
// live tokens, but one probing for valid addresses would dodge the
// durable counter entirely, and the counter's whole purpose is to bound
// guessing across mints. So the insert stays unconditional and the
// TABLE gets a retention policy instead.
//
// # What is swept
//
// SETTLED rows only:
//
//	updated_at < now() - Retention  AND  (locked_until IS NULL OR locked_until <= now())
//
// A live lock is never reaped at any age — that is the load-bearing
// half. Reaping the rest costs nothing: [DefaultRetention] is longer
// than the counting window in
// `dashboardauth.durableCodeFailureWindow`, so any row old enough to
// sweep has already elapsed its window and its next failure would have
// restarted the count from 1 regardless.
//
// The worker is the same small ticker loop as internal/signupreaper
// (sweep immediately, then every Interval), runs in the API binary, and
// is bounded to the process root context.
//
// # Not operator-tunable
//
// Deliberately no config section, unlike the signup reaper. Retention
// here is a DoS control, and an operator disabling or lengthening it
// re-opens the hole — the same reasoning that keeps
// `dashboardauth.maxDurableCodeFailures` a constant. It also cannot be
// silently disabled by an unrelated toggle: the signup reaper's
// `enabled=false` must not switch this off too.
package logincodereaper

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Defaults for a zero Options.
const (
	// DefaultInterval is the sweep cadence. Hourly is far more often
	// than needed to hold the table flat, and cheap: one indexed range
	// DELETE plus one count.
	DefaultInterval = time.Hour
	// DefaultRetention is how long a settled row is kept. MUST stay
	// longer than dashboardauth.durableCodeFailureWindow (24 h) so a
	// sweep can never shorten a live counting window; 48 h leaves a
	// full window of slack and keeps a day of forensics on a grinder
	// who has stopped.
	DefaultRetention = 48 * time.Hour
)

// LockoutStore is the reaper's narrow seam, satisfied by
// *postgresstore.TokenStore. Declared here rather than widening
// platform.TokenStore so the delete surface stays off the broad
// interface every token fake would otherwise have to implement — same
// reasoning as signupreaper.OrphanStore.
type LockoutStore interface {
	// SweepLoginCodeLockouts deletes settled rows whose updated_at is
	// before olderThan, returning the number removed. Never deletes a
	// row whose lock is still in force.
	SweepLoginCodeLockouts(ctx context.Context, olderThan time.Time) (int64, error)
	// CountLoginCodeLockouts returns the current row count.
	CountLoginCodeLockouts(ctx context.Context) (int64, error)
}

// Options tunes the Reaper. Zero values yield production defaults.
type Options struct {
	// Interval is the sweep cadence. <= 0 falls back to DefaultInterval.
	Interval time.Duration
	// Retention is the settled-row age threshold. <= 0 falls back to
	// DefaultRetention.
	Retention time.Duration
	Logger    *slog.Logger
	// Clock lets tests pin "now". Defaults to time.Now().UTC.
	Clock func() time.Time
}

// Reaper periodically deletes settled login-code lockout rows and
// publishes the table's size.
type Reaper struct {
	store     LockoutStore
	interval  time.Duration
	retention time.Duration
	logger    *slog.Logger
	now       func() time.Time
}

// New builds a Reaper. Panics if store is nil (a wiring bug — the
// caller must gate construction on the Postgres token store existing).
func New(store LockoutStore, opts Options) *Reaper {
	if store == nil {
		panic("logincodereaper: New requires a non-nil store")
	}
	r := &Reaper{
		store:     store,
		interval:  opts.Interval,
		retention: opts.Retention,
		logger:    opts.Logger,
		now:       opts.Clock,
	}
	if r.interval <= 0 {
		r.interval = DefaultInterval
	}
	if r.retention <= 0 {
		r.retention = DefaultRetention
	}
	if r.logger == nil {
		r.logger = slog.Default()
	}
	if r.now == nil {
		r.now = func() time.Time { return time.Now().UTC() }
	}
	return r
}

// Run drives the sweep loop until ctx is cancelled. Sweeps once
// immediately — a process that has just started may be inheriting a
// table that grew while it was down — then every Interval.
func (r *Reaper) Run(ctx context.Context) error {
	tick := time.NewTicker(r.interval)
	defer tick.Stop()
	r.logger.Info("login-code-lockout reaper started",
		"interval", r.interval, "retention", r.retention)
	for {
		r.Sweep(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// Sweep runs one retention pass and refreshes the row-count gauge.
// Exported so tests can drive a single pass deterministically.
//
// Errors are recorded on [obs.LoginCodeLockoutErrorsTotal] and
// swallowed: this is a background janitor, and a failed sweep is
// retried next tick. The gauge is refreshed even when the DELETE failed
// — that is exactly when an operator most needs to see the row count.
func (r *Reaper) Sweep(ctx context.Context) {
	deleted, err := r.store.SweepLoginCodeLockouts(ctx, r.now().Add(-r.retention))
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return // clean shutdown, not a failure
	case err != nil:
		obs.LoginCodeLockoutErrorsTotal.WithLabelValues(obs.LoginCodeLockoutOpSweep).Inc()
		r.logger.Warn("login-code-lockout reaper: sweep failed", "err", err)
	case deleted > 0:
		obs.LoginCodeLockoutRowsDeletedTotal.Add(float64(deleted))
		r.logger.Info("login-code-lockout reaper: deleted settled rows", "deleted", deleted)
	}
	r.refreshGauge(ctx)
}

// refreshGauge publishes the current row count. A count failure is
// counted under the same `sweep` op — from an operator's point of view
// the janitor pass failed either way, and splitting it into a fourth
// label would add a series nobody queries separately.
func (r *Reaper) refreshGauge(ctx context.Context) {
	n, err := r.store.CountLoginCodeLockouts(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		obs.LoginCodeLockoutErrorsTotal.WithLabelValues(obs.LoginCodeLockoutOpSweep).Inc()
		r.logger.Warn("login-code-lockout reaper: count failed", "err", err)
		return
	}
	obs.LoginCodeLockoutRows.Set(float64(n))
}
