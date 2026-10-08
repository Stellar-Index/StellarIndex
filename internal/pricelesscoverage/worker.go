// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package pricelesscoverage pages when a popular asset has no served price and
// no recorded reason, instead of waiting for someone to notice it on /assets.
//
// Popularity is measured on market-character volume, never raw volume: a wash
// farm trades huge raw volume and would otherwise self-select into the alert.
// Volume concentrated in one counterparty key is excluded on every venue that
// records one; venues recording no account only dilute the share, so an
// unmeasurable market pages rather than being quietly dropped.
package pricelesscoverage

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// DefaultInterval is the sweep cadence; a gap pages a ticket, not an SLO, and the
// windows are 24h/7d, so a slow cadence keeps the full-catalogue scan cheap.
const DefaultInterval = 10 * time.Minute

// DefaultSweepTimeout bounds one candidate read (~55s warm); a timed-out sweep is
// skipped and the gauge keeps its last good value.
const DefaultSweepTimeout = 5 * time.Minute

// DefaultProbeTimeout bounds one candidate's classic-alias probe. Each probe derives
// from the sweep's parent context so a long sweep's shrinking deadline cannot time
// many out together and mimic a mass coverage gap.
const DefaultProbeTimeout = 15 * time.Second

// Popularity and market-character thresholds; "withheld" is asked of the serving
// gate through Options.Withheld, not re-derived here.
const (
	// FloorVolume7dUSD / FloorTrades7d are the popularity floor, applied to
	// MARKET-CHARACTER volume/trades (raw minus wash). Above EITHER, an
	// asset is popular enough that a missing price is worth paging for.
	FloorVolume7dUSD = 10_000.0
	FloorTrades7d    = 5_000

	// washConcentrationThreshold: one counterparty key (the order-book pair, or an
	// AMM's lone taker) owning this share of 7d priced volume is the wash signature,
	// and such an asset contributes no market-character volume.
	washConcentrationThreshold = 0.90
)

// CandidateReader is the storage seam the tripwire needs: the per-asset
// coverage signals for every priceless, actively-traded asset. Satisfied
// by *timescale.Store.PopularPricelessCandidates.
type CandidateReader interface {
	PopularPricelessCandidates(ctx context.Context) ([]timescale.AssetCoverageSignals, error)
}

// Options configures the [Worker].
type Options struct {
	// ResolveSAC maps a SAC id to its classic asset id. Soroban trades record the
	// contract id while the price is served under the classic id, so without it a
	// SAC-wrapped asset reads as priceless. Nil disables aliasing.
	ResolveSAC func(ctx context.Context, contractID string) (string, bool)
	// IsPriced asks the sweep's own priced set about one asset id — the
	// resolved classic id. Nil disables the aliasing.
	IsPriced func(ctx context.Context, assetID string) (bool, error)
	// Withheld reports whether the serving substance gate deliberately withholds the
	// asset's USD price; wire it to the gate itself, since a re-derivation disagrees.
	// It must answer false when the gate could not measure, so an unknown pages.
	Withheld func(ctx context.Context, assetID string) bool

	// Interval is the sweep cadence. <= 0 falls back to DefaultInterval.
	Interval time.Duration
	// SweepTimeout bounds one sweep's candidate read. <= 0 falls back to
	// DefaultSweepTimeout.
	SweepTimeout time.Duration
	// ProbeTimeout bounds ONE candidate's classic-alias probe. <= 0 falls
	// back to DefaultProbeTimeout.
	ProbeTimeout time.Duration
	Logger       *slog.Logger
	// Clock lets tests pin "now" for the last-success timestamp. Defaults
	// to time.Now().UTC.
	Clock func() time.Time
}

