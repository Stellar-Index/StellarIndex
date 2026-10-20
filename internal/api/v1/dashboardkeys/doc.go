// Package dashboardkeys implements the customer-dashboard API surface for managing API keys.
//
// Distinct from /v1/account/keys (the programmatic signup path): these routes are gated on a
// dashboard session rather than a bearer key, and use `internal/platform/postgresstore.APIKeyStore`
// as the source of truth. While the runtime auth validator still reads from Redis, new keys are
// mirror-written into the Redis store so a dashboard-created key works programmatically at once.
package dashboardkeys
