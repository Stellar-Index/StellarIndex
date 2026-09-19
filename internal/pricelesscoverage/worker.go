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
//
// The concentration is measured on EVERY venue that records a
// counterparty, not only the order book: an AMM fill names the taker and
// leaves the maker side to the pool, so its key is that one account (see
// timescale.popularPricelessCandidatesSQL). Volume from a venue that
// records no account at all (the external CEX feeds) can only dilute the
// share, so an unmeasurable market pages an operator rather than being
// quietly dropped — the same fail-loud direction as pricedViaClassicAlias.
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

// DefaultSweepTimeout bounds one sweep's candidate read. The full-catalogue
// scan measures ~55s warm against the live trades hypertable and can run
// longer on a cold cache or under DB load; this ceiling is comfortably
// above that yet stops a pathological sweep from holding a connection
// indefinitely. A timed-out sweep is best-effort-skipped (the gauge holds
// its last good value), never fatal. Options.SweepTimeout <= 0 uses this.
const DefaultSweepTimeout = 5 * time.Minute

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

	// washConcentrationThreshold — a single unordered counterparty key
	// (the (maker,taker) pair on the order book, the lone taker account on
	// an AMM) owning >= this share of an asset's 7d priced volume is the
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
	// ResolveSAC maps a Stellar Asset Contract id to its classic asset's
	// canonical id ("CODE-ISSUER" / native). A Soroban-venue trade is
	// recorded under the contract id while the same asset's price is
	// served under its classic id, so without this a SAC-wrapped classic
	// asset that trades on an AMM reads as a priceless popular asset
	// (yBTC on aquarius, 2026-09-17). Nil disables the aliasing; a miss
	// leaves the candidate as read.
	ResolveSAC func(ctx context.Context, contractID string) (string, bool)
	// IsPriced asks the sweep's own priced set about one asset id — the
	// resolved classic id. Nil disables the aliasing.
	IsPriced func(ctx context.Context, assetID string) (bool, error)

	// Interval is the sweep cadence. <= 0 falls back to DefaultInterval.
	Interval time.Duration
	// SweepTimeout bounds one sweep's candidate read. <= 0 falls back to
	// DefaultSweepTimeout.
	SweepTimeout time.Duration
	Logger       *slog.Logger
	// Clock lets tests pin "now" for the last-success timestamp. Defaults
	// to time.Now().UTC.
	Clock func() time.Time
}

// Worker sweeps the catalogue on a ticker and publishes the
// priceless-popular coverage gauge + sweep-health metrics.
type Worker struct {
	reader       CandidateReader
	resolveSAC   func(ctx context.Context, contractID string) (string, bool)
	isPriced     func(ctx context.Context, assetID string) (bool, error)
	interval     time.Duration
	sweepTimeout time.Duration
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
		resolveSAC:   opts.ResolveSAC,
		isPriced:     opts.IsPriced,
		interval:     opts.Interval,
		sweepTimeout: opts.SweepTimeout,
		logger:       opts.Logger,
		now:          opts.Clock,
	}
	if w.interval <= 0 {
		w.interval = DefaultInterval
	}
	if w.sweepTimeout <= 0 {
		w.sweepTimeout = DefaultSweepTimeout
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
	count := 0
	for _, sig := range sigs {
		if !popularPriceless(sig) {
			continue
		}
		classic, priced := w.pricedViaClassicAlias(sweepCtx, sig.AssetID)
		if priced {
			w.logger.Info("priceless-popular coverage: SAC candidate is priced under its classic asset",
				"asset_id", sig.AssetID, "classic_asset", classic,
				"volume_7d_usd", sig.Volume7dUSD, "trades_7d", sig.Trades7d)
			continue
		}
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
	obs.PricelessCoverageCheckRunsTotal.WithLabelValues("ok").Inc()
	obs.PricelessCoverageCheckLastSuccessUnix.Set(float64(w.now().Unix()))
}

// popularPriceless is the tripwire's pure verdict for one asset: fire iff
// the asset is priceless, NOT deliberately withheld, NOT a wash farm, and
// popular by MARKET-CHARACTER volume. Every threshold lives here (never in
// the SQL), so the classification is unit-testable without a database.
// pricedViaClassicAlias resolves a C… candidate to its classic asset and
// asks whether THAT is priced. Returns the classic id (empty when the
// candidate is not a resolvable SAC) and the verdict. A resolver or probe
// error is logged and treated as "not priced": the tripwire fails loud,
// never quiet.
func (w *Worker) pricedViaClassicAlias(ctx context.Context, assetID string) (string, bool) {
	if w.resolveSAC == nil || w.isPriced == nil || !looksLikeContractID(assetID) {
		return "", false
	}
	classic, ok := w.resolveSAC(ctx, assetID)
	if !ok || classic == "" || classic == assetID {
		return "", false
	}
	priced, err := w.isPriced(ctx, classic)
	if err != nil {
		w.logger.Warn("priceless-popular coverage: priced probe for the classic alias failed; treating as priceless",
			"asset_id", assetID, "classic_asset", classic, "err", err)
		return classic, false
	}
	return classic, priced
}

// looksLikeContractID is the C-strkey shape: 56 chars, leading C.
func looksLikeContractID(id string) bool {
	return len(id) == 56 && id[0] == 'C'
}

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
// single counterparty key — the market-character discriminator. A
// concentrated asset contributes no market-character volume, so it never
// clears the popularity floor no matter how large its RAW volume.
//
// The share is measured against the asset's FULL 7d priced volume on
// every venue that names a counterparty, so this fires for an AMM-only
// asset (one wallet round-tripping through a pool) exactly as it does for
// an order-book wash pair. No floor on AttributedVolShare is needed: the
// share can only reach the threshold if the attributed population does
// too, so a market with no recorded accounts is never suppressed by it.
func washConcentrated(s timescale.AssetCoverageSignals) bool {
	return s.TopAccountPairVolShare >= washConcentrationThreshold
}
