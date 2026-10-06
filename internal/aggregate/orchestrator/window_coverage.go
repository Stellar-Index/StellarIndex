package orchestrator

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// fetchTradesDetectTruncation wraps the store fetch with the per-query
// cap and bumps AggregatorWindowTruncatedTotal (+ a WARN) when the
// returned row count hits the cap — i.e. the window held more trades
// than `MaxTradesPerWindow` and the VWAP is computed over only the
// newest `cap` of them. `target` is the aggregation target (for the log
// line); `fetch` is the actual pair queried (== target for the direct
// path, a stablecoin-backer pair under proxy expansion). A capped read
// also returns its oldest trade's timestamp as coveredFrom; zero otherwise.
func (o *Orchestrator) fetchTradesDetectTruncation(
	ctx context.Context, target, fetch canonical.Pair, from, to time.Time,
) (trades []canonical.Trade, coveredFrom time.Time, err error) {
	t, err := o.store.TradesInRange(ctx, fetch, from, to, o.cfg.MaxTradesPerWindow)
	if err != nil {
		return nil, time.Time{}, err
	}
	if len(t) > 0 && len(t) >= o.cfg.MaxTradesPerWindow {
		coveredFrom = t[0].Timestamp
		for i := range t {
			if t[i].Timestamp.Before(coveredFrom) {
				coveredFrom = t[i].Timestamp
			}
		}
		obs.AggregatorWindowTruncatedTotal.Inc()
		o.logger.Warn("trade window truncated at MaxTradesPerWindow — VWAP over newest-N slice only",
			"target", target.String(),
			"fetch_pair", fetch.String(),
			"cap", o.cfg.MaxTradesPerWindow,
			"from", from.UTC(),
			"to", to.UTC(),
		)
	}
	return t, coveredFrom, nil
}

// widenCoverage folds one source pair's capped-read point into c.
func widenCoverage(c cachekeys.WindowCoverage, coveredFrom time.Time) cachekeys.WindowCoverage {
	if coveredFrom.IsZero() {
		return c
	}
	if !c.Truncated || coveredFrom.After(c.CoveredFrom) {
		return cachekeys.WindowCoverage{Truncated: true, CoveredFrom: coveredFrom.UTC()}
	}
	return c
}
