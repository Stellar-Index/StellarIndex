// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package obs

import "github.com/prometheus/client_golang/prometheus"

// LateTradeCAGGRefreshTotal counts the indexer's refreshes of the trades
// continuous aggregates over trades written older than a view's refresh
// policy reaches back (pipeline.LateTradeRefresher). Pre-seeded.
var LateTradeCAGGRefreshTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_late_trade_cagg_refresh_total",
		Help: "Refreshes of the trades continuous aggregates over trades written past a view's refresh-policy lookback, by outcome (ok|error). error = those buckets are not yet materialised; the refresh is retried with backoff.",
	},
	[]string{"outcome"},
)

func init() {
	Registry.MustRegister(LateTradeCAGGRefreshTotal)
	for _, outcome := range []string{"ok", "error"} {
		LateTradeCAGGRefreshTotal.WithLabelValues(outcome)
	}
}
