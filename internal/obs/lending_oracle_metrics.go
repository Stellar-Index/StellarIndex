package obs

import "github.com/prometheus/client_golang/prometheus"

// LendingOracleVerdictsTotal counts pool reserves the pool-oracle reader could not price, by pool and verdict.
var LendingOracleVerdictsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "stellarindex_lending_oracle_verdicts_total",
	Help: "Lending pool reserves whose own-oracle lastprice was not reproduced, by pool and verdict (oracle_unsupported, oracle_price_stale, ...).",
}, []string{"pool", "reason"})

func init() {
	Registry.MustRegister(LendingOracleVerdictsTotal)
}
