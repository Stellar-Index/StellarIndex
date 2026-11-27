package obs

import "github.com/prometheus/client_golang/prometheus"

// LendingEmissionsRollupLastSuccessUnix is when the lending emissions USD rollup last finished a cycle holding its lock.
var LendingEmissionsRollupLastSuccessUnix = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "stellarindex_lending_emissions_rollup_last_success_unix",
	Help: "Unix seconds of the lending emissions USD rollup's last completed cycle that held its lock.",
})

// LendingEmissionsRollupStartedUnix is when this process started the rollup worker; 0 where it is disabled, so the stall
// alert covers a worker that has never succeeded since it started.
var LendingEmissionsRollupStartedUnix = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "stellarindex_lending_emissions_rollup_started_unix",
	Help: "Unix seconds this process started the lending emissions USD rollup worker; 0 when the worker is disabled.",
})

// LendingEmissionsRollupRowsValuedTotal counts lending_emissions_usd_daily rows written (new or changed).
var LendingEmissionsRollupRowsValuedTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "stellarindex_lending_emissions_rollup_rows_valued_total",
	Help: "lending_emissions_usd_daily rows the rollup wrote because their day was missing or differed from a fresh valuation.",
})

// LendingEmissionsRollupSweepMismatchesTotal counts stored days the daily sweep found to differ from a fresh valuation.
var LendingEmissionsRollupSweepMismatchesTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "stellarindex_lending_emissions_rollup_sweep_mismatches_total",
	Help: "Stored lending emissions USD days whose USD the sweep's fresh, non-degraded valuation of the same tokens did not reproduce exactly; each was rewritten.",
})

// LendingEmissionsRollupDaysNotValued is the last cycle's count of pool-days left unstored or stored unpriced, by reason.
var LendingEmissionsRollupDaysNotValued = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "stellarindex_lending_emissions_rollup_days_not_valued",
	Help: "Pool-days the last emissions rollup cycle left unstored (degraded: a read failed or was cut short; retried next cycle) or stored unpriced (refused: a read-time verdict or the token identity gate; the sweep retries daily).",
}, []string{"reason"})

func init() {
	Registry.MustRegister(LendingEmissionsRollupLastSuccessUnix, LendingEmissionsRollupStartedUnix,
		LendingEmissionsRollupRowsValuedTotal, LendingEmissionsRollupSweepMismatchesTotal, LendingEmissionsRollupDaysNotValued)
	for _, r := range []string{"degraded", "refused"} {
		LendingEmissionsRollupDaysNotValued.WithLabelValues(r)
	}
}