// Worker sweeps the catalogue on a ticker and publishes the
// priceless-popular coverage gauge + sweep-health metrics.
type Worker struct {
	reader CandidateReader
	// resolveSAC is atomic because a background dial retry may set it while Sweep runs.
	resolveSAC   atomic.Pointer[func(ctx context.Context, contractID string) (string, bool)]
	isPriced     func(ctx context.Context, assetID string) (bool, error)
	withheld     func(ctx context.Context, assetID string) bool
	interval     time.Duration
	sweepTimeout time.Duration
	probeTimeout time.Duration
	logger       *slog.Logger
	now          func() time.Time
}

// New builds a Worker. Panics if reader is nil (a wiring bug — the caller
// gates construction on the store being present).
func New(reader CandidateReader, opts Options) *Worker {
	if reader == nil {
		panic("pricelesscoverage: New requires a non-nil reader")
	}
	w := &Worker{
		reader:       reader,
		isPriced:     opts.IsPriced,
		withheld:     opts.Withheld,
		interval:     opts.Interval,
		sweepTimeout: opts.SweepTimeout,
		probeTimeout: opts.ProbeTimeout,
		logger:       opts.Logger,
		now:          opts.Clock,
	}
	if opts.ResolveSAC != nil {
		w.SetResolveSAC(opts.ResolveSAC)
	}
	if w.interval <= 0 {
		w.interval = DefaultInterval
	}
	if w.sweepTimeout <= 0 {
		w.sweepTimeout = DefaultSweepTimeout
	}
	if w.probeTimeout <= 0 {
		w.probeTimeout = DefaultProbeTimeout
	}
	if w.logger == nil {
		w.logger = slog.Default()
	}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	return w
}

// SetResolveSAC wires the SAC resolver after construction; safe concurrently with
// Run/Sweep, so start-up neither blocks on the lake dial nor gives up after one failure.
func (w *Worker) SetResolveSAC(f func(ctx context.Context, contractID string) (string, bool)) {
	w.resolveSAC.Store(&f)
}

// Run sweeps once immediately (fresh before the staleness alert's grace window),
// then every Interval, until ctx is cancelled.
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

// Sweep runs one coverage pass and publishes the metrics. Errors are counted and
// swallowed; the gauge updates only on success so a read failure cannot flap to 0.
func (w *Worker) Sweep(ctx context.Context) {
	sweepCtx, cancel := context.WithTimeout(ctx, w.sweepTimeout)
	defer cancel()
	sigs, err := w.reader.PopularPricelessCandidates(sweepCtx)
	if err != nil {
		// A parent cancellation (shutdown) is silent; a sweep-timeout or
		// DB error is recorded so the sweep-health metric shows it, and
		// the gauge is left holding its last good value.
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return
		}
		w.logger.Warn("priceless-popular coverage sweep: candidate read failed", "err", err)
		obs.PricelessCoverageCheckRunsTotal.WithLabelValues("error").Inc()
		return
	}
	count, probeTimeouts := 0, 0
	for _, sig := range sigs {
		if !popularPriceless(sig) {
			continue
		}
		// Derive from the parent ctx, not sweepCtx; see DefaultProbeTimeout.
		probeCtx, probeCancel := context.WithTimeout(ctx, w.probeTimeout)
		classic, priced, timedOut := w.pricedViaClassicAlias(probeCtx, sig.AssetID)
		if timedOut {
			probeTimeouts++
		}
		if priced {
			w.logger.Info("priceless-popular coverage: SAC candidate is priced under its classic asset",
				"asset_id", sig.AssetID, "classic_asset", classic,
				"volume_7d_usd", sig.Volume7dUSD, "trades_7d", sig.Trades7d)
			probeCancel()
			continue
		}
		if w.withheld != nil && w.withheld(probeCtx, sig.AssetID) {
			w.logger.Info("priceless-popular coverage: substance gate withholds the price; not a gap",
				"asset_id", sig.AssetID, "volume_24h_usd", sig.Volume24hUSD)
			probeCancel()
			continue
		}
		probeCancel()
		count++
		w.logger.Warn("priceless-popular coverage gap: market-popular asset has no price",
			"asset_id", sig.AssetID,
			"classic_asset", classic,
			"volume_7d_usd", sig.Volume7dUSD,
			"trades_7d", sig.Trades7d,
			"top_account_pair_share", sig.TopAccountPairVolShare,
			"attributed_vol_share", sig.AttributedVolShare)
	}
	obs.AssetsPopularPriceless.Set(float64(count))
	// "ok" means every probe answered, so a mass gap under "ok" is not a timeout burst.
	outcome := "ok"
	if probeTimeouts > 0 {
		w.logger.Warn("priceless-popular coverage sweep: probe timeouts during sweep",
			"probe_timeouts", probeTimeouts, "candidates", len(sigs))
		outcome = "degraded"
	}
	obs.PricelessCoverageCheckRunsTotal.WithLabelValues(outcome).Inc()
	obs.PricelessCoverageCheckLastSuccessUnix.Set(float64(w.now().Unix()))
}

