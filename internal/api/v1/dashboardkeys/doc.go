// Package dashboardkeys implements the customer-dashboard API surface for managing API keys.
//
// Distinct from /v1/account/keys (the programmatic signup path): these routes are gated on a
// dashboard session rather than a bearer key, and use
// `internal/platform/postgresstore.APIKeyStore` (Postgres) as the source of truth.
//
// While the runtime auth validator still reads from Redis, this package mirror-writes new keys
// into the Redis store so a dashboard-created key works for programmatic calls immediately.
// The mirror-write goes away once cmd/stellarindex-api swaps to a Postgres read-through
// validator.
package dashboardkeys
