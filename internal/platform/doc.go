// Package platform models the customer and staff dashboard aggregates:
// accounts, users and sessions, magic-link tokens and invites, API keys,
// usage, audit log and customer webhooks; postgresstore implements its
// stores.
//
// Postgres is the source of truth for API keys. `api.auth_backend`
// ([config.APIConfig.AuthBackend]) chooses whether runtime validation
// reads it (through the Redis-cached auth.NewPostgresAPIKeyValidator) or
// Redis alone; its doc has the canary and rollback procedure.
package platform
