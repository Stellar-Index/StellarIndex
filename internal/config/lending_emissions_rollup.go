package config

// LendingEmissionsRollupConfig gates the API's lending emissions USD rollup worker (migration 0240); off by default.
type LendingEmissionsRollupConfig struct {
	// Enabled starts the rollup loop in the API binary.
	Enabled bool `toml:"enabled" doc:"Start the lending emissions USD rollup in the API binary: it stores what /v1/lending/pools/{pool}/emissions/history serves at 1d, per pool, UTC day and stream line, into lending_emissions_usd_daily, one writer under an advisory lock. Off by default. The archival-node ansible role renders it from the stellarindex_lending_emissions_rollup_enabled inventory variable, itself default false. To enable: apply migration 0240, deploy, set the variable; the table fills itself from the emission event tables, no replay needed. The stellarindex_lending_emissions_rollup_stalled and _sweep_mismatch alerts cover it." default:"false"`
}
