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

// refreshDivergenceAll asks the [DivergenceRefresher] to update each pair's `div:<base>/<quote>` entry from the shortest-window VWAP this
// Tick wrote (or the pinned last-known-good for a frozen pair).
// Best-effort: errors are counted in obs.DivergenceRefreshTotal{outcome} and logged; outcomes no_vwap, parse_error, refresh_error (entry
// NOT updated; old TTL keeps counting down).
func (o *Orchestrator) refreshDivergenceAll(ctx context.Context, now time.Time) {
	// Divergence is a SECONDARY best-effort guard; a panic in it must never take the PRIMARY VWAP refresh down. Compare recovers per-reference panics; this covers the outer per-pair loop.
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
	// Gate behind a minimum interval so the external-reference quota (CMC free tier = 10K/month) isn't exhausted; skips are visible as the outcome rate going to zero. Zero interval = every tick.
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
		// Time the full per-pair attempt (cache lookup, parse, HTTP fan-out), labelled by outcome so `ok` latency is charted apart from fast-fail paths.
		start := time.Now()
		raw, err := o.cachedVWAPAcrossAliases(ctx, pair, shortest)
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
			// Distinguish "all references dark" from a real refresh
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

// refreshPairDivergence hands the refresher the pair's cached VWAP, as a pinned price when frozen at that window ([Orchestrator.keepFrozenVWAPAlive]).
// Judged as fresh, the refused price would read as divergence, fire divergence_warning for a pair deliberately not updating, and feed a depressed input into the confidence that is itself a freeze leg. frozenLeg covers a freeze made this tick and one still in its hold.
func (o *Orchestrator) refreshPairDivergence(
	ctx context.Context, pair canonical.Pair, window time.Duration, price float64, now time.Time,
) error {
	if o.frozenLeg(pair, window) {
		return o.cfg.DivergenceRefresher.RefreshPinnedPair(ctx, pair, price, now)
	}
	return o.cfg.DivergenceRefresher.RefreshPair(ctx, pair, price, now)
}

// cachedVWAPAcrossAliases reads the pair's cached VWAP under each alias spelling, self first: native/fiat:USD's market is
// recorded under crypto:XLM, so a literal read never finds it and the pair never refreshes, as the price API already folds.
func (o *Orchestrator) cachedVWAPAcrossAliases(ctx context.Context, pair canonical.Pair, window time.Duration) (string, error) {
	var firstErr error
	for _, base := range canonical.AssetAliases(pair.Base) {
		for _, quote := range canonical.AssetAliases(pair.Quote) {
			raw, err := o.cache.Get(ctx, cachekeys.VWAP(base, quote, window).String()).Result()
			if err == nil {
				return raw, nil
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return "", firstErr
}
