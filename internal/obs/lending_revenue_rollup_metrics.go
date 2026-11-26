package obs

import "github.com/prometheus/client_golang/prometheus"

// LendingRevenueRollupLastSuccessUnix is when the lending revenue USD rollup last finished a cycle holding its lock.
var LendingRevenueRollupLastSuccessUnix = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "stellarindex_lending_revenue_rollup_last_success_unix",
	Help: "Unix seconds of the lending revenue USD rollup's last completed cycle that held its lock.",
})

// LendingRevenueRollupRowsValuedTotal counts lending_revenue_usd_daily rows written (new or changed).
var LendingRevenueRollupRowsValuedTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "stellarindex_lending_revenue_rollup_rows_valued_total",
	Help: "lending_revenue_usd_daily rows the rollup wrote because they were missing or differed from a fresh valuation.",
})

// LendingRevenueRollupSweepMismatchesTotal counts stored days the daily sweep found to differ from a fresh day walk.
var LendingRevenueRollupSweepMismatchesTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "stellarindex_lending_revenue_rollup_sweep_mismatches_total",
	Help: "Stored lending revenue USD days whose value the sweep's fresh, non-degraded day walk did not reproduce exactly; each was rewritten.",
})

// LendingRevenueRollupDaysNotValued is the last cycle's count of reserve-days left unstored, by reason (degraded, refused).
var LendingRevenueRollupDaysNotValued = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "stellarindex_lending_revenue_rollup_days_not_valued",
	Help: "Reserve-days the last rollup cycle left unstored: degraded (a price read failed or was cut short; retried next cycle) or refused (a read-time verdict; the sweep retries daily).",
}, []string{"reason"})

func init() {
	Registry.MustRegister(LendingRevenueRollupLastSuccessUnix, LendingRevenueRollupRowsValuedTotal,
		LendingRevenueRollupSweepMismatchesTotal, LendingRevenueRollupDaysNotValued)
	for _, r := range []string{"degraded", "refused"} {
		LendingRevenueRollupDaysNotValued.WithLabelValues(r)
	}
}
