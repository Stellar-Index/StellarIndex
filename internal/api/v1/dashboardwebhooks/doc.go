// Package dashboardwebhooks serves the `/v1/dashboard/webhooks` CRUD routes over
// platform.WebhookStore, plus rotate-secret (the old secret co-signs for 24 h) and recent
// deliveries. DELETE cascades to deliveries.
//
// Routes are session-authenticated (401 without a session, 403 when the role cannot manage
// webhooks) and answer bare JSON, not the v1 envelope (docs/reference/api-design.md §4.1).
// internal/customerwebhook drains the delivery queue.
package dashboardwebhooks
