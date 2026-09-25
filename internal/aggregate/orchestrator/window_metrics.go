package orchestrator

import (
	"fmt"
	"math/big"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Per-refresh window observability (2026-08-28 outlier-trim redesign).
//
// The `outlier_storm` alert used to gate on the per-tick re-count of
// trimmed prints, which measures the whole-window MAD band's
// disagreement with the window tail — not venue disagreement. These
// helpers publish what the alert actually wants to know: the
// per-venue VWAP of the trade set the filter was handed, and how many
// trades each filter stage left standing in THIS refresh.

// windowLabel renders a refresh window as the metric label the alert
// rules match on: "5m", "1h", "24h" (seconds for anything not a whole
// minute).
func windowLabel(window time.Duration) string {
	switch {
	case window >= time.Hour && window%time.Hour == 0:
		return fmt.Sprintf("%dh", int(window/time.Hour))
	case window >= time.Minute && window%time.Minute == 0:
		return fmt.Sprintf("%dm", int(window/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(window/time.Second))
	}
}

// recordWindowStage sets AggregatorWindowTrades{stage} for this
// (pair, window) refresh.
func recordWindowStage(pair canonical.Pair, window time.Duration, stage string, n int) {
	obs.AggregatorWindowTrades.WithLabelValues(pair.String(), windowLabel(window), stage).Set(float64(n))
}

// recordWindowStageVolume is [recordWindowStage] plus the stage's base
// volume (AggregatorWindowBaseVolume). Volume is in whole units rather
// than at the stage's common smallest-unit scale: that scale is the
// stage's own maximum, so a trim that removed every 8dp print would put
// the survivors on a different scale from the class set.
func recordWindowStageVolume(pair canonical.Pair, window time.Duration, stage string, trades []canonical.Trade) {
	recordWindowStage(pair, window, stage, len(trades))
	bySource := make(map[string]*big.Int, 4)
	for i := range trades {
		sum, ok := bySource[trades[i].Source]
		if !ok {
			sum = new(big.Int)
			bySource[trades[i].Source] = sum
		}
		sum.Add(sum, trades[i].BaseAmount.BigInt())
	}
	total := new(big.Rat)
	for source, sum := range bySource {
		unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(amountScaleDecimalsFor(source))), nil)
		total.Add(total, new(big.Rat).SetFrac(sum, unit))
	}
	vol, _ := total.Float64() // i128:ok gauge boundary; operator signal, never served
	obs.AggregatorWindowBaseVolume.WithLabelValues(pair.String(), windowLabel(window), stage).Set(vol)
}

// recordVenueVWAPs publishes one AggregatorVenueVWAP series per source
// present in `trades` (the PRE-outlier, post-class set) and deletes
// the series of every source that was present on a previous refresh
// but is absent now — a venue that stopped trading must not keep a
// stale level in the max/min disagreement ratio.
//
// Prices are adjusted to the served scale with the same decimals
// lookup as [Orchestrator.computeNormalizedVWAP]; the float64
// conversion happens only at the gauge boundary (operator signal,
// never a served value).
func (o *Orchestrator) recordVenueVWAPs(pair canonical.Pair, window time.Duration, trades []canonical.Trade) {
	pairLabel, wLabel := pair.String(), windowLabel(window)
	// Drop every series for this (pair, window) first so absent
	// sources disappear; DeletePartialMatch is a no-op when nothing
	// matches (first refresh).
	obs.AggregatorVenueVWAP.DeletePartialMatch(prometheus.Labels{"pair": pairLabel, "window": wLabel})
	baseDec := aggregate.ResolveDecimals(o.cfg.DecimalsLookup, pair.Base)
	quoteDec := aggregate.ResolveDecimals(o.cfg.DecimalsLookup, pair.Quote)
	for _, c := range aggregate.SourceContributions(trades) {
		if c.BaseVolume == nil || c.BaseVolume.Sign() <= 0 || c.QuoteVolume == nil {
			continue
		}
		vwap := aggregate.AdjustPrice(new(big.Rat).SetFrac(c.QuoteVolume, c.BaseVolume), baseDec, quoteDec)
		f, _ := vwap.Float64() // i128:ok Prometheus per-venue VWAP gauge, not served
		obs.AggregatorVenueVWAP.WithLabelValues(pairLabel, wLabel, c.Source).Set(f)
	}
}
