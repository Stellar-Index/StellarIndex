package orchestrator

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// refreshDivergenceAll iterates over every configured pair and asks
// the [DivergenceRefresher] to update its `div:<base>/<quote>` cache entry
// for the asset, using the shortest-window VWAP this Tick just
// wrote as the "our price" input — or, for a pair frozen at that
// window, the pinned last-known-good, refreshed as such
// ([Orchestrator.refreshPairDivergence]).
//
// Best-effort: per-pair errors are counted via
// `obs.DivergenceRefreshTotal{outcome=…}` and logged at WARN; the
// Tick's overall outcome label is unaffected. The cache TTL
// (the refresh cadence plus a worst-case pass) is the safety net — even if a
// few ticks fail, the API hot-path still serves stale-but-valid
// data while the worker recovers.
//
// Outcome labels:
//   - `ok`            — refresh succeeded; cache entry written
//     (pinned or not).
//   - `no_vwap`       — VWAP cache miss for this pair (frozen,
//     empty window, transient cache error). Skip.
//   - `parse_error`   — cached value couldn't be parsed as float.
//     Indicates a writer regression.
//   - `refresh_error` — the refresher returned an error (network
//     failure to all references, marshal failure,
//     cache write failure). The cache entry is
//     NOT updated; the previous entry's TTL keeps
//     counting down.
//
// Skipped silently when DivergenceRefresher or Windows is nil/empty
// (operator config / launch order).
func (o *Orchestrator) refreshDivergenceAll(ctx context.Context, now time.Time) {
	// The divergence cross-check is a SECONDARY, best-effort guard; a
	// panic in it (or the references it fans out to) must never crash the
	// aggregator and take the PRIMARY VWAP refresh down with it. Contain
	// it here so the panic path matches the "best-effort, never abort the
	// tick" contract the caller (Tick) already documents — before this,
	// a panic propagated through Tick→Run and crash-looped the process
	// (audit 2026-08-03). Compare already recovers per-reference
	// goroutine panics; this covers the outer per-pair loop.
	defer func() {
		if r := recover(); r != nil {
			o.logger.Error("divergence refresh panicked; VWAP tick protected", "panic", r)
		}
	}()
	if o.cfg.DivergenceRefresher == nil || len(o.cfg.Windows) == 0 {
		obs.DivergenceRefresherWired.Set(0)
		return
	}
	// Set before the interval gate and the per-pair loop, so a pass that
	// never reaches an outcome still arms stellarindex_divergence_no_ok_outcomes.
	obs.DivergenceRefresherWired.Set(1)
	// F-0030 follow-up (2026-05-27): gate the refresh behind a
	// minimum-elapsed interval so the external-reference quota (CMC
	// free tier = 10K/month) isn't exhausted by every-tick refreshes.
	// Skip silently when within the interval — operators see the gap
	// via `obs.DivergenceRefreshTotal{outcome=*}` rate going to zero
	// during the suppressed window. Zero interval = legacy
	// every-tick behaviour for backwards compatibility.
	if o.cfg.DivergenceMinInterval > 0 && !o.lastDivergenceRefreshAt.IsZero() &&
		now.Sub(o.lastDivergenceRefreshAt) < o.cfg.DivergenceMinInterval {
		return
	}
	o.lastDivergenceRefreshAt = now
	// The shortest window gives the freshest VWAP as the divergence
	// input; New sorts Windows ascending, so it is Windows[0].
	shortest := o.cfg.Windows[0]

	for _, pair := range o.cfg.Pairs {
		if err := ctx.Err(); err != nil {
			return
		}
		// Time the full per-pair refresh attempt — including the
		// VWAP cache lookup + parse + HTTP fan-out to every
		// configured reference. Recorded against the outcome
		// label so operators chart `ok` p95/p99 separately from
		// `refresh_error` (often the fast-fail path) and the
		// near-zero `no_vwap` / `parse_error` paths.
		start := time.Now()
		key := cachekeys.VWAP(pair.Base, pair.Quote, shortest)
		raw, err := o.cache.Get(ctx, key.String()).Result()
		if err != nil {
			// Cache miss is normal-path on the first tick or after
			// a freeze; log at debug so an operator looking at INFO
			// doesn't see false noise.
			obs.DivergenceRefreshTotal.WithLabelValues("no_vwap").Inc()
			obs.DivergenceRefreshDurationSeconds.WithLabelValues("no_vwap").Observe(time.Since(start).Seconds())
			o.logger.Debug("divergence refresh: no vwap in cache",
				"pair", pair.String(), "window", shortest, "err", err)
			continue
		}
		ourPrice, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			obs.DivergenceRefreshTotal.WithLabelValues("parse_error").Inc()
			obs.DivergenceRefreshDurationSeconds.WithLabelValues("parse_error").Observe(time.Since(start).Seconds())
			o.logger.Warn("divergence refresh: vwap parse failed",
				"pair", pair.String(), "raw", raw, "err", err)
			continue
		}
		if err := o.refreshPairDivergence(ctx, pair, shortest, ourPrice, now); err != nil {
			// CS-088: distinguish "all references dark" from a real refresh
			// error so a total reference outage is alertable, not silently
			// folded into the healthy path. Both still `continue`.
			outcome := "refresh_error"
			if errors.Is(err, divergence.ErrNoReferenceResponded) {
				outcome = "no_reference"
			}
			obs.DivergenceRefreshTotal.WithLabelValues(outcome).Inc()
			obs.DivergenceRefreshDurationSeconds.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
			o.logger.Warn("divergence refresh failed",
				"pair", pair.String(), "outcome", outcome, "err", err)
			continue
		}
		obs.DivergenceRefreshTotal.WithLabelValues("ok").Inc()
		obs.DivergenceRefreshDurationSeconds.WithLabelValues("ok").Observe(time.Since(start).Seconds())
	}
}

// refreshPairDivergence hands the refresher the pair's cached VWAP, as a
// pinned price when the pair is frozen at that window. A freeze skips the
// VWAP write and keeps the last-known-good alive ([Orchestrator.keepFrozenVWAPAlive]),
// so the cached value is then the price the freeze refused to move off:
// judged as a fresh price it reads as divergence from the market, which
// fires divergence_warning and its webhook for a pair deliberately not
// updating, and feeds a depressed cross-oracle input back into the
// confidence that is itself a freeze leg. frozenLeg covers a freeze made
// this tick and one still inside its hold.
func (o *Orchestrator) refreshPairDivergence(
	ctx context.Context, pair canonical.Pair, window time.Duration, price float64, now time.Time,
) error {
	if o.frozenLeg(pair, window) {
		return o.cfg.DivergenceRefresher.RefreshPinnedPair(ctx, pair, price, now)
	}
	return o.cfg.DivergenceRefresher.RefreshPair(ctx, pair, price, now)
}
