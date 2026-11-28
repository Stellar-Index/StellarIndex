package obs

import "github.com/prometheus/client_golang/prometheus"

// LendingOracleVerdictsTotal counts pool reserves the pool-oracle reader could not price, by pool and verdict.
var LendingOracleVerdictsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "stellarindex_lending_oracle_verdicts_total",
	Help: "Lending pool reserves whose own-oracle lastprice was not reproduced, by pool and verdict (oracle_unsupported, oracle_price_stale, ...).",
}, []string{"pool", "reason"})

// LendingHealthFillFailuresTotal counts /health requests whose snapshot read failed outright, by pool.
var LendingHealthFillFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "stellarindex_lending_health_fill_failures_total",
	Help: "GET /v1/lending/pools/{pool}/health requests answered with an error because the positions snapshot could not be read, by pool.",
}, []string{"pool"})

func init() {
	Registry.MustRegister(LendingOracleVerdictsTotal, LendingHealthFillFailuresTotal)
}
