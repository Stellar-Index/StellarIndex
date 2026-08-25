// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package pricelesscoverage is the priceless-popular pricing-coverage
// tripwire (task #28 Part B). A recurring aggregator sweep asks: is any
// asset genuinely popular yet has no served price and no recorded reason?
// Such a gap should PAGE — surface as a metric + ticket alert — instead of
// waiting for an operator to notice it while browsing /assets.
//
// The popularity floor is deliberately measured on MARKET-CHARACTER
// volume, never raw volume: a volume-painting wash farm (the reported scam
// AUD: ~108/109 of its trades one wallet pair) trades huge raw USD volume,
// so a raw-volume floor would let every wash farm self-select into the
// alert. The classifier excludes volume concentrated in a single account
// pair, mirroring the volume-character rollup design
// (feat/scam-labels-and-volume-character, PR #161): while that branch is
// unmerged the same single-account-pair filter is computed inline here.
package pricelesscoverage

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// DefaultInterval is the sweep cadence when Options.Interval is unset. A
// coverage gap is not time-critical (it pages a ticket, not an SLO), and
// the underlying trailing windows are 24h/7d, so a slow cadence keeps the
// full-catalogue scan cheap.
const DefaultInterval = 10 * time.Minute

// Popularity + market-character thresholds. The floor numbers are the
// task-directed values; the concentration + substance thresholds match the
// serving stack so the tripwire's notion of "popular" and "withheld" agree
// with what the API actually does.
const (
	// FloorVolume7dUSD / FloorTrades7d are the popularity floor, applied to
	// MARKET-CHARACTER volume/trades (raw minus wash). Above EITHER, an
	// asset is popular enough that a missing price is worth paging for.
	FloorVolume7dUSD = 10_000.0
	FloorTrades7d    = 5_000

	// washConcentrationThreshold — a single unordered (maker,taker) account
	// pair owning >= this share of an asset's 7d priced volume is the
	// volume-painting / ping-pong / dust signature. Matches PR #161's
	// volumeCharacterConcentrationThreshold. A wash-concentrated asset
	// contributes NO market-character volume, so it can never be "popular".
	washConcentrationThreshold = 0.90

	// substanceServeFloorUSD is the substance gate's trailing-window USD
	// serve floor (pricingguard.DefaultSubstanceMinVolumeUSD). A recent
	// (24h) market below it is one the gate WITHHOLDS fail-closed, so the
	// asset's pricelessness is expected (a recorded withheld verdict) — not
	// a coverage gap. Kept as a literal to avoid importing the pricingguard
	// serving package into this analytics worker; pinned equal by
	// TestSubstanceFloor_MatchesServingDefault.
	substanceServeFloorUSD = 1_000.0
)

// CandidateReader is the storage seam the tripwire needs: the per-asset
// coverage signals for every priceless, actively-traded asset. Satisfied
// by *timescale.Store.PopularPricelessCandidates.
type CandidateReader interface {
	PopularPricelessCandidates(ctx context.Context) ([]timescale.AssetCoverageSignals, error)
}

// Options configures the [Worker].
type Options struct {
	// Interval is the sweep cadence. <= 0 falls back to DefaultInterval.
	Interval time.Duration
	Logger   *slog.Logger
	// Clock lets tests pin "now" for the last-success timestamp. Defaults
	// to time.Now().UTC.
	Clock func() time.Time
}

// Worker sweeps the catalogue on a ticker and publishes the
// priceless-popular coverage gauge + sweep-health metrics.
type Worker struct {
	reader   CandidateReader
	interval time.Duration
	logger   *slog.Logger
	now      func() time.Time
}

// New builds a Worker. Panics if reader is nil (a wiring bug — the caller
// gates construction on the store being present).
func New(reader CandidateReader, opts Options) *Worker {
	if reader == nil {
		panic("pricelesscoverage: New requires a non-nil reader")
	}
	w := &Worker{
		reader:   reader,
		interval: opts.Interval,
		logger:   opts.Logger,
		now:      opts.Clock,
	}
	if w.interval <= 0 {
		w.interval = DefaultInterval
	}
	if w.logger == nil {
		w.logger = slog.Default()
	}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	return w
}

// Run drives the sweep loop until ctx is cancelled. Sweeps once
// immediately (so the gauge is fresh within one tick of start-up, before
// the staleness alert's grace window), then every Interval. Returns
// ctx.Err() on cancellation.
func (w *Worker) Run(ctx context.Context) error {
	tick := time.NewTicker(w.interval)
	defer tick.Stop()
	w.logger.Info("priceless-popular coverage tripwire started", "interval", w.interval)
	for {
		w.Sweep(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// Sweep runs one coverage pass and publishes the metrics. Exported so
// tests drive a single pass deterministically. Errors are recorded on the
// outcome counter and swallowed (best-effort background worker); the gauge
// is only updated on success so a read failure leaves the last good count
// standing rather than flapping to a false 0.
func (w *Worker) Sweep(ctx context.Context) {
	sigs, err := w.reader.PopularPricelessCandidates(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		w.logger.Warn("priceless-popular coverage sweep: candidate read failed", "err", err)
		obs.PricelessCoverageCheckRunsTotal.WithLabelValues("error").Inc()
		return
	}
	count := 0
	for _, sig := range sigs {
		if popularPriceless(sig) {
			count++
			w.logger.Warn("priceless-popular coverage gap: market-popular asset has no price",
				"asset_id", sig.AssetID,
				"volume_7d_usd", sig.Volume7dUSD,
				"trades_7d", sig.Trades7d,
				"top_account_pair_share", sig.TopAccountPairVolShare)
		}
	}
	obs.AssetsPopularPriceless.Set(float64(count))
	obs.PricelessCoverageCheckRunsTotal.WithLabelValues("ok").Inc()
	obs.PricelessCoverageCheckLastSuccessUnix.Set(float64(w.now().Unix()))
}

// popularPriceless is the tripwire's pure verdict for one asset: fire iff
// the asset is priceless, NOT deliberately withheld, NOT a wash farm, and
// popular by MARKET-CHARACTER volume. Every threshold lives here (never in
// the SQL), so the classification is unit-testable without a database.
func popularPriceless(s timescale.AssetCoverageSignals) bool {
	if s.HasPriceUSD {
		return false // priced — not a coverage gap
	}
	if withheldVerdict(s) {
		return false // gate withholds a below-floor recent market by design
	}
	if washConcentrated(s) {
		return false // volume-painting wash is not a real market
	}
	return s.Volume7dUSD > FloorVolume7dUSD || s.Trades7d > FloorTrades7d
}

// withheldVerdict reports whether the asset's price is deliberately
// withheld: its trailing-24h market is below the substance serve floor, so
// the substance gate refuses to publish a price (fail-closed). A priceless
// asset with a withheld verdict is EXPECTED, not a coverage gap.
func withheldVerdict(s timescale.AssetCoverageSignals) bool {
	return s.Volume24hUSD < substanceServeFloorUSD
}

// washConcentrated reports whether the asset's volume is dominated by a
// single account pair — the market-character discriminator. A concentrated
// asset contributes no market-character volume, so it never clears the
// popularity floor no matter how large its RAW volume.
func washConcentrated(s timescale.AssetCoverageSignals) bool {
	return s.TopAccountPairVolShare >= washConcentrationThreshold
}
