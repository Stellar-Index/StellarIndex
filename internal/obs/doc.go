// Package obs defines every Prometheus metric in the repository, so an alert in
// docs/operations/alerts-catalog.md greps to one definition, plus the HTTP metrics middleware.
//
// Names start with `stellarindex_`, except the client's own go_* and process_* and the conventional
// http_* names. Everything registers on [Registry], served by [Handler]. Unbounded labels (API key,
// asset id) never go on histograms; only counters and gauges with a bounded label set.
package obs
