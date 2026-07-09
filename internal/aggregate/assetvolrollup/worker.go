// Package assetvolrollup maintains the asset_volume_24h rollup that
// backs the volume_24h_usd column on the /v1/assets listing.
//
// A ticker-driven worker runs the trailing-24h per-asset USD-volume SUM
// over the prices_1m continuous aggregate (single-sided: each asset as
// base OR quote) on a slow cadence and upserts one row per asset into
// the rollup table, so the listing LEFT JOINs a small keyed-on-PK table
// instead of re-summing prices_1m per request — the ~4.8s cold
// all-asset scan the 2026-07-06 latency incident measured. Runs in the
// aggregator binary alongside the change-summary + supply refresh
// workers.
//
// See migrations/0087_create_asset_volume_24h_rollup.up.sql for the
// table and internal/storage/timescale/asset_catalogue.go for the SUM + upsert
// SQL.
package assetvolrollup

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/StellarIndex/stellar-index/internal/obs"
)

// DefaultInterval is the refresh cadence. The SUM scans ~24h of
// prices_1m per asset — the heaviest of the two #43 rollups — so a
// couple of minutes keeps the aggregator load modest while a
// trailing-24h volume figure a minute or two stale is immaterial to the
// /v1/assets listing it feeds.
const DefaultInterval = 2 * time.Minute

// Refresher recomputes and atomically replaces the asset_volume_24h
// rollup. Production wiring is *timescale.Store.RefreshAssetVolume24h;
// tests use a fake.
type Refresher interface {
	RefreshAssetVolume24h(ctx context.Context) error
}

// Options tunes a Worker. Logger rides here (not positional) per the
// repo's Go idioms.
type Options struct {
	// Interval is the refresh cadence. <= 0 falls back to DefaultInterval.
	Interval time.Duration
	Logger   *slog.Logger
}

// Worker periodically refreshes the asset_volume_24h rollup.
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

// Run refreshes once immediately (so a fresh boot doesn't render
// null-volume for a full interval), then on every tick until ctx is
// cancelled. Refresh failures log + count in the metric; the worker
// never exits on a transient Postgres error.
func (w *Worker) Run(ctx context.Context) error {
	if w == nil {
		return errors.New("assetvolrollup: nil Worker")
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

// refresh runs one sum-and-upsert pass, recording the paired outcome
// counter + latency histogram (the wave-88/89/90/91 worker convention).
func (w *Worker) refresh(ctx context.Context) {
	start := time.Now()
	if err := w.refresher.RefreshAssetVolume24h(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		w.observe("refresh_error", start)
		w.logger.Warn("asset-volume rollup refresh failed", "err", err)
		return
	}
	w.observe("ok", start)
}

func (w *Worker) observe(outcome string, start time.Time) {
	obs.AssetVolumeRollupSweepsTotal.WithLabelValues(outcome).Inc()
	obs.AssetVolumeRollupSweepDurationSeconds.WithLabelValues(outcome).
		Observe(time.Since(start).Seconds())
}
