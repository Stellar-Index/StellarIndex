// Package pricealerts evaluates customer price-threshold alerts in the
// aggregator binary, beside the other customer-webhook producers and the
// freshest prices. CRUD lives in internal/api/v1/dashboardpricealerts.
//
// Each tick [Worker] compares every enabled `price_alerts` row with the
// latest CLOSED 1-minute VWAP for its pair (both stored orientations,
// every canonical.AssetAliases spelling of each leg, no last-trade or
// triangulation fallback). An armed alert whose condition holds and
// whose cooldown has passed enqueues a `price.alert` delivery into
// `webhook_deliveries` and disarms until a fresh price clears the
// condition, so one crossing notifies once.
//
// Alerts belong to one account, so delivery fans out through
// ListWebhooksForAccount, never the global Fanout. Off unless
// `[price_alerts] enabled = true`.
package pricealerts
