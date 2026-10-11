// Package signupreaper deletes the orphan "speculative-account" rows left by the lost-signup-race
// recovery path.
//
// Two concurrent /v1/auth/callback provisions for one email can both create an `accounts` row,
// but only one CreateUser wins on `users_email_idx`. The recovery path in
// internal/api/v1/dashboardauth marks the loser Suspended with a `signup-race:` reason, which
// this reaper matches.
//
// It sweeps once immediately, then every Interval, deleting rows Suspended with that reason,
// older than MinAge and, as a second guard, with no child users or api_keys.
package signupreaper

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// SignupRaceReasonPrefix is the `suspended_reason` prefix the recovery path stamps on the losing
// account; the reaper matches only reasons starting with this exact literal.
const SignupRaceReasonPrefix = "signup-race:"

// Defaults for a zero Options.
const (
	DefaultInterval = time.Hour
	DefaultMinAge   = 24 * time.Hour
)

// OrphanStore is the reaper's narrow write seam (*postgresstore.AccountStore), kept off
// platform.AccountStore so every account fake need not implement the delete surface.
type OrphanStore interface {
	// ReapSuspendedOrphans deletes accounts Suspended with a reason starting with reasonPrefix
	// and suspended before olderThan, returning the rows removed. The predicate is conservative.
	ReapSuspendedOrphans(ctx context.Context, reasonPrefix string, olderThan time.Time) (int64, error)
	// CountAccounts feeds [obs.AccountRows]: POST /v1/register is unauthenticated and mints a
	// permanent account per call, which the sweep never touches, so this gauge is the only view
	// of growth.
	CountAccounts(ctx context.Context) (int64, error)
	// CountAPIKeys feeds [obs.APIKeyRows]: register mints one durable key per account, so this
	// tracks the same growth from the credential side.
	CountAPIKeys(ctx context.Context) (int64, error)
}

// Options tunes the Reaper. Zero values yield production defaults.
type Options struct {
	// Interval is the sweep cadence. <= 0 falls back to DefaultInterval.
	Interval time.Duration
	// MinAge is how long an orphan must have been suspended before reaping, a safety window
	// well past any in-flight signup race. <= 0 falls back to DefaultMinAge.
	MinAge time.Duration
	Logger *slog.Logger
	// Clock lets tests pin "now". Defaults to time.Now().UTC.
	Clock func() time.Time
}

// Reaper periodically deletes signup-race speculative-account orphans.
type Reaper struct {
	store    OrphanStore
	interval time.Duration
	minAge   time.Duration
	logger   *slog.Logger
	now      func() time.Time
}

// New builds a Reaper. Panics if store is nil: a wiring bug, since the caller must gate
// construction on the account store being present.
func New(store OrphanStore, opts Options) *Reaper {
	if store == nil {
		panic("signupreaper: New requires a non-nil store")
	}
	r := &Reaper{
		store:    store,
		interval: opts.Interval,
		minAge:   opts.MinAge,
		logger:   opts.Logger,
		now:      opts.Clock,
	}
	if r.interval <= 0 {
		r.interval = DefaultInterval
	}
	obs.AuthReaperIntervalSeconds.WithLabelValues(obs.AuthReaperSignup).Set(r.interval.Seconds())
	if r.minAge <= 0 {
		r.minAge = DefaultMinAge
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
// immediately, then every Interval. Returns ctx.Err() on cancellation.
func (r *Reaper) Run(ctx context.Context) error {
	tick := time.NewTicker(r.interval)
	defer tick.Stop()
	r.logger.Info("signup-reaper started",
		"interval", r.interval, "min_age", r.minAge)
	for {
		r.Sweep(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// Sweep runs one reap pass and records the paired metric. Errors are swallowed after being
// recorded (best-effort worker); the outcome label is what the alert rule reads.
func (r *Reaper) Sweep(ctx context.Context) {
	start := r.now()
	deleted, outcome := r.sweepOnce(ctx)
	obs.SignupReaperRunsTotal.WithLabelValues(outcome).Inc()
	obs.SignupReaperRunDurationSeconds.WithLabelValues(outcome).
		Observe(time.Since(start).Seconds())
	if deleted > 0 {
		obs.SignupReaperRowsDeletedTotal.Add(float64(deleted))
		r.logger.Info("signup-reaper deleted speculative-account orphans",
			"deleted", deleted)
	}
	r.refreshRowGauges(ctx)
	// Liveness: the sweep COMPLETED — including the failure arm
	// above; only the cancelled early return skips this.
	obs.AuthReaperLastSweepUnix.WithLabelValues(obs.AuthReaperSignup).Set(float64(r.now().Unix()))
}

// refreshRowGauges publishes the accounts/api_keys row counts. Unauthenticated POST /v1/register
// grows both and this reaper bounds neither, so they are the only registration-volume signal.
// A count failure is logged, not a sweep failure: the delete's own outcome drives the alert.
func (r *Reaper) refreshRowGauges(ctx context.Context) {
	if n, err := r.store.CountAccounts(ctx); err != nil {
		if !errors.Is(err, context.Canceled) {
			r.logger.Warn("signup-reaper: count accounts failed", "err", err)
		}
	} else {
		obs.AccountRows.Set(float64(n))
	}
	if n, err := r.store.CountAPIKeys(ctx); err != nil {
		if !errors.Is(err, context.Canceled) {
			r.logger.Warn("signup-reaper: count api_keys failed", "err", err)
		}
	} else {
		obs.APIKeyRows.Set(float64(n))
	}
}

// sweepOnce performs the delete and returns (rows deleted, outcome). Cancellation reports "ok"
// (clean shutdown, not a failure), matching the pricealerts worker.
func (r *Reaper) sweepOnce(ctx context.Context) (int64, string) {
	olderThan := r.now().Add(-r.minAge)
	deleted, err := r.store.ReapSuspendedOrphans(ctx, SignupRaceReasonPrefix, olderThan)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return 0, "ok"
		}
		r.logger.Warn("signup-reaper: reap failed", "err", err)
		return 0, "error"
	}
	return deleted, "ok"
}
