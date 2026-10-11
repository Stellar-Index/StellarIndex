package orchestrator

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// fetchTradesDetectTruncation wraps the store fetch with the per-query cap, warning and counting when the cap is hit (VWAP then covers only the newest cap trades).
// `target` is the aggregation target, `fetch` the pair actually queried (a stablecoin-backer pair under proxy expansion). A capped read returns its oldest trade's timestamp as coveredFrom; zero otherwise.
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
