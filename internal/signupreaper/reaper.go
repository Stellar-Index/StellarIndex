// Package signupreaper deletes the orphan "speculative-account" rows
// that the F-1255 lost-signup-race recovery path leaves behind.
//
// Background (F-1255, codex audit-2026-05-12): two concurrent
// /v1/auth/callback provisions for the same just-verified email can
// both pass the GetUserByEmail check and both create an `accounts`
// row, but only one CreateUser wins on the `users_email_idx` unique
// index. The loser's account has no user attached and never will; the
// recovery path in internal/api/v1/dashboardauth marks it Suspended
// with a `signup-race:` reason so it carries an unambiguous,
// machine-matchable signal instead of accumulating as a
// never-recovered row. This package is the reaper that consumes that
// signal.
//
// The worker is a small ticker loop (usage.Rollup / pricealerts.Worker
// shape): sweep once immediately, then every Interval, deleting rows
// that are Suspended with the signup-race reason, older than MinAge,
// and — belt-and-suspenders — have no child users or api_keys. It runs
// in the API binary alongside the customer-webhook + usage-rollup
// workers and is bounded to the process's root context for graceful
// shutdown.
package signupreaper

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// SignupRaceReasonPrefix is the `suspended_reason` prefix the F-1255
// recovery path stamps on the losing account
// ("signup-race: orphan speculative account <email>"). The reaper
// matches only rows whose reason starts with this exact literal.
const SignupRaceReasonPrefix = "signup-race:"

// Defaults for a zero Options.
const (
	DefaultInterval = time.Hour
	DefaultMinAge   = 24 * time.Hour
)

// OrphanStore is the reaper's narrow write seam. Satisfied by
// *postgresstore.AccountStore. Declared here (rather than widening
// platform.AccountStore) so the delete surface stays off the broad
// interface every account fake would otherwise have to implement.
type OrphanStore interface {
	// ReapSuspendedOrphans deletes accounts Suspended with a reason
	// starting with reasonPrefix and suspended before olderThan,
	// returning the number of rows removed. Conservative predicate —
	// see the implementation.
	ReapSuspendedOrphans(ctx context.Context, reasonPrefix string, olderThan time.Time) (int64, error)
	// CountAccounts returns the current row count of the accounts
	// table. Unlike ReapSuspendedOrphans this is not a delete surface —
	// it exists so the reaper's sweep can publish [obs.AccountRows]:
	// POST /v1/register (ADR-0049) is unauthenticated and mints a
	// permanent account on every accepted call, and the reaper's own
	// sweep never touches those rows (only signup-race orphans), so
	// this gauge is the only visibility into that table's growth.
	CountAccounts(ctx context.Context) (int64, error)
	// CountAPIKeys returns the current row count of the api_keys
	// table, published as [obs.APIKeyRows]. Register mints one durable
	// key per account (mintRegisterKey), so this tracks the same
	// growth from the credential side.
	CountAPIKeys(ctx context.Context) (int64, error)
}

// Options tunes the Reaper. Zero values yield production defaults.
type Options struct {
	// Interval is the sweep cadence. <= 0 falls back to DefaultInterval.
	Interval time.Duration
	// MinAge is how long an orphan must have been suspended before it
	// is eligible for reaping — a safety window well past any in-flight
	// signup race. <= 0 falls back to DefaultMinAge.
	MinAge time.Duration
	Logger *slog.Logger
	// Clock lets tests pin "now". Defaults to time.Now().UTC.
	Clock func() time.Time
}

// Reaper periodically deletes F-1255 speculative-account orphans.
type Reaper struct {
	store    OrphanStore
	interval time.Duration
	minAge   time.Duration
	logger   *slog.Logger
	now      func() time.Time
}

// New builds a Reaper. Panics if store is nil (a wiring bug — the
// caller must gate construction on the platform account store being
// present).
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

// Sweep runs one reap pass and records the paired metric. Exported so
// tests can drive a single pass deterministically. Errors are
// swallowed after being recorded (best-effort background worker) — the
// outcome is what the metric + alert rule read.
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
	// Liveness (#368 M5): the sweep COMPLETED — including the failure arm
	// above; only the cancelled early return skips this.
	obs.AuthReaperLastSweepUnix.WithLabelValues(obs.AuthReaperSignup).Set(float64(r.now().Unix()))
}

// refreshRowGauges publishes the current accounts/api_keys row counts
// (obs.AccountRows / obs.APIKeyRows). Both tables are durably written
// by the unauthenticated, friction-free POST /v1/register path
// (ADR-0049) and neither is bounded by this reaper — it deletes only
// signup-race orphans — so this is the only per-deployment signal of
// registration volume. A count failure is logged, not treated as a
// sweep failure: the delete's own outcome already drives the alert.
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

// sweepOnce performs the delete and returns (rows deleted, outcome).
// A context cancellation is reported as "ok" (clean shutdown, not a
// failure) matching the pricealerts worker convention.
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
