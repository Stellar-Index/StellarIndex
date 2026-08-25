// Package assetcharacterrollup maintains the asset_volume_character rollup
// that backs the volume_character label + account-structure signals on the
// GET /v1/assets listing and the GET /v1/assets/{id} detail
// (wash-and-scam-signals design §2).
//
// A ticker-driven worker runs the all-asset single-pass account-structure
// roll over the trailing window of the `trades` hypertable (each trade
// contributes to BOTH its base_asset and quote_asset, folded onto their
// canonical assets) on a slow cadence and upserts one row per asset into
// the rollup table, so the detail does a keyed-on-PK lookup and the listing
// LEFT JOINs a small keyed table instead of running the ~4s per-request
// trades roll (measured 4.09s on the USDC detail, tripping the 4s
// per-request timeout and returning null). Runs in the aggregator binary
// alongside the asset-volume + protocol-events + change-summary workers.
//
// See migrations/0149_create_asset_volume_character_rollup.up.sql for the
// table and internal/storage/timescale/asset_volume_character_rollup.go for
// the all-asset roll + upsert SQL.
package assetcharacterrollup

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// DefaultInterval is the refresh cadence. The roll scans the full trailing
// window of `trades` for EVERY actively-traded asset (both sides, unordered
// account-pair aggregation) — the heaviest of the aggregator's asset
// rollups — so a slow cadence keeps aggregator load modest. A
// volume-character label a few minutes stale is immaterial to the analytics
// overlay it feeds (the window itself is 14 days).
const DefaultInterval = 15 * time.Minute

// Refresher recomputes and atomically replaces the asset_volume_character
// rollup. Production wiring is *timescale.Store.RefreshAssetVolumeCharacter;
// tests use a fake.
type Refresher interface {
	RefreshAssetVolumeCharacter(ctx context.Context) error
}

// Options tunes a Worker. Logger rides here (not positional) per the repo's
// Go idioms.
type Options struct {
	// Interval is the refresh cadence. <= 0 falls back to DefaultInterval.
	Interval time.Duration
	Logger   *slog.Logger
}

// Worker periodically refreshes the asset_volume_character rollup.
type Worker struct {
	refresher Refresher
	interval  time.Duration
	logger    *slog.Logger
}

// New constructs the worker. Returns nil when the refresher is missing so
// callers can gate with a plain nil check (mirrors assetvolrollup.New).
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

// Run refreshes once immediately (so a fresh boot doesn't omit
// volume_character for a full interval), then on every tick until ctx is
// cancelled. Refresh failures log + count in the metric; the worker never
// exits on a transient Postgres error.
func (w *Worker) Run(ctx context.Context) error {
	if w == nil {
		return errors.New("assetcharacterrollup: nil Worker")
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
