// Package assetcharacterrollup maintains the asset_volume_character rollup behind volume_character on /v1/assets.
// A slow ticker rolls the trailing window of trades (each trade counts for both base and quote, folded to canonical assets) into one row per asset, replacing a ~4s per-request roll.
// See migrations/0149_create_asset_volume_character_rollup.up.sql and internal/storage/timescale/asset_volume_character_rollup.go.
package assetcharacterrollup

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/rollupworker"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// DefaultInterval is the refresh cadence; slow because each roll scans the full 14-day trades window.
const DefaultInterval = 6 * time.Hour

// DefaultStartupDelay holds the first roll back so its long ACCESS SHARE lock on trades cannot block a post-deploy migration.
const DefaultStartupDelay = 30 * time.Minute

// Refresher recomputes and atomically replaces the asset_volume_character rollup.
type Refresher interface {
	RefreshAssetVolumeCharacter(ctx context.Context) error
	// AssetVolumeCharacterComputedAt is the last roll's time; zero when the rollup is empty.
	AssetVolumeCharacterComputedAt(ctx context.Context) (time.Time, error)
}

// Options tunes a Worker.
type Options struct {
	// Interval is the refresh cadence. <= 0 falls back to DefaultInterval.
	Interval time.Duration
	// StartupDelay is how long Run waits before the first roll. 0 rolls
	// immediately; negative falls back to DefaultStartupDelay.
	StartupDelay time.Duration
	Logger       *slog.Logger
}

// Worker periodically refreshes the asset_volume_character rollup.
type Worker struct {
	refresher Refresher
	interval  time.Duration
	delay     time.Duration
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
	delay := opts.StartupDelay
	if delay < 0 {
		delay = DefaultStartupDelay
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{refresher: refresher, interval: interval, delay: delay, logger: logger}
}

// Run refreshes after the startup delay, then every tick until ctx is cancelled; refresh errors are logged, never fatal.
func (w *Worker) Run(ctx context.Context) error {
	if w == nil {
		return errors.New("assetcharacterrollup: nil Worker")
	}
	if delay := w.firstDelay(ctx, time.Now()); delay > 0 {
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return rollupworker.Run(ctx, w.interval, w.refresh)
}

// firstDelay holds the first roll until the stored rollup is due, so a restart (every deploy) does not re-run the ~17 min roll early.
// The startup delay stays the floor; an unreadable computed_at falls back to it.
func (w *Worker) firstDelay(ctx context.Context, now time.Time) time.Duration {
	at, err := w.refresher.AssetVolumeCharacterComputedAt(ctx)
	if err != nil {
		w.logger.Warn("asset-character rollup: read last roll time", "err", err)
		return w.delay
	}
	if due := at.Add(w.interval).Sub(now); due > w.delay {
		return due
	}
	return w.delay
}

// refresh runs one roll-and-upsert pass, recording the paired outcome
// counter + latency histogram (the aggregator worker convention).
func (w *Worker) refresh(ctx context.Context) {
	start := time.Now()
	if err := w.refresher.RefreshAssetVolumeCharacter(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		w.observe("refresh_error", start)
		w.logger.Warn("asset-character rollup refresh failed", "err", err)
		return
	}
	w.observe("ok", start)
}

func (w *Worker) observe(outcome string, start time.Time) {
	obs.AssetCharacterRollupSweepsTotal.WithLabelValues(outcome).Inc()
	obs.AssetCharacterRollupSweepDurationSeconds.WithLabelValues(outcome).
		Observe(time.Since(start).Seconds())
}
