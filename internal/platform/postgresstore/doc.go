// Package postgresstore implements every store interface from
// internal/platform against the Postgres schema in
// migrations/0027_platform_v1_schema.
//
// AGT-08 (audit-2026-07-23): the "future work" phasing this doc
// previously described has shipped. Current reality:
//
//   - AccountStore + UserStore + TokenStore back the live
//     magic-link dashboard-auth flow (internal/api/v1/dashboardauth).
//   - APIKeyStore backs the runtime API auth path via
//     auth.NewPostgresAPIKeyValidator (internal/auth/apikey_postgres.go)
//     — a Redis-cached read-through wrapper, exactly as originally
//     planned. Whether it's the ACTIVE runtime validator (vs. the
//     legacy Redis-only path) is the operator-controlled
//     api.auth_backend cutover flag — see
//     [config.APIConfig.AuthBackend]'s doc for the canary/rollback
//     procedure. The dashboard's key-management surface
//     (dashboardkeys) always writes through this store regardless
//     of the flag.
//
// Tests use testcontainers-go to spin a transient Postgres +
// TimescaleDB container per package, matching the
// internal/storage/timescale pattern (see the per-store *_test.go
// files, e.g. apikey_store_test.go).
package postgresstore
