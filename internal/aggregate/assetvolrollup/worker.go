// Package assetvolrollup maintains the asset_volume_24h rollup behind volume_24h_usd on /v1/assets.
// A slow ticker sums trailing-24h per-asset USD volume over prices_1m (single-sided) into one row per asset, replacing a ~4.8s per-request scan.
// See migrations/0087_create_asset_volume_24h_rollup.up.sql and internal/storage/timescale/asset_catalogue.go.
package assetvolrollup

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/rollupworker"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// DefaultInterval is the refresh cadence; the SUM scans ~24h of prices_1m per asset, so it stays at minutes.
const DefaultInterval = 2 * time.Minute

// Refresher recomputes and atomically replaces the asset_volume_24h rollup.
type Refresher interface {
	RefreshAssetVolume24h(ctx context.Context) error
}

// Options tunes a Worker.
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

// Run refreshes immediately (no null volume after boot), then every tick until ctx is cancelled; errors are logged, never fatal.
func (w *Worker) Run(ctx context.Context) error {
	if w == nil {
		return errors.New("assetvolrollup: nil Worker")
	}
	return rollupworker.Run(ctx, w.interval, w.refresh)
}

// refresh runs one sum-and-upsert pass, recording the paired outcome
// counter + latency histogram (the worker convention).
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
