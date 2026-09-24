// Package retentionreaper bounds the platform tables whose rows reach a
// terminal state but were never deleted: `sessions` (ended dashboard
// sign-ins, each carrying IP / user-agent / geo history) and
// `webhook_deliveries` (finished outbound attempts, each carrying the
// event payload). The sibling token tables already have dedicated
// reapers (internal/logincodereaper, internal/magiclinkreaper,
// internal/signupreaper); these two grew with every sign-in and every
// delivered event.
//
// Each Reaper wraps one store sweep: delete terminal rows older than
// Retention, immediately and then every Interval, in the API binary,
// bounded to the process root context. Liveness publishes on the
// shared auth-reaper gauges so `stellarindex_auth_reaper_stalled`
// covers these sweeps too.
//
// Deliberately no config section, for the reason the magic-link reaper
// has none: retention here is a PII bound, and a knob that disables it
// re-opens the unbounded table.
//
// `audit_log` is intentionally NOT reaped. The platform spec (§8.2) sets
// it at 12 months online plus 7 years archived; no archive exists, so
// deleting rows would destroy compliance records rather than move them.
package retentionreaper

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

const (
	// DefaultInterval is the sweep cadence.
	DefaultInterval = time.Hour
	// SessionRetention is how long an expired or revoked session row is
	// kept for sign-in forensics: 90 days, the "immediately available"
	// audit-trail window PCI DSS 10.7 sets for security logs.
	SessionRetention = 90 * 24 * time.Hour
	// WebhookDeliveryRetention is how long a finished delivery stays in
	// the customer-visible delivery log. Retries span 72 h, so a row
	// this old is long settled.
	WebhookDeliveryRetention = 30 * 24 * time.Hour
)

// SweepFunc deletes terminal rows older than olderThan and returns the
// count removed. It must never delete a row that is still live.
type SweepFunc func(ctx context.Context, olderThan time.Time) (int64, error)

// Options configures one Reaper.
type Options struct {
	// Name is the `reaper` metric label (obs.AuthReaperSession,
	// obs.AuthReaperWebhookDelivery).
	Name  string
	Sweep SweepFunc
	// Retention is the terminal-row age threshold. Required: a table's
	// retention is a policy decision, not something to default.
	Retention time.Duration
	// Interval <= 0 falls back to DefaultInterval.
	Interval time.Duration
	Logger   *slog.Logger
	// Clock lets tests pin "now". Defaults to time.Now().UTC.
	Clock func() time.Time
}

// Reaper periodically runs one retention sweep.
type Reaper struct {
	name      string
	sweep     SweepFunc
	interval  time.Duration
	retention time.Duration
	logger    *slog.Logger
	now       func() time.Time
}

// New builds a Reaper. Panics on a missing name, sweep or retention —
// each is a wiring bug.
func New(opts Options) *Reaper {
	if opts.Name == "" || opts.Sweep == nil || opts.Retention <= 0 {
		panic("retentionreaper: New requires Name, Sweep and a positive Retention")
	}
	r := &Reaper{
		name:      opts.Name,
		sweep:     opts.Sweep,
		interval:  opts.Interval,
		retention: opts.Retention,
		logger:    opts.Logger,
		now:       opts.Clock,
	}
	if r.interval <= 0 {
		r.interval = DefaultInterval
	}
	if r.logger == nil {
		r.logger = slog.Default()
	}
	if r.now == nil {
		r.now = func() time.Time { return time.Now().UTC() }
	}
	obs.AuthReaperIntervalSeconds.WithLabelValues(r.name).Set(r.interval.Seconds())
	return r
}

// Run sweeps once immediately, then every Interval, until ctx is
// cancelled.
func (r *Reaper) Run(ctx context.Context) error {
	tick := time.NewTicker(r.interval)
	defer tick.Stop()
	r.logger.Info("retention reaper started",
		"reaper", r.name, "interval", r.interval, "retention", r.retention)
	for {
		r.Sweep(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// Sweep runs one retention pass. Errors are counted and swallowed: a
// failed pass is retried next tick.
func (r *Reaper) Sweep(ctx context.Context) {
	deleted, err := r.sweep(ctx, r.now().Add(-r.retention))
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return
	case err != nil:
		obs.RetentionReaperErrorsTotal.WithLabelValues(r.name).Inc()
		r.logger.Warn("retention reaper: sweep failed", "reaper", r.name, "err", err)
	case deleted > 0:
		obs.RetentionReaperRowsDeletedTotal.WithLabelValues(r.name).Add(float64(deleted))
		r.logger.Info("retention reaper: deleted rows", "reaper", r.name, "deleted", deleted)
	}
	obs.AuthReaperLastSweepUnix.WithLabelValues(r.name).Set(float64(r.now().Unix()))
}
