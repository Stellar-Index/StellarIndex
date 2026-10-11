// Package protoeventsrollup maintains the protocol_events_24h rollup behind events_24h on /v1/protocols.
// A slow ticker runs the trailing-24h per-source event census (UNION ALL count over ~17 hypertables) into one row per source, replacing a multi-second request-time census.
// See migrations/0086_create_protocol_events_24h_rollup.up.sql and internal/storage/timescale/protocol_stats.go.
package protoeventsrollup

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/rollupworker"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// DefaultInterval is the refresh cadence; the census is cheap per hypertable but not free.
const DefaultInterval = 2 * time.Minute

// Refresher recomputes and atomically replaces the protocol_events_24h rollup.
type Refresher interface {
	RefreshProtocolEventCounts(ctx context.Context) error
}

// Options tunes a Worker.
type Options struct {
	// Interval is the refresh cadence. <= 0 falls back to DefaultInterval.
	Interval time.Duration
	Logger   *slog.Logger
}

// Worker periodically refreshes the protocol_events_24h rollup.
type Worker struct {
	refresher Refresher
	interval  time.Duration
	logger    *slog.Logger
}

// New constructs the worker, or returns nil when the refresher is missing.
func New(refresher Refresher, opts Options) *Worker {
	if refresher == nil {
		return nil
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{refresher: refresher, interval: interval, logger: logger}
}

// Run refreshes immediately (no zeros after boot), then every tick until ctx is cancelled; errors are logged, never fatal.
func (w *Worker) Run(ctx context.Context) error {
	if w == nil {
		return errors.New("protoeventsrollup: nil Worker")
	}
	return rollupworker.Run(ctx, w.interval, w.refresh)
}

// refresh runs one census-and-upsert pass, recording the paired
// outcome counter + latency histogram (the worker
// convention).
func (w *Worker) refresh(ctx context.Context) {
	start := time.Now()
	if err := w.refresher.RefreshProtocolEventCounts(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		w.observe("refresh_error", start)
		w.logger.Warn("protocol-events rollup refresh failed", "err", err)
		return
	}
	w.observe("ok", start)
}

func (w *Worker) observe(outcome string, start time.Time) {
	obs.ProtocolEventsRollupSweepsTotal.WithLabelValues(outcome).Inc()
	obs.ProtocolEventsRollupSweepDurationSeconds.WithLabelValues(outcome).
		Observe(time.Since(start).Seconds())
}
