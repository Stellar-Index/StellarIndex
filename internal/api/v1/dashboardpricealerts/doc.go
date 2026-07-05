// Package dashboardpricealerts serves the customer-facing
// `/v1/dashboard/price-alerts*` CRUD surface that backs the dashboard's
// price-alert management page (BACKLOG #60, RFP §6).
//
// Mounting + session auth mirror internal/api/v1/dashboardwebhooks: a
// session cookie planted by dashboardauth.Middleware identifies the
// account; routes 401 on a missing session, 403 when the role can't
// manage alerts. Wire shape is bare JSON, not the v1 envelope — the
// dashboard surface is session-scoped and intentionally bypasses the
// envelope (docs/reference/api-design.md §4.1, F-1235).
//
// Read path:
//
//   - GET    /v1/dashboard/price-alerts       — list the account's alerts
//   - POST   /v1/dashboard/price-alerts       — create
//   - PATCH  /v1/dashboard/price-alerts/{id}   — update pair / condition / threshold / cooldown / enabled
//   - DELETE /v1/dashboard/price-alerts/{id}   — delete
//
// Persistence: platform.PriceAlertStore (production:
// postgresstore.PriceAlertStore, migration 0080). The evaluator that
// checks these alerts against live prices and enqueues `price.alert`
// webhook deliveries runs in the AGGREGATOR binary (internal/pricealerts)
// and is orthogonal to these handlers — a customer can register an alert
// even while the evaluator is disabled; nothing fires until an operator
// flips `[price_alerts] enabled = true`.
//
// The per-account cap is tier-aware (platform.Tier.MaxPriceAlerts),
// overridable per tier via Config.AlertQuotas — same shape as the
// webhook + key ceilings.
package dashboardpricealerts
