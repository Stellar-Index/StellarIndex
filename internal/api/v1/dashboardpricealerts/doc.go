// Package dashboardpricealerts serves the `/v1/dashboard/price-alerts`
// CRUD routes (list, create, PATCH and DELETE by id) over
// platform.PriceAlertStore.
//
// Like the other dashboard surfaces it is session-authenticated (401
// without a session, 403 when the role cannot manage alerts) and answers
// bare JSON, not the v1 envelope (docs/reference/api-design.md §4.1).
// The per-account cap is platform.Tier.MaxPriceAlerts, overridable per
// tier through Config.AlertQuotas.
//
// Alerts are evaluated by internal/pricealerts in the aggregator; a
// customer can register one while that evaluator is off, and nothing
// fires until `[price_alerts] enabled = true`.
package dashboardpricealerts
