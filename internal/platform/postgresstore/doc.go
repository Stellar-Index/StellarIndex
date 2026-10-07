// Package postgresstore implements internal/platform's store interfaces
// over the schema from migrations/0027_platform_v1_schema. The dashboard
// always writes keys through [APIKeyStore]; `api.auth_backend` decides
// whether runtime auth reads them here ([config.APIConfig.AuthBackend]).
// Tests run against a real Postgres via testcontainers-go.
package postgresstore