// pricedViaClassicAlias resolves a C… candidate to its classic asset and asks
// whether that is priced, returning the classic id, the verdict, and whether the
// probe timed out. Errors read as unpriced (fail loud); timeouts are reported apart.
func (w *Worker) pricedViaClassicAlias(ctx context.Context, assetID string) (string, bool, bool) {
	resolveSACPtr := w.resolveSAC.Load()
	if resolveSACPtr == nil || w.isPriced == nil || !looksLikeContractID(assetID) {
		return "", false, false
	}
	resolveSAC := *resolveSACPtr
	classic, ok := resolveSAC(ctx, assetID)
	if !ok || classic == "" || classic == assetID {
		return "", false, errors.Is(ctx.Err(), context.DeadlineExceeded)
	}
	priced, err := w.isPriced(ctx, classic)
	if err != nil {
		w.logger.Warn("priceless-popular coverage: priced probe for the classic alias failed; treating as priceless",
			"asset_id", assetID, "classic_asset", classic, "err", err)
		return classic, false, errors.Is(err, context.DeadlineExceeded)
	}
	return classic, priced, false
}

// looksLikeContractID is the C-strkey shape: 56 chars, leading C.
func looksLikeContractID(id string) bool {
	return len(id) == 56 && id[0] == 'C'
}

// popularPriceless fires iff the asset is priceless, not wash-concentrated, and
// popular by market-character volume. Thresholds live here, not in SQL, so this
// is unit-testable.
func popularPriceless(s timescale.AssetCoverageSignals) bool {
	if s.HasPriceUSD {
		return false // priced — not a coverage gap
	}
	if washConcentrated(s) {
		return false // volume-painting wash is not a real market
	}
	// Subtract the top pair's own volume and trades first, so a sub-threshold wash
	// pair cannot carry an asset past the floor.
	marketVol := s.Volume7dUSD - s.TopAccountPairVolUSD
	marketTrades := s.Trades7d - s.TopAccountPairTrades7d
	return marketVol > FloorVolume7dUSD || marketTrades > FloorTrades7d
}

// washConcentrated reports whether one counterparty key dominates the asset's full
// 7d priced volume on every venue naming one, so it fires for an AMM-only round-tripper
// too. A market with no recorded accounts can never reach the threshold.
func washConcentrated(s timescale.AssetCoverageSignals) bool {
	return s.TopAccountPairVolShare >= washConcentrationThreshold
}

// SubstanceWithheld builds [Options.Withheld] from the serving substance gate and the
// USD pegs, asking what /v1/assets asks. Unparseable or unmeasured means not withheld.
func SubstanceWithheld(gate pricingguard.SubstanceVerdicter, usdPegs []canonical.Asset) func(context.Context, string) bool {
	if gate == nil {
		return nil
	}
	return func(ctx context.Context, assetID string) bool {
		asset, err := canonical.ParseAsset(assetID)
		if err != nil {
			return false
		}
		allowed, measured := pricingguard.AssetSubstanceVerdict(ctx, gate, asset, usdPegs, "priceless_coverage")
		return measured && !allowed
	}
}
