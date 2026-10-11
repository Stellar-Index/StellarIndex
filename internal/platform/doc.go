// Package platform models the customer and staff dashboard aggregates (accounts, users, sessions, tokens, API keys, usage, audit log, webhooks); postgresstore implements its stores.
// Postgres is the source of truth for API keys; `api.auth_backend` ([config.APIConfig.AuthBackend]) chooses whether runtime validation reads it or Redis alone.
package platform
