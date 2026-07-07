// Package protoeventsrollup maintains the protocol_events_24h rollup
// that backs the events_24h column on /v1/protocols.
//
// A ticker-driven worker runs the trailing-24h per-source event census
// (a UNION ALL count(*) over ~17 served protocol hypertables) on a slow
// cadence and upserts one row per source into the rollup table, so the
// API read path is a keyed-on-PK lookup instead of the multi-second
// census the 2026-07-06 latency incident measured. Runs in the
// aggregator binary alongside the change-summary + supply refresh
// workers.
//
// See migrations/0086_create_protocol_events_24h_rollup.up.sql for the
// table and internal/storage/timescale/protocol_stats.go for the
// census + upsert SQL.
package protoeventsrollup

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/StellarIndex/stellar-index/internal/obs"
)

// DefaultInterval is the refresh cadence. The census legs are each a
// 24h count over one hypertable's recent chunks — cheap, but not free —
// and a couple of minutes of staleness on a trailing-24h event count is
// immaterial to the /v1/protocols surface it feeds.
const DefaultInterval = 2 * time.Minute

// Refresher recomputes and atomically replaces the protocol_events_24h
// rollup. Production wiring is *timescale.Store.RefreshProtocolEventCounts;
// tests use a fake.
type Refresher interface {
	RefreshProtocolEventCounts(ctx context.Context) error
}

// Options tunes a Worker. Logger rides here (not positional) per the
// repo's Go idioms.
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

// New constructs the worker. Returns nil when the refresher is missing
// so callers can gate with a plain nil check (mirrors usage.NewRollup).
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

// Run refreshes once immediately (so a fresh boot doesn't render zeros
// for a full interval), then on every tick until ctx is cancelled.
// Refresh failures log + count in the metric; the worker never exits on
// a transient Postgres error.
func (w *Worker) Run(ctx context.Context) error {
	if w == nil {
		return errors.New("protoeventsrollup: nil Worker")
	}
	tick := time.NewTicker(w.interval)
	defer tick.Stop()

	w.refresh(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			w.refresh(ctx)
		}
	}
}

// refresh runs one census-and-upsert pass, recording the paired
// outcome counter + latency histogram (the wave-88/89/90/91 worker
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
