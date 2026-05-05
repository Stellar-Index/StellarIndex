// Package dashboardkeys implements the customer-dashboard API
// surface for managing API keys.
//
// Distinct from /v1/account/keys (the existing programmatic
// signup-flow path that mints a key in exchange for an email
// address): the routes here are gated on a dashboard SESSION
// rather than a bearer key, and read/write the
// `internal/platform/postgresstore.APIKeyStore` (Postgres) as
// the source of truth.
//
// During Phase 1 the runtime auth validator still reads from
// Redis; this package mirror-writes new keys into the Redis
// store too so a key created from the dashboard works for
// programmatic API calls immediately. The Phase 1 cutover is
// the cmd/ratesengine-api wiring that swaps to a Postgres-
// backed read-through validator; once that lands the mirror-
// write goes away and Postgres is canonical.
package dashboardkeys
