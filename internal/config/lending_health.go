package config

// LendingHealthConfig gates the API's GET /v1/lending/pools/{pool}/health route; off by default.
type LendingHealthConfig struct {
	// Enabled serves the route; when false it answers 404 lending-health-not-enabled.
	Enabled bool `toml:"enabled" doc:"Serve GET /v1/lending/pools/{pool}/health: each account's health factor on the pool's own oracle, computed on read at one lake ledger. Off by default; when off the route answers 404 lending-health-not-enabled. The archival-node ansible role renders it from the stellarindex_lending_health_enabled inventory variable, itself default false. No migration or replay: enable by setting the variable and deploying." default:"false"`
}
