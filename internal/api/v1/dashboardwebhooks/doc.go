// Package dashboardwebhooks serves the customer-facing
// `/v1/dashboard/webhooks*` CRUD surface that backs the dashboard's
// "incident callbacks" page (per the F-1270 audit-2026-05-12
// finding — proposal-promised Discord/Slack callbacks).
//
// Mounting + session auth follow the same pattern as
// `internal/api/v1/dashboardkeys`: a session cookie planted by
// dashboardauth.Middleware identifies the account; routes 401 on
// missing session, 403 when the role can't manage webhooks.
//
// Read path:
//
//   - GET    /v1/dashboard/webhooks         — list account's webhooks
//   - POST   /v1/dashboard/webhooks         — create
//   - PATCH  /v1/dashboard/webhooks/{id}    — update name / url / events / enabled
//   - DELETE /v1/dashboard/webhooks/{id}    — delete (cascades to deliveries)
//   - GET    /v1/dashboard/webhooks/{id}/deliveries — most-recent attempts
//
// Wire shape: bare JSON, not the v1 envelope — the dashboard
// surface is session-scoped and intentionally bypasses the
// envelope per `docs/reference/api-design.md §4.1` (F-1235).
//
// Persistence: every method delegates to the
// platform.WebhookStore wired by main.go (production:
// postgresstore.NewWebhookStore). The delivery worker that
// drains the queue lives in internal/customerwebhook and is
// orthogonal to these handlers.
package dashboardwebhooks
