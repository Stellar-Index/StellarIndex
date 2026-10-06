// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package obs

import (
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// LateTradeCAGGRefreshTotal counts the indexer's refreshes of the trades
// continuous aggregates over trades written older than a view's refresh
// policy reaches back (pipeline.LateTradeRefresher). Pre-seeded.
var LateTradeCAGGRefreshTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_late_trade_cagg_refresh_total",
		Help: "Refresh cycles of the trades continuous aggregates over trades written past a view's refresh-policy lookback, by outcome (ok|error). error = some buckets are not yet materialised; the refresh is retried with backoff.",
	},
	[]string{"outcome"},
)

// lateTradeOverdueSince is the unix-nano time the oldest unrefreshed late
// window fell due; 0 = none.
var lateTradeOverdueSince atomic.Int64

// SetLateTradeCAGGRefreshOverdueSince publishes when the oldest unrefreshed
// late-trade window fell due; the zero time clears it.
func SetLateTradeCAGGRefreshOverdueSince(t time.Time) {
	if t.IsZero() {
		lateTradeOverdueSince.Store(0)
		return
	}
	lateTradeOverdueSince.Store(t.UnixNano())
}

// lateTradeCAGGRefreshOverdueSeconds is computed at scrape time so a refresh
// that hangs still shows a growing age.
var lateTradeCAGGRefreshOverdueSeconds = prometheus.NewGaugeFunc(
	prometheus.GaugeOpts{
		Name: "stellarindex_late_trade_cagg_refresh_overdue_seconds",
		Help: "Seconds since the oldest late-trade window still unrefreshed fell due (past its view's rate limit); 0 when none is due. Grows through refresh errors and hangs alike; resets when the refresh succeeds.",
	},
	func() float64 {
		since := lateTradeOverdueSince.Load()
		if since == 0 {
			return 0
		}
		return max(time.Since(time.Unix(0, since)).Seconds(), 0)
	},
)

func init() {
	Registry.MustRegister(LateTradeCAGGRefreshTotal, lateTradeCAGGRefreshOverdueSeconds)
	for _, outcome := range []string{"ok", "error"} {
		LateTradeCAGGRefreshTotal.WithLabelValues(outcome)
	}
}
