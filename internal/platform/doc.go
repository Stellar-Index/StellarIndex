// Package platform models the customer + staff dashboard primitives
// from docs/architecture/platform-spec.md.
//
// AGT-08 (audit-2026-07-23): this doc previously narrated the auth
// flow, key management, and Postgres cutover as future per-week
// work; all of it has since shipped. Current reality:
//
//   - The dashboard auth flow (magic-link + session cookies) is
//     live — see internal/api/v1/dashboardauth, wired in
//     cmd/stellarindex-api/main.go's buildDashboardBundle.
//   - Key management (mint/list/revoke) is live via
//     internal/api/v1/dashboardkeys atop postgresstore.APIKeyStore.
//   - The Redis-only → Postgres-canonical key-storage cutover is
//     live and operator-controlled via api.auth_backend ("redis" |
//     "postgres" — see [config.APIConfig.AuthBackend]'s doc for the
//     canary/rollback procedure). auth.NewPostgresAPIKeyValidator
//     (internal/auth/apikey_postgres.go) is the read-through
//     validator: Postgres is the dashboard's source of truth
//     regardless of the flag; "postgres" additionally makes it the
//     RUNTIME validation path, with Redis as a cache.
//
// Package layout:
//
//	account.go      — Account aggregate
//	user.go         — User + Session aggregates
//	token.go        — MagicLinkToken + Invite aggregates
//	apikey.go       — Extended APIKey aggregate (replaces auth.APIKeyRecord
//	                  once the migration cuts over)
//	usage.go        — UsageEvent + UsageRollup wire types
//	billing.go      — Subscription + StripeEventLog aggregates
//	audit.go        — AuditLog entry
//	webhook.go      — CustomerWebhook + WebhookDelivery aggregates
//	errors.go       — Sentinel errors
package platform
