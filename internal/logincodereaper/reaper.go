// Package logincodereaper bounds the `login_code_lockouts` table.
//
// The lockout counter is keyed by an attacker-chosen email on the unauthenticated
// `POST /v1/auth/verify-code`, so every synthetic address inserts a row that no
// successful sign-in will ever clear: a cheap remote table-fill bounded only by the
// per-IP rate limit. Gating the insert on live tokens would let an address prober
// dodge the durable counter, so the insert stays unconditional and the table gets
// retention instead.
//
// Only settled rows are swept (updated_at < now()-Retention and no lock in force).
// A live lock is never reaped; [DefaultRetention] outlasts the counting window, so
// a swept row's next failure would have restarted at 1 anyway. A capped pass
// re-sweeps after [DefaultDrainPause] because one IP can out-insert the hourly cap.
//
// Deliberately not operator-tunable: retention is a DoS control, and neither config
// nor the signup reaper's `enabled=false` may switch it off.
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
	// DefaultInterval is the sweep cadence. Hourly is cheap: bounded
	// indexed range DELETEs plus one count.
	DefaultInterval = time.Hour
	// DefaultDrainPause is the gap between passes while a sweep keeps
	// hitting the store's per-call cap. The insert rate is
	// attacker-driven, so a capped pass must not wait a full Interval:
	// that would bound the drain rate below the insert rate.
	DefaultDrainPause = 5 * time.Second
	// DefaultDrainPause is the gap between passes that hit the store's cap; the
	// insert rate is attacker-driven, so waiting a full Interval would lose the race.
	DefaultRetention = 48 * time.Hour
)

// DefaultRetention MUST stay longer than dashboardauth.durableCodeFailureWindow
// (24 h) so a sweep never shortens a live counting window; 48 h keeps a day of forensics.
type LockoutStore interface {
	// LockoutStore is the reaper's narrow seam, kept off platform.TokenStore so token
	// fakes need not implement deletes (as signupreaper.OrphanStore).
	SweepLoginCodeLockouts(ctx context.Context, olderThan time.Time) (int64, bool, error)
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
	// DrainPause is the gap before re-sweeping after a capped pass.
	// <= 0 falls back to DefaultDrainPause.
	DrainPause time.Duration
	Logger     *slog.Logger
	// Clock lets tests pin "now". Defaults to time.Now().UTC.
	Clock func() time.Time
}

// Reaper periodically deletes settled login-code lockout rows and
// publishes the table's size.
type Reaper struct {
	store      LockoutStore
	interval   time.Duration
	retention  time.Duration
	drainPause time.Duration
	logger     *slog.Logger
	now        func() time.Time
}

// New builds a Reaper. Panics if store is nil (a wiring bug — the
// caller must gate construction on the Postgres token store existing).
func New(store LockoutStore, opts Options) *Reaper {
	if store == nil {
		panic("logincodereaper: New requires a non-nil store")
	}
	r := &Reaper{
		store:      store,
		interval:   opts.Interval,
		retention:  opts.Retention,
		drainPause: opts.DrainPause,
		logger:     opts.Logger,
		now:        opts.Clock,
	}
	if r.interval <= 0 {
		r.interval = DefaultInterval
	}
	obs.AuthReaperIntervalSeconds.WithLabelValues(obs.AuthReaperLoginCode).Set(r.interval.Seconds())
	if r.retention <= 0 {
		r.retention = DefaultRetention
	}
	if r.drainPause <= 0 {
		r.drainPause = DefaultDrainPause
	}
	if r.logger == nil {
		r.logger = slog.Default()
	}
	if r.now == nil {
		r.now = func() time.Time { return time.Now().UTC() }
	}
	return r
}

// Run sweeps immediately (the table may have grown while the process was down),
// then every Interval, or after DrainPause while a pass reports a backlog.
func (r *Reaper) Run(ctx context.Context) error {
	r.logger.Info("login-code-lockout reaper started",
		"interval", r.interval, "retention", r.retention)
	for {
		wait := r.interval
		if r.Sweep(ctx) {
			wait = r.drainPause
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Sweep runs one pass, refreshes the row-count gauge, and reports whether the store
// stopped at its cap. Errors are counted and retried next tick; the gauge still
// refreshes after a failed DELETE, when the row count matters most.
func (r *Reaper) Sweep(ctx context.Context) bool {
	deleted, more, err := r.store.SweepLoginCodeLockouts(ctx, r.now().Add(-r.retention))
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false // clean shutdown, not a failure
	case err != nil:
		obs.LoginCodeLockoutErrorsTotal.WithLabelValues(obs.LoginCodeLockoutOpSweep).Inc()
		r.logger.Warn("login-code-lockout reaper: sweep failed", "err", err)
	case deleted > 0:
		obs.LoginCodeLockoutRowsDeletedTotal.Add(float64(deleted))
		r.logger.Info("login-code-lockout reaper: deleted settled rows", "deleted", deleted)
	}
	r.refreshGauge(ctx)
	// Liveness: the sweep COMPLETED — including the failure arm
	// above; only the cancelled early return skips this.
	obs.AuthReaperLastSweepUnix.WithLabelValues(obs.AuthReaperLoginCode).Set(float64(r.now().Unix()))
	return err == nil && more
}

// refreshGauge publishes the row count; a count failure counts under the `sweep`
// op, since to an operator the janitor pass failed either way.
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
