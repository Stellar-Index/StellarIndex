// Package dashboardwebhooks serves the `/v1/dashboard/webhooks` CRUD
// routes over platform.WebhookStore: list, create, PATCH, DELETE
// (cascading to deliveries), rotate-secret (the old secret co-signs for
// 24 h) and recent deliveries.
//
// It is session-authenticated (401 without a session, 403 when the role
// cannot manage webhooks) and answers bare JSON, not the v1 envelope
// (docs/reference/api-design.md §4.1). internal/customerwebhook drains
// the delivery queue.
package dashboardwebhooks
