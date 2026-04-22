// Package obs centralises Prometheus metric definitions and the
// HTTP middleware that emits HTTP-level metrics.
//
// # Why centralised?
//
// Every metric in the repo is defined here. When an alert in
// docs/operations/alerts-catalog.md fires on metric `X`, grep
// finds its definition in one file. Distributing definitions
// across packages makes consistent naming + help text a matter
// of discipline rather than a single source of truth.
//
// # Metric naming
//
// All metrics start with `ratesengine_` except for the
// language-native `go_*` and `process_*` that prometheus client
// emits automatically, plus the HTTP ones which follow
// Prometheus best-practice naming (`http_request_duration_seconds`
// etc.) for dashboard portability.
//
// # Registration
//
// Every metric registers to [Registry] at init. Binaries expose
// that registry via [Handler] which returns an http.Handler for
// /metrics endpoints.
//
// # Label cardinality
//
// Labels that could blow up cardinality (API key, asset_id) are
// NOT used on histograms — only on counters + gauges where the
// label set is bounded. High-cardinality metrics go to traces, not
// Prometheus.
package obs
