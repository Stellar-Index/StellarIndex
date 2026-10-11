// Package magiclinkreaper bounds the `magic_link_tokens` table (migration 0027) that the dashboard
// magic-link / email-code sign-in flow writes to.
//
// # Why this exists
//
// The table holds plaintext PII (requester email and IP), its key is ATTACKER-CHOSEN, and the
// write path (POST /v1/auth/login) is UNAUTHENTICATED. A row is removed only by being consumed,
// which never happens for an address the caller does not own. Without this reaper the only bound
// is the anonymous per-IP rate limit, a slow, cheap, remote table-fill whose first alarm would be
// the volume-level disk page. internal/logincodereaper does the same for `login_code_lockouts`.
//
// # What is swept
//
// EXPIRED rows only (`expires_at < now() - Retention`). A row past `expires_at` is terminal:
// ConsumeMagicLinkToken and ReserveLoginCode both require `expires_at > now`. Retention keeps
// expired rows long enough to preserve the expired-vs-absent distinction classifyMagicLinkMiss
// draws for a slow user and a forensics window on a mint flood, then deletes them over
// `magic_link_tokens_expires_idx`. A live token is never touched at any age.
//
// # Not operator-tunable
//
// There is deliberately no config section. Retention is a DoS / PII control, so an operator
// disabling or lengthening it re-opens the hole, and an unrelated toggle must not be able to
// disable it silently.
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
	// DefaultInterval is the sweep cadence; hourly holds the table flat for one indexed range
	// DELETE plus one count.
	DefaultInterval = time.Hour
	// DefaultRetention is how long an EXPIRED row is kept past expires_at: long enough for the
	// expired-vs-absent error and a forensics window on a mint flood.
	DefaultRetention = 48 * time.Hour
)

// MagicLinkStore is the reaper's narrow seam, satisfied by *postgresstore.TokenStore. It is
// declared here so the delete surface stays off platform.TokenStore, which every token fake would
// otherwise have to implement.
type MagicLinkStore interface {
	// SweepExpiredMagicLinkTokens deletes rows expired before olderThan and returns the count.
	// It never deletes a live row.
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

// Run drives the sweep loop until ctx is cancelled. It sweeps once immediately, since the table may
// have grown while the process was down, then every Interval.
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

// Sweep runs one retention pass and refreshes the row-count gauge; it is exported so tests can
// drive a single pass.
//
// Errors are recorded on [obs.MagicLinkTokenErrorsTotal] and swallowed because the next tick
// retries. The gauge is refreshed even when the DELETE failed, since that is when an operator most
// needs the row count.
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
	// Liveness: the sweep completed, including the failure arm; only cancellation skips this.
	obs.AuthReaperLastSweepUnix.WithLabelValues(obs.AuthReaperMagicLink).Set(float64(r.now().Unix()))
}

// refreshGauge publishes the current row count. A count failure is counted under the same `sweep`
// op, since the janitor pass failed either way.
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
