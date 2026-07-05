// Package pricealerts is the aggregator-side evaluator for
// customer-registered price-threshold alerts (BACKLOG #60, RFP §6).
//
// The CRUD half (register / list / update / delete an alert) lives on
// the API binary's dashboard surface
// (internal/api/v1/dashboardpricealerts). This package is the other
// half: a small ticker loop that runs in the AGGREGATOR binary — the
// process where price data is freshest (it computes + writes prices_1m)
// and where the existing customer-webhook fan-out producers already
// live (anomaly.freeze, divergence.firing). Placing the evaluator here
// keeps every webhook-producing path in one binary and lets the sweep
// read the latest closed VWAP the aggregator just materialised.
//
// Each tick the [Worker] lists every enabled row from `price_alerts`,
// compares each against the latest CLOSED 1-minute VWAP for its pair
// (the same reader the API price surface uses, which combines both
// stored orientations), and — for alerts whose condition holds and
// whose cooldown has elapsed since last_fired_at — enqueues a
// `price.alert` delivery into the existing customer-webhook queue
// (`webhook_deliveries`) for the OWNING account's subscribed webhooks.
// Delivery (HMAC-sign + POST + retry) is done by the orthogonal
// internal/customerwebhook worker in the API binary; no change was
// needed there — the queue is event-type agnostic.
//
// Account scoping: unlike the operational webhook events, a price alert
// belongs to one account, so the evaluator fans out via
// ListWebhooksForAccount (NOT the global Fanout) — one account's alerts
// never reach another account's webhooks.
//
// Off by default: the goroutine is only started when
// `[price_alerts] enabled = true` (config.PriceAlertsConfig).
package pricealerts
