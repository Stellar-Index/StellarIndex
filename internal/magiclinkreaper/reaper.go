// Package magiclinkreaper bounds the `magic_link_tokens` table
// (migration 0027) that the dashboard magic-link / email-code sign-in
// flow writes to.
//
// # Why this exists (PRV-2)
//
// `magic_link_tokens` is durable plaintext PII — it stores the
// requester's email (citext) and requested_ip (inet). Its key is
// ATTACKER-CHOSEN and the write path is UNAUTHENTICATED:
//
//	POST /v1/auth/login {"email":"<random>@example.com"}
//	  → CreateMagicLinkToken
//	  → INSERT INTO magic_link_tokens (email = <random>@example.com, requested_ip = <caller>)
//
// A row is removed only by being consumed (a successful click of the
// emailed link), which never happens for an address the caller does
// not own — nobody clicks the link. The only bound was the anonymous
// per-IP rate limit, which on a disk-fixed host makes this a slow,
// cheap, remote table-fill whose first alarm would otherwise be the
// volume-level disk page. The sibling `login_code_lockouts` table
// (C3-032) already has exactly this reaper; this table lacked one.
//
// # What is swept
//
// EXPIRED rows only:
//
//	expires_at < now() - Retention
//
// Every row past `expires_at` is TERMINAL: both ConsumeMagicLinkToken
// and ConsumableLoginCandidates require `expires_at > now`, so an
// expired row can never again be redeemed, consumed or not. Retention
// keeps expired rows a while — long enough to preserve the
// expired-vs-absent distinction classifyMagicLinkMiss draws for a slow
// user, and to retain a window of forensics on a mint flood — then
// deletes them over `magic_link_tokens_expires_idx`. A live
// (unexpired) token is never touched at any age, mirroring the
// login-code reaper's live-lock exemption.
//
// The worker is the same small ticker loop as
// internal/logincodereaper (sweep immediately, then every Interval),
// runs in the API binary, and is bounded to the process root context.
//
// # Not operator-tunable
//
// Deliberately no config section. Retention here is a DoS / PII
// control, and an operator disabling or lengthening it re-opens the
// hole — the same reasoning that keeps the login-code reaper
// un-toggleable. It also cannot be silently disabled by an unrelated
// toggle (e.g. the signup reaper's `enabled=false`).
package magiclinkreaper

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
	// DefaultRetention is how long an EXPIRED row is kept past its
	// expires_at. The magic-link TTL is 15 minutes
	// (dashboardauth.MagicLinkTTL), so 48 h leaves a wide margin: it
	// preserves the expired-vs-absent error distinction for a user who
	// clicks a stale link up to two days late, keeps a window of
	// forensics on a mint flood, and still holds the table flat.
	DefaultRetention = 48 * time.Hour
)

// MagicLinkStore is the reaper's narrow seam, satisfied by
// *postgresstore.TokenStore. Declared here rather than widening
// platform.TokenStore so the delete surface stays off the broad
// interface every token fake would otherwise have to implement — same
// reasoning as logincodereaper.LockoutStore.
type MagicLinkStore interface {
	// SweepExpiredMagicLinkTokens deletes rows whose expires_at is
	// before olderThan, returning the number removed. Never deletes a
	// row that is still live (unexpired).
	SweepExpiredMagicLinkTokens(ctx context.Context, olderThan time.Time) (int64, error)
	// CountMagicLinkTokens returns the current row count.
	CountMagicLinkTokens(ctx context.Context) (int64, error)
}

// Options tunes the Reaper. Zero values yield production defaults.
type Options struct {
	// Interval is the sweep cadence. <= 0 falls back to DefaultInterval.
	Interval time.Duration
	// Retention is the expired-row age threshold past expires_at. <= 0
	// falls back to DefaultRetention.
	Retention time.Duration
	Logger    *slog.Logger
	// Clock lets tests pin "now". Defaults to time.Now().UTC.
	Clock func() time.Time
}

// Reaper periodically deletes expired magic-link token rows and
// publishes the table's size.
type Reaper struct {
	store     MagicLinkStore
	interval  time.Duration
	retention time.Duration
	logger    *slog.Logger
	now       func() time.Time
}

// New builds a Reaper. Panics if store is nil (a wiring bug — the
// caller must gate construction on the Postgres token store existing).
func New(store MagicLinkStore, opts Options) *Reaper {
	if store == nil {
		panic("magiclinkreaper: New requires a non-nil store")
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
	obs.AuthReaperIntervalSeconds.WithLabelValues(obs.AuthReaperMagicLink).Set(r.interval.Seconds())
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
	r.logger.Info("magic-link-token reaper started",
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
// Errors are recorded on [obs.MagicLinkTokenErrorsTotal] and
// swallowed: this is a background janitor, and a failed sweep is
// retried next tick. The gauge is refreshed even when the DELETE failed
// — that is exactly when an operator most needs to see the row count.
func (r *Reaper) Sweep(ctx context.Context) {
	deleted, err := r.store.SweepExpiredMagicLinkTokens(ctx, r.now().Add(-r.retention))
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return // clean shutdown, not a failure
	case err != nil:
		obs.MagicLinkTokenErrorsTotal.WithLabelValues(obs.MagicLinkTokenOpSweep).Inc()
		r.logger.Warn("magic-link-token reaper: sweep failed", "err", err)
	case deleted > 0:
		obs.MagicLinkTokenRowsDeletedTotal.Add(float64(deleted))
		r.logger.Info("magic-link-token reaper: deleted expired rows", "deleted", deleted)
	}
	r.refreshGauge(ctx)
	// Liveness (#368 M5): the sweep COMPLETED — including the failure arm
	// above; only the cancelled early return skips this.
	obs.AuthReaperLastSweepUnix.WithLabelValues(obs.AuthReaperMagicLink).Set(float64(r.now().Unix()))
}

// refreshGauge publishes the current row count. A count failure is
// counted under the same `sweep` op — from an operator's point of view
// the janitor pass failed either way.
func (r *Reaper) refreshGauge(ctx context.Context) {
	n, err := r.store.CountMagicLinkTokens(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		obs.MagicLinkTokenErrorsTotal.WithLabelValues(obs.MagicLinkTokenOpSweep).Inc()
		r.logger.Warn("magic-link-token reaper: count failed", "err", err)
		return
	}
	obs.MagicLinkTokenRows.Set(float64(n))
}
