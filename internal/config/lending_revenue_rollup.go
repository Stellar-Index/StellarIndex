package config

// LendingRevenueRollupConfig gates the API's lending revenue USD rollup worker (migration 0239); off by default.
type LendingRevenueRollupConfig struct {
	// Enabled starts the rollup loop in the API binary.
	Enabled bool `toml:"enabled" doc:"Start the lending revenue USD rollup in the API binary: it values each pool reserve's interest per UTC day into lending_revenue_usd_daily, one writer under an advisory lock. Off by default. The archival-node ansible role renders it from the stellarindex_lending_revenue_rollup_enabled inventory variable, itself default false." default:"false"`
}
