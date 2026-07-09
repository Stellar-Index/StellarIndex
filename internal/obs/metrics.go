package obs

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds every metric the binary emits. Binaries expose
// it at /metrics via [Handler].
//
// We use a non-default registry (not prometheus.DefaultRegisterer)
// so tests can spin up isolated registries without state leakage +
// the default process/go collectors are opt-in here.
var Registry = prometheus.NewRegistry()

func init() {
	// Register language-native metrics — heap, goroutines,
	// gc pauses, open file descriptors, process uptime.
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	// Register every application metric (split into a helper to keep
	// init() under the funlen ceiling as the metric set grows).
	registerAppMetrics()
}

func registerAppMetrics() {
	Registry.MustRegister(
		HTTPRequestsTotal,
		HTTPRequestDuration,
		HTTPRequestSuccessDuration,
		IngestGapLedgers,
		IngestGapCount,
		IngestGapMaxSize,
		IngestSourceDistinctLedgers,
		IngestGapDetectorTip,
		IngestGapDetectorRunsTotal,
		IngestGapDetectorDurationSeconds,
		IngestGapDetectorLastSuccessUnix,
		ProjectorLagLedgers,
		ProjectorRunsTotal,
		ProjectorEventsDecoded,
		ProjectorCycleDurationSeconds,
		APICacheOpsTotal,

		SourceEventsTotal,
		SourceLastEventUnix,
		SourceLastInsertUnix,
		SourceEnabled,
		SourceMatchedEventsTotal,
		SourceDecodeErrorsTotal,
		SourceUnknownSymbolsTotal,
		SourceOrphanEventsTotal,
		ExternalPollerPollsTotal,
		ExternalPollerLastSuccessUnix,
		ExternalFXLastQuoteUnix,
		ExternalDustDroppedTotal,
		CEXStreamDisconnectTotal,
		DiscoveryDroppedHitsTotal,
		DiscoverySkippedHitsTotal,
		SourceInsertErrorsTotal,
		RateLimitFailOpenTotal,
		Sep1CacheOpsTotal,
		CursorLastLedger,
		DivergenceRefreshTotal,
		TradeInsertsTotal,
		TradeInsertOutcomeTotal,
		DexTradeUnitRatioTotal,
		TradeInsertRetriesTotal,
		TradeInsertBufferDepth,
		StreamPublishTotal,

		PriceStalenessSeconds,
		OracleLastUpdateUnix,
		OracleResolutionSeconds,

		AggregatorTicksTotal,
		AggregatorVWAPWritesTotal,
		AggregatorVWAPCacheWriteErrorsTotal,
		AggregatorEmptyWindowsTotal,
		AggregatorWindowTruncatedTotal,
		AggregatorStreamPublishTotal,
		APIStreamSubscribeTotal,
		APICORSDecisionsTotal,
		CustomerWebhookDeliveryAttemptsTotal,
		AggregatorDroppedTradesTotal,
		AggregatorDroppedWindowsTotal,

		SupplyCrossCheckDivergenceStroops,
		SupplyCrossCheckTotal,
		SupplyDivergenceRatio,
		SupplyDivergenceTotal,

		AnomalyFreezeEngagedTotal,
		AnomalyFreezeRecoveredTotal,
		AnomalyFreezeRecoverySweepsTotal,
		AggregatorTriangulationsTotal,
		AggregatorFXSnapFallbackTotal,
		AggregatorBaselineRefreshTotal,
		AggregatorSupplyRefreshTotal,
		SEP41SupplyRollupAdvancesTotal,
		AggregatorConfidenceComputeTotal,

		VerifyArchiveLedgersVerified,
		VerifyArchiveCurrentLedger,
		VerifyArchiveCheckpointsTotal,
		VerifyArchiveMismatchesTotal,

		StripePlatformSyncErrorsTotal,

		ChLiveSinkLedgersTotal,

		MarketsSkippedRowsTotal,
	)
	registerAppMetricsTail()
}

// registerAppMetricsTail registers the remainder of the app metric set
// — split out of registerAppMetrics purely to keep each function under
// the funlen ceiling as the metric set grows (same reason init() calls
// registerAppMetrics).
func registerAppMetricsTail() {
	Registry.MustRegister(
		MEVDetectRunsTotal,
		MEVEventsInsertedTotal,
		MEVDetectDurationSeconds,

		PostgresPingTotal,
		PostgresPingFailureStreak,
		TLSCertNotAfterUnix,
		TLSCertProbeTotal,

		CustomerWebhookDeliveryDurationSeconds,
		DivergenceRefreshDurationSeconds,
		SupplyDivergenceDurationSeconds,
		AggregatorSupplyRefreshDurationSeconds,
		SEP41SupplyRollupAdvanceDurationSeconds,
		AnomalyFreezeRecoverySweepDurationSeconds,

		UsageRollupSweepsTotal,
		UsageRollupSweepDurationSeconds,

		ProtocolEventsRollupSweepsTotal,
		ProtocolEventsRollupSweepDurationSeconds,

		AssetVolumeRollupSweepsTotal,
		AssetVolumeRollupSweepDurationSeconds,

		PriceAlertEvalTotal,
		PriceAlertEvalDurationSeconds,

		SignupReaperRunsTotal,
		SignupReaperRunDurationSeconds,
		SignupReaperRowsDeletedTotal,

		DEXTradeNonstandardDecimalsTotal,
		PriceServeDeclinedNonstandardDecimalsTotal,
		NonstandardDecimalsCacheRefreshFailuresTotal,

		HashdbAppendTotal,
		HashdbAppendDurationSeconds,
		HashdbVerifyRunsTotal,
		HashdbVerifyRunDurationSeconds,
		HashdbDriftTotal,
	)

	// F-0033 closure: pre-seed zero-valued series for the
	// bounded-cardinality counters whose alert rules use rate() /
	// increase() but whose label combinations never appear in
	// /metrics output until the first event fires. Without
	// pre-seeding, PromQL queries against e.g.
	// `rate(stellarindex_aggregator_triangulations_total{outcome="ok"}[15m])`
	// resolve to "no data" (gap, not zero) until the first
	// triangulation succeeds — which makes `absent()` / `<= 0` checks
	// ambiguous and the audit found multiple alerts whose underlying
	// metric was "missing from scrape output." That was a Prometheus
	// client-library quirk, not a code bug: counters only register a
	// series after the first .Inc on a given label combo.
	//
	// Only counters with a *bounded, well-known* label set are
	// pre-seeded here. AggregatorFXSnapFallbackTotal's `leg` label
	// is per-pair (unbounded by operator config) so it stays
	// emit-on-error.
	for _, outcome := range []string{"ok", "missing_leg", "parse_error", "redis_error"} {
		AggregatorTriangulationsTotal.WithLabelValues(outcome)
	}
	for _, op := range []string{"get_account", "upsert_subscription", "account_update", "list_keys", "key_update", "key_cache_invalidate"} {
		StripePlatformSyncErrorsTotal.WithLabelValues(op)
	}
	for _, outcome := range []string{"written", "buffered", "dropped", "errored"} {
		ChLiveSinkLedgersTotal.WithLabelValues(outcome)
	}
	for _, outcome := range []string{"ok", "scan_error", "write_error"} {
		MEVDetectRunsTotal.WithLabelValues(outcome)
	}
	for _, outcome := range []string{"ok", "scan_error", "sink_error"} {
		UsageRollupSweepsTotal.WithLabelValues(outcome)
	}
	for _, outcome := range []string{"ok", "refresh_error"} {
		ProtocolEventsRollupSweepsTotal.WithLabelValues(outcome)
		AssetVolumeRollupSweepsTotal.WithLabelValues(outcome)
	}
	for _, outcome := range []string{"ok", "list_error", "partial_error"} {
		PriceAlertEvalTotal.WithLabelValues(outcome)
	}
	for _, outcome := range []string{"ok", "error"} {
		SignupReaperRunsTotal.WithLabelValues(outcome)
	}
	// Bounded outcome set for the 2026-07-06 backpressure retry counter
	// so the `trade_insert_backpressure` alert's rate() query reads a
	// real zero (not "no data") before the first outage.
	for _, outcome := range []string{"retry", "recovered", "abandoned"} {
		TradeInsertRetriesTotal.WithLabelValues(outcome)
	}
	// Supply cross-check outcomes — bounded, well-known label set so the
	// `no_reference` "checker running blind" query reads a real zero
	// (not "no data") before the first supply cross-check tick.
	for _, outcome := range []string{"ok", "divergent", "no_reference", "refresh_error"} {
		SupplyDivergenceTotal.WithLabelValues(outcome)
	}
	// hashdb append/verify outcomes — bounded, well-known label sets so
	// the append-error-rate and verify-run-failure queries read a real
	// zero (not "no data") on a freshly-enabled region before the first
	// tick / first ledger.
	for _, outcome := range []string{"ok", "error"} {
		HashdbAppendTotal.WithLabelValues(outcome)
		HashdbVerifyRunsTotal.WithLabelValues(outcome)
	}
	HashdbVerifyRunsTotal.WithLabelValues("drift")
}

// Handler returns an http.Handler that serves Prometheus-formatted
// metrics from [Registry]. Binaries mount this at /metrics.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{
		// Compression removes redundant labels from the scrape body
		// at the cost of a tiny bit of CPU per scrape.
		EnableOpenMetrics: true,
	})
}

// ─── HTTP-layer metrics ──────────────────────────────────────────

// HTTPRequestsTotal — count of HTTP requests served, by method,
// route pattern (not raw URL — avoids cardinality blow-up on IDs),
// and status class.
//
// Alert rules reference this via `http_requests_total{status=~"5..", job=~"stellarindex[-_]api"}`.
// (F-1276, audit-2026-05-13: scrape jobs use `stellarindex_api` on HA
// multi-host and `stellarindex-api` on R1; rules match both via regex.
// Earlier comment said `job="api"` which never matched any series.)
var HTTPRequestsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Count of HTTP requests served by the API, labelled by method, route pattern, and status.",
	},
	[]string{"method", "route", "status"},
)

// HTTPRequestDuration — histogram of request latency in seconds,
// from first byte in to last byte out.
//
// Buckets cover 1 ms → 10 s so the p95 ≤ 200 ms SLA target lands
// inside a bucket boundary (0.2) for accurate p95/p99 readouts.
var HTTPRequestDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name: "http_request_duration_seconds",
		Help: "Request latency histogram, labelled by method + route pattern.",
		// Matches Freighter SLA: p95 ≤ 200ms, p99 ≤ 500ms. Buckets
		// are picked so the .2 + .5 boundaries land exactly.
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.5, 1, 2.5, 5, 10},
	},
	[]string{"method", "route"},
)

// IngestGapLedgers is the **data-derived** ingest-gap signal: total
// missing ledgers in contiguous gaps >= the worker's threshold per
// source. Reported by [internal/storage/timescale.GapDetector]
// against the soroban_events hypertable on a periodic timer.
//
// Pairs with IngestGapCount + IngestGapMaxSize to feed an alert
// rule that fires when an ingest gap forms (e.g. the F-0020
// cascade-window soroban_events writer halt — the alert would have
// caught the 92,737-ledger gap within one detector cycle instead
// of requiring an audit pass to surface).
//
// Labels:
//   - `source` — semantic source identifier (e.g. blend-positions,
//     soroban-events, sep41-transfers). One source may span
//     multiple tables (Blend's three projections).
//   - `table` — the actual Postgres hypertable name. Disambiguates
//     when one source has multiple targets.
//
// SDEX uses a separate ingest path (trades hypertable, classic
// not Soroban); its detection lives under {source="sdex",
// table="trades"} as of rc.88 / PR #3.
//
// Gauge semantics: set to current value on every detector cycle;
// reset to 0 when the worker finds no gaps >= threshold. NOT a
// counter — operators read absolute value, not deltas.
var IngestGapLedgers = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_ingest_gap_ledgers",
		Help: "Total missing ledgers in contiguous data-coverage gaps (>= detector min-gap-size) per (source, table). Data-derived; complements cursor-coverage density.",
	},
	[]string{"source", "table"},
)

// IngestGapCount counts the number of contiguous gaps per source
// at the same detector cycle that updates IngestGapLedgers. A
// single 100K-ledger gap and 100 ten-ledger gaps both report 1000
// missing ledgers in IngestGapLedgers but very different shapes;
// operators chart this gauge to distinguish "one big halt"
// (typical cascade signature) from "many small drops" (typical
// flaky-write pattern).
var IngestGapCount = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_ingest_gap_count",
		Help: "Number of contiguous data-coverage gaps (>= detector min-gap-size) per (source, table) at the most recent detector cycle.",
	},
	[]string{"source", "table"},
)

// IngestGapMaxSize reports the size of the largest contiguous gap
// per source. Useful when the operator wants to know "how big is
// the biggest hole" without parsing the gap list. Always equals
// max(IngestGapLedgers / IngestGapCount) under the cycle's
// invariant, but exposed directly so a single PromQL query can
// alert on it.
var IngestGapMaxSize = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_ingest_gap_max_size_ledgers",
		Help: "Size of the largest contiguous data-coverage gap per (source, table) at the most recent detector cycle.",
	},
	[]string{"source", "table"},
)

// IngestGapDetectorRunsTotal counts detector cycle attempts +
// outcomes. Operators read its rate to confirm the detector is
// alive even when IngestGapLedgers is steady at zero (which is the
// healthy state). Outcome ∈ {ok, error} — the latter increments
// when the underlying SQL fails (typically a transient Postgres
// connection blip).
var IngestGapDetectorRunsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_ingest_gap_detector_runs_total",
		Help: "Periodic data-gap detector runs, by (source, table, outcome). Rate goes to zero if the worker has wedged.",
	},
	[]string{"source", "table", "outcome"},
)

// IngestSourceDistinctLedgers is the **data-derived covered-
// ledgers** signal: COUNT(DISTINCT ledger) per (source, table)
// over the detector's trailing scan window [from, tip]. Together
// with `IngestGapMaxSize` powers the ADR-0031 data-derived coverage
// projection.
//
// Density = IngestSourceDistinctLedgers / (tip - from + 1).
// Gap-free = 1 - IngestGapMaxSize / (tip - from + 1).
//
// The `from` lower bound is the trailing window the detector scans
// (2026-07-06 IO-saturation incident) — steady state ~[last high-
// water, tip], first run within FirstScanCap of tip, never the full
// [genesis, tip]. Deep-history coverage is the ADR-0033 completeness
// verdict's domain, not this gauge.
//
// Emitted by the gap detector at the same cadence as the gap
// gauges (one COUNT query alongside the LAG-over-DISTINCT scan
// per target per cycle).
var IngestSourceDistinctLedgers = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_ingest_source_distinct_ledgers",
		Help: "Distinct-ledger count per (source, table) at the most recent gap-detector cycle. Numerator of the ADR-0031 data-derived density signal.",
	},
	[]string{"source", "table"},
)

// IngestGapDetectorTip is the live ledgerstream cursor's
// `last_ledger` value at the most recent gap-detector cycle's
// start — the upper bound `tip` used by every per-target scan. The
// per-target density denominator is `tip - from + 1` where `from`
// is the target's trailing-window lower bound (2026-07-06 incident),
// so this gauge alone is no longer sufficient to recompute density;
// read the persisted source_coverage_snapshots row for that.
//
// Single-vector gauge (no `source`/`table` labels) because every
// target uses the same tip in the same cycle; emitting per-target
// would be redundant + the consumer needs only one read.
var IngestGapDetectorTip = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_ingest_gap_detector_tip_ledger",
		Help: "Live ledgerstream tip ledger at the most recent gap-detector cycle. Upper bound of the per-target trailing scan window; the lower bound is per-source (see source_coverage_snapshots).",
	},
)

// IngestGapDetectorDurationSeconds measures detector-cycle latency.
// Operators chart `outcome=ok` p95/p99 separately from `error`
// outcomes (see wave-100 obstest patterns).
var IngestGapDetectorDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name: "stellarindex_ingest_gap_detector_duration_seconds",
		Help: "Wall-clock duration of one data-gap detector cycle, by (source, table, outcome). Buckets extend to 600s because soroban_events scans on r1 measure ~300s against ~50M distinct ledgers.",
		// Extended buckets to 600 because the soroban_events scan on
		// r1 is ~300s; the original 60s cap put every successful
		// scan in the overflow bucket.
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600},
	},
	[]string{"source", "table", "outcome"},
)

// IngestGapDetectorLastSuccessUnix is the wall-clock timestamp (Unix
// seconds) of the most recent SUCCESSFUL per-(source, table) gap scan.
// It is the reset-proof liveness primitive the
// `stellarindex_ingest_gap_detector_silent` alert keys off, replacing
// the fragile `rate(runs_total{outcome="ok"}[7h]) == 0` construct.
//
// Why a timestamp gauge, not rate() over the counter: the heavy targets
// (sdex/trades, soroban-events/soroban_events) scan on a 6h
// ScanCadence, so their `ok` counter increments only once every 6h.
// When the aggregator restarts more often than that (deploys, incident
// recoveries), each process life records exactly ONE ok, pinning the
// counter at 1. Because the value is 1 both before AND after the
// restart, Prometheus counter-reset detection never triggers (it only
// fires on a DECREASE), so `rate(...ok[7h])` reads a flat line and
// evaluates to 0 — the silent alert false-fired for >7h even though
// every startup scan succeeded (live incident 2026-07-06). A wall-clock
// gauge is immune: the startup scan re-stamps it to now(), so a healthy
// restart immediately clears staleness, while a genuinely wedged
// target's stamp simply stops advancing and `time() - gauge` grows past
// the alert threshold.
//
// Advances ONLY on a clean scan; a scan that errors or times out leaves
// the previous stamp untouched. A target that has NEVER once succeeded
// since process start emits no series here — that case is covered by the
// paired `runs_total{outcome="error"}` rate, not this gauge.
var IngestGapDetectorLastSuccessUnix = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_ingest_gap_detector_last_success_unix",
		Help: "Wall-clock timestamp (Unix seconds) of the most recent successful gap-detector scan per (source, table). The silent-detector alert keys off its staleness; reset-proof across restarts, unlike rate() over the once-per-6h runs_total counter.",
	},
	[]string{"source", "table"},
)

// ProjectorLagLedgers is how far behind tip each projector source
// currently is, in ledgers. The projector reads soroban_events
// (raw) and writes per-source classifier tables; this gauge =
// tip - last_projected_ledger. ADR-0032.
//
// Steady-state value is 0-few-ledgers when the projector is
// keeping up. A sustained > 1000 value means the projector is
// falling behind (decoder error storm, downstream sink saturated,
// or projector stopped). Paging alert
// `stellarindex_projector_lag_high` fires on sustained drift.
var ProjectorLagLedgers = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_projector_lag_ledgers",
		Help: "Per-source projector lag in ledgers (tip - last_projected). 0 = caught up. Sustained > 1000 = falling behind.",
	},
	[]string{"source"},
)

// ProjectorRunsTotal counts projector cycle outcomes per source.
// `outcome` ∈ {ok, error, idle}; rate is the alive-check (zero
// rate sustained 5+ minutes means the source's loop wedged).
var ProjectorRunsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_projector_runs_total",
		Help: "Per-source projector cycle outcomes (ok, error, idle). Rate goes to zero if the source's loop has wedged.",
	},
	[]string{"source", "outcome"},
)

// ProjectorEventsDecoded counts events the projector emitted
// through the sink (or that failed decode). `outcome` ∈ {ok,
// decode_error}. Operators chart `rate(ok[5m])` against the
// equivalent dispatcher counter during Phase 3 parallel mode to
// verify the projector keeps pace with live ingest.
var ProjectorEventsDecoded = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_projector_events_decoded_total",
		Help: "Per-source events the projector decoded + emitted (ok) or failed to decode (decode_error). Compare ok-rate against dispatcher equivalent to gauge parallel-mode parity.",
	},
	[]string{"source", "outcome"},
)

// ProjectorCycleDurationSeconds measures wall-clock per cycle.
var ProjectorCycleDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_projector_cycle_duration_seconds",
		Help:    "Wall-clock duration of one projector cycle per source.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
	},
	[]string{"source"},
)

// HTTPRequestSuccessDuration is the success-only twin of
// HTTPRequestDuration: same buckets / labels, but the middleware
// only records into this histogram when the response status is NOT
// 5xx. Pair the two metrics in the SLO ratio so a fast-5xx burns
// the latency budget (numerator excludes the error; denominator
// counts everything):
//
//	api_slow_request_ratio =
//	  sum(rate(http_request_success_duration_seconds_bucket{le="0.2",...}[w]))
//	  / sum(rate(http_request_duration_seconds_count{...}[w]))
//
// Before this metric existed, both numerator and denominator used
// the same `_duration_seconds` series — a fast 500 landed in both
// and reported as "good" against the latency SLO even though the
// customer experience was a hard outage (F-0105, audit-2026-05-26).
// The availability SLO (http_requests_total{status_class="5xx"})
// is unchanged — it stays the authority for 5xx rate, and this
// metric is only about getting the latency SLO right.
//
// Same buckets as HTTPRequestDuration so the
// `le="0.2"` filter lands on the identical boundary across both.
var HTTPRequestSuccessDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "http_request_success_duration_seconds",
		Help:    "Request latency histogram for non-5xx responses only. Pair with http_request_duration_seconds_count for SLO ratios that burn budget on fast 5xx.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.5, 1, 2.5, 5, 10},
	},
	[]string{"method", "route"},
)

// APICacheOpsTotal — every read through an in-memory cache wrapper
// (`v1.CachedMarketsReader`, `v1.CachedAssetsReader`, …) increments
// this counter. The `result` label is `hit` (returned cached value)
// or `miss` (called upstream). The `op` label names the cached
// method (e.g. `all_pools`, `distinct_pairs`, `list_coins`).
//
// Why: prewarm goroutines warm cache keys that MUST match what
// handlers look up. If those keys drift (different filter shape,
// different limit, different order), every user request becomes a
// miss while the prewarm slot sits unread. The bug is invisible to
// tests + log-greps, so an operator dashboard on hit-rate is the
// cheapest detector.
//
// Alert idea: `rate(stellarindex_api_cache_ops_total{result="miss"}
// [5m]) / rate(stellarindex_api_cache_ops_total[5m]) > 0.5` sustained
// 10 min on any (cache, op) is suspicious — prewarm should keep
// hot ops > 90% hit.
var APICacheOpsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_api_cache_ops_total",
		Help: "Cache reads through API in-memory cache wrappers, labelled by cache name + op + result (hit|miss).",
	},
	[]string{"cache", "op", "result"},
)

// ─── Ingestion-layer metrics ─────────────────────────────────────

// SourceEventsTotal — per-source event count; increments on every
// event the consumer emits to its out-channel.
var SourceEventsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_source_events_total",
		Help: "Total events emitted by each ingestion source.",
	},
	[]string{"source"},
)

// SourceLastEventUnix — per-source gauge, Unix-epoch timestamp of
// the source's most recent observed event.
var SourceLastEventUnix = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_source_last_event_unix",
		Help: "Timestamp of the most recent event per source (Unix seconds).",
	},
	[]string{"source"},
)

// SourceLastInsertUnix — per-source gauge, Unix-epoch wall-clock
// timestamp of the most recent SUCCESSFUL trade row landed for the
// source (i.e. InsertTrade returned with rowsInserted==1, not
// `ON CONFLICT DO NOTHING`).
//
// Pairs with [SourceLastEventUnix] to detect the
// stuck-cursor / replay-loop pattern: when the dispatcher matches
// events (last_event_unix climbs) but ON CONFLICT short-circuits
// every insert (last_insert_unix stops climbing), the gap between
// the two grows. Direct alert template:
//
//	time() - stellarindex_source_last_insert_unix{source="sdex"} > 3600
//
// catches the live r1 2026-05-28 pattern (157 SDEX insert-attempts/
// min, all duplicates, max(ts) 11 h old) within an hour of recurrence.
// Complements the [TradeInsertOutcomeTotal] rate-shape alert with a
// timestamp-shape signal that doesn't require sustained traffic to
// fire.
var SourceLastInsertUnix = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_source_last_insert_unix",
		Help: "Wall-clock timestamp of the most recent successfully-inserted trade row per source (Unix seconds). Stops advancing during a stuck-cursor / duplicate-flood pattern — the gap vs stellarindex_source_last_event_unix is the diagnostic signature.",
	},
	[]string{"source"},
)

// SourceEnabled — per-source 0/1 gauge indicating config-time
// enablement. Used by the "source_stopped" alert to qualify rate-
// zero with "but it was supposed to be running".
var SourceEnabled = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_source_enabled",
		Help: "1 if source is configured enabled; 0 otherwise.",
	},
	[]string{"source"},
)

// SourceMatchedEventsTotal — per-source counter of inputs (events,
// contract calls, entry changes, ops) that a decoder's Matches()
// claimed. The DENOMINATOR of decoder error-rate; the numerator is
// SourceDecodeErrorsTotal. Bumped pre-Decode so a decoder that
// matches then errors still counts — error-rate stays meaningful
// (errors / inputs_attempted) instead of tautological (errors /
// successful_outputs).
//
// Distinct from SourceEventsTotal — that's a per-source count of
// consumer.Events the SINK processes, i.e. decoder OUTPUTS. A
// decoder that buffers (soroswap swap+sync correlation) or
// produces zero outputs for an intermediate matched event would
// register on this counter but not on SourceEventsTotal.
//
// Mirror of dispatcher.Stats.EventsSeen, emitted via the
// pipeline.processor delta loop.
var SourceMatchedEventsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_source_matched_events_total",
		Help: "Inputs each source's decoder Matches() claimed (the denominator of decoder error-rate).",
	},
	[]string{"source"},
)

// SourceDecodeErrorsTotal — per-source counter of decode failures
// (SCVal parse errors, malformed event schemas, etc.).
var SourceDecodeErrorsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_source_decode_errors_total",
		Help: "Events that failed to decode, per source.",
	},
	[]string{"source"},
)

// SourceUnknownSymbolsTotal — per-source counter of asset slots
// dropped from an otherwise-decoded event because the symbol/feed
// id isn't in our canonical asset allow-list (ADR-0010). Distinct
// from SourceDecodeErrorsTotal because the rest of the event still
// decodes cleanly — the parent decode `continue`s past the slot.
//
// F-1234 (codex audit-2026-05-12): upstream oracle coverage can
// expand while we silently omit the new asset; without this counter
// operators have no signal that a feed is being skipped. Reflector,
// Redstone, and Band all increment this on ErrUnknownSymbol /
// ErrUnknownFeedID branches; the alert in
// `deploy/monitoring/rules/external-pollers.yml` fires on a
// sustained per-source non-zero rate.
var SourceUnknownSymbolsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_source_unknown_symbols_total",
		Help: "Asset slots skipped from a decoded event because the symbol/feed id isn't in the canonical allow-list.",
	},
	[]string{"source"},
)

// ExternalPollerPollsTotal — per-source, per-outcome counter of
// PollOnce invocations. Outcome is one of:
//
//   - success — venue returned 200 and the response decoded OK
//   - error   — PollOnce returned a non-nil error (network, HTTP
//     4xx/5xx, decode failure)
//   - skipped — the poller's internal cooldown (after a previous
//     throttle) suppressed the HTTP call
//
// Pre-2026-05-09 there was no signal at all when an external poller
// was sustained-failing — CoinGecko throttling went undetected for
// ~13h on r1 because the only output was a per-minute WARN log. The
// `success` outcome plus PromQL absence-checking is the canonical
// way to alert: `rate(...{outcome="success", source="<name>"}[30m])
// == 0` for sources expected to contribute.
var ExternalPollerPollsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_external_poller_polls_total",
		Help: "External poller invocations, labelled by source and outcome (success | error | skipped).",
	},
	[]string{"source", "outcome"},
)

// ExternalDustDroppedTotal — per-source counter of streamed CEX trades
// dropped at ingest as dust (quote leg below ~$0.001). CEX feeds emit
// sub-microcent fills whose tiny integer amounts make quote/base a
// meaningless round fraction (1/8, 1/10, …); kept, they polluted the
// unweighted OHLC high/low (max/min of quote/base) on the served API
// while contributing ~zero real volume. See the runner's dust guard.
var ExternalDustDroppedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_external_dust_dropped_total",
		Help: "Streamed CEX trades dropped at ingest as sub-$0.001 dust, by source.",
	},
	[]string{"source"},
)

// CEXStreamDisconnectTotal — per-source, per-reason counter of CEX
// WebSocket stream disconnects. Reason is one of:
//
//   - reset           — TCP RST surfaced as "connection reset by peer"
//   - broken_pipe     — write after peer hung up
//   - timeout         — read/handshake timed out
//   - dial            — handshake failed (DNS, TLS, refused, etc.)
//   - server_requested — bitstamp's bts:request_reconnect frame
//   - other           — EOF, framing, or anything else
//
// F-0029 (audit-2026-05-27): r1 logs showed Binance + Bitstamp
// reconnecting every 6-12 min with backoff pinned at 60 s. Pre-fix
// there was no signal for the disconnect cadence — operators read
// raw WARN lines off Loki. Sustained non-zero rate with reason="reset"
// likely means we're missing PING/PONG (handled by coder/websocket
// v1.8.14, but configurable to disable via OnPingReceived returning
// false) or the host TCP keepalive is off (now enabled, F-0029).
var CEXStreamDisconnectTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_cex_stream_disconnect_total",
		Help: "CEX WebSocket stream disconnects by source and reason (reset | broken_pipe | timeout | dial | server_requested | other). F-0029.",
	},
	[]string{"source", "reason"},
)

// ExternalPollerLastSuccessUnix — per-source UNIX-seconds timestamp
// of the most recent successful PollOnce. Zero / unset when the
// poller has never succeeded since process start.
//
// Companion to ExternalPollerPollsTotal: a gauge makes "data is
// stale by N minutes" expressible as `time() - <gauge>` rather than
// requiring multi-window rate math, which is much easier to alert on.
var ExternalPollerLastSuccessUnix = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_external_poller_last_success_unix",
		Help: "UNIX seconds of the most recent successful PollOnce, per source. Zero = never succeeded since startup.",
	},
	[]string{"source"},
)

// ExternalFXLastQuoteUnix — per-source UNIX-seconds timestamp of the
// most recent successful fx_quotes WRITE from the active fiat-FX feed
// (`massive`, the internal/sources/forex worker). Stamped only after
// InsertFXQuoteBatch commits a NON-EMPTY batch — a failed write or an
// empty snapshot (upstream returned no usable rates) leaves the prior
// stamp untouched, so a stuck-but-erroring worker cannot keep the
// gauge green.
//
// Why a SEPARATE gauge from ExternalPollerLastSuccessUnix: the FX feed
// does NOT run under the external.Connector poller framework — it is
// the forex worker in the API binary writing the fx_quotes hypertable,
// which the X2.5 triangulation forex-snap (FXQuoteAtOrBefore) reads
// with a 7-day lookback for every fiat-quoted pair (XLM/EUR, …). A dry
// feed is invisible to the poller-staleness alert (massive emits no
// external_poller series) and to the fx_snap read (a stale-but-present
// row still prices) until the 7-day lookback finally expires and fiat
// pairs silently break. This gauge makes "the FX feed VWAP depends on
// has gone dry" expressible as `time() - <gauge>` and alertable long
// before that 7-day cliff (see stellarindex_external_fx_feed_stale).
//
// Reset-proof across restarts: the worker's startup refresh re-stamps
// it within seconds of a healthy boot (mirrors the gap-detector
// last_success gauge, 2026-07-06). A source that has never once
// written since process start emits no series here — that "never came
// up" case is covered by the paired absent()-based alert.
var ExternalFXLastQuoteUnix = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_external_fx_last_quote_unix",
		Help: "UNIX seconds of the most recent successful fx_quotes write per FX source (currently `massive`). Reset-proof liveness for the active fiat-FX feed the triangulation forex-snap depends on; only advances on a committed non-empty batch.",
	},
	[]string{"source"},
)

// SourceOrphanEventsTotal — per-source counter of events that
// arrived but never correlated into a complete observation.
// Soroswap emits one per aged-out half of a swap/sync pair;
// Phoenix emits one per aged-out incomplete 8-field set.
//
// Distinct from SourceDecodeErrorsTotal because an orphan event
// was well-formed on its own — the surrounding context is what's
// missing. A sustained rate usually means the RPC is dropping
// events or the contract shape shifted.
var SourceOrphanEventsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_source_orphan_events_total",
		Help: "Events that arrived without their required correlation partner, per source.",
	},
	[]string{"source"},
)

// DiscoveryDroppedHitsTotal — count of SEP-41 discovery hits that
// were dropped because the async sink buffer was full. Discovery is
// intentionally best-effort, but operators still need a live signal
// when the buffer starts shedding records under write pressure.
var DiscoveryDroppedHitsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_discovery_dropped_hits_total",
		Help: "Discovery hits dropped because the async discovery sink buffer was full.",
	},
)

// DiscoverySkippedHitsTotal — count of SEP-41 discovery hits whose
// (contract_id, event_type) had already been enqueued in this
// process and were therefore deduplicated before reaching the
// async sink buffer. A high ratio of Skipped to (Skipped + Recorded)
// is expected and healthy — most events for already-discovered
// contracts are noise. Tracked for capacity-planning visibility, not
// alerting.
var DiscoverySkippedHitsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_discovery_skipped_hits_total",
		Help: "Discovery hits skipped because (contract_id, event_type) was already enqueued in this process.",
	},
)

// Sep1CacheOpsTotal — per-outcome counter for SEP-1 cache
// operations. Label `result` is one of:
//
//	hit         — served from cache.
//	miss        — fetched upstream + cached.
//	upstream_error — upstream fetch failed; not cached (see ADR).
//
// A rising `upstream_error` rate usually means an issuer's
// stellar.toml is down; a very low hit rate means the TTL is too
// short or the caller distribution is too dispersed for caching
// to help.
var Sep1CacheOpsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_sep1_cache_ops_total",
		Help: "SEP-1 resolver cache operations by outcome.",
	},
	[]string{"result"},
)

// RateLimitFailOpenTotal — counter of requests that skipped the
// rate-limit check because of a backing-store (Redis) error. The
// middleware fails-open on error so a Redis outage doesn't take
// the whole API down, but operators need a quantitative signal of
// how often it's happening. A spike here usually correlates with
// the redis readyz probe turning red.
var RateLimitFailOpenTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_ratelimit_fail_open_total",
		Help: "Requests that bypassed rate-limiting because Redis errored.",
	},
)

// SourceInsertErrorsTotal — per-source counter of persistence
// failures (DB connection lost, constraint violation, etc.).
// Separate from decode errors because operators respond differently:
// decode errors mean the source schema drifted; insert errors mean
// the storage layer is struggling. kind="trade"|"oracle"|"panic"|
// "unhandled" lets dashboards split trade vs oracle-update writes,
// flag recovered sink panics distinctly from storage-layer rejects,
// and surface half-wired sources whose event type the sink's
// type-switch doesn't recognise.
var SourceInsertErrorsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_source_insert_errors_total",
		Help: "Events that failed to persist to the store, per source + kind (trade/oracle/panic).",
	},
	[]string{"source", "kind"},
)

// CursorLastLedger — per-source gauge, the last-committed cursor
// value in the ingestion_cursors table. Used to detect stuck
// cursors (increase == 0 over time).
var CursorLastLedger = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_cursor_last_ledger",
		Help: "Last ledger committed to the per-source cursor.",
	},
	[]string{"source"},
)

// DivergenceRefreshTotal — per-outcome counter for the orchestrator's
// divergence cache-refresh loop. Labels:
//
//   - `ok`            — refresh succeeded; div:<asset> cache entry
//     written.
//   - `no_vwap`       — VWAP cache miss for this pair (frozen, empty
//     window, transient cache error). Skip.
//   - `parse_error`   — cached VWAP couldn't be parsed as float.
//     Indicates a writer regression.
//   - `refresh_error` — RefreshPair returned a network/marshal/cache
//     error. The previous entry's TTL keeps
//     counting down; flag stays at last-known good.
//
// Operators alert on a sustained `refresh_error` rate (CoinGecko
// down, Chainlink RPC unreachable) — that means
// `flags.divergence_warning` is going stale across the API surface.
// `no_vwap` is benign during cold-start and after freezes; not
// alert-worthy on its own.
var DivergenceRefreshTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_divergence_refresh_total",
		Help: "Aggregator divergence-cache refresh outcomes per Tick (ok|no_vwap|parse_error|refresh_error).",
	},
	[]string{"outcome"},
)

// DivergenceRefreshDurationSeconds — latency histogram for the
// per-pair divergence refresh call. `RefreshPair` fans out to
// every configured external reference (CoinGecko, Chainlink, …)
// for the pair, so the natural failure mode is "one vendor's API
// goes slow and the whole refresh tick stretches" — currently
// invisible without this metric.
//
// Labelled by outcome (matches the counter labels) so operators
// chart `ok` p95/p99 separately from `refresh_error` (often the
// fast-fail path) and `no_vwap` (cache miss, no work done).
//
// Buckets span 10 ms → 30 s — covers a healthy local cache-only
// refresh (≤ 50 ms when every reference is cached), a single
// slow vendor (~1-5 s on CG / Chainlink), and the worst-case
// per-reference timeout (`per_reference_timeout_seconds`,
// default 5 s) compounded across multiple references.
var DivergenceRefreshDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_divergence_refresh_duration_seconds",
		Help:    "Per-pair divergence-refresh latency, labelled by outcome (ok|no_vwap|parse_error|refresh_error).",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
)

// UsageRollupSweepsTotal — per-outcome counter for the API binary's
// usage-rollup worker (internal/usage.Rollup), which folds the Redis
// per-endpoint request counters into the `usage_daily` Timescale
// hypertable every 5 minutes. Labels:
//
//   - `ok`         — sweep completed (including the no-rows case).
//   - `scan_error` — the Redis SCAN/HGETALL pass failed. Counters
//     keep accumulating in Redis; nothing is lost yet
//     (35-day TTL), but /v1/account/usage endpoint rows
//     stop advancing.
//   - `sink_error` — the Timescale upsert failed (Postgres
//     unreachable / migration missing). Same
//     consequence as scan_error.
//
// A sustained non-`ok` rate means the dashboard's per-endpoint
// usage analytics are going stale — informational severity (the
// customer-facing pricing surface is unaffected).
var UsageRollupSweepsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_usage_rollup_sweeps_total",
		Help: "Usage-rollup worker sweep outcomes (ok|scan_error|sink_error).",
	},
	[]string{"outcome"},
)

// UsageRollupSweepDurationSeconds — latency histogram for one
// usage-rollup sweep (Redis SCAN + HGETALLs + one batched Timescale
// upsert), labelled by outcome (matches the counter labels) so
// operators chart `ok` p95/p99 separately from the fail-fast error
// paths — "sweep slow" (Redis key population growing, Postgres lock
// contention) is a different signal from "sweep failing".
//
// Buckets span 5 ms → 30 s: a healthy sweep with a handful of
// active subjects is ≤ 50 ms; hundreds of subjects × two days of
// hashes plus a slow upsert can reach seconds.
var UsageRollupSweepDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_usage_rollup_sweep_duration_seconds",
		Help:    "Usage-rollup sweep latency, labelled by outcome (ok|scan_error|sink_error).",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
)

// ProtocolEventsRollupSweepsTotal — per-sweep outcome counter for the
// aggregator's protocol-events rollup worker
// (internal/aggregate/protoeventsrollup, #43), which folds the
// trailing-24h per-source event census into the protocol_events_24h
// table so /v1/protocols' events_24h column reads a keyed-on-PK lookup
// instead of a multi-table UNION count per request. Labels:
//
//   - `ok`            — sweep completed; rollup rows upserted + pruned.
//   - `refresh_error` — the census/upsert transaction failed (Postgres
//     unreachable, migration 0086 missing). The rollup keeps its
//     previous rows; /v1/protocols events_24h goes stale, not blank.
//
// A sustained `refresh_error` rate means /v1/protocols' activity
// counters stop advancing — informational severity (the customer-facing
// pricing surface is unaffected).
var ProtocolEventsRollupSweepsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_protocol_events_rollup_sweeps_total",
		Help: "Protocol-events rollup worker sweep outcomes (ok|refresh_error).",
	},
	[]string{"outcome"},
)

// ProtocolEventsRollupSweepDurationSeconds — latency histogram for one
// protocol-events rollup sweep (the trailing-24h UNION ALL census over
// ~17 hypertables + one upsert + one prune), labelled by outcome so
// operators chart `ok` p95/p99 separately from the fail-fast error path.
//
// Buckets span 10 ms → 30 s: the census is the multi-second leg the
// #43 rollup moved off the request path, so watching its p95 here is
// how an operator learns the served-tier census is getting heavier.
var ProtocolEventsRollupSweepDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_protocol_events_rollup_sweep_duration_seconds",
		Help:    "Protocol-events rollup sweep latency, labelled by outcome (ok|refresh_error).",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
)

// AssetVolumeRollupSweepsTotal — per-sweep outcome counter for the
// aggregator's asset-volume rollup worker
// (internal/aggregate/assetvolrollup, #43), which folds the trailing-24h
// per-asset USD-volume SUM over prices_1m (single-sided: base OR quote)
// into the asset_volume_24h table so the /v1/assets listing reads a
// keyed-on-PK lookup instead of the ~256k-row per-request scan the
// 2026-07-06 latency incident measured (~4.8s cold). Labels:
//
//   - `ok`            — sweep completed; rollup rows upserted + pruned.
//   - `refresh_error` — the sum/upsert transaction failed (Postgres
//     unreachable, migration 0087 missing). The rollup keeps its
//     previous rows; the listing's volume_24h_usd goes stale, not blank.
//
// A sustained `refresh_error` rate means /v1/assets 24h volumes stop
// advancing — informational severity (the customer-facing pricing
// surface is unaffected).
var AssetVolumeRollupSweepsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_asset_volume_rollup_sweeps_total",
		Help: "Asset-volume rollup worker sweep outcomes (ok|refresh_error).",
	},
	[]string{"outcome"},
)

// AssetVolumeRollupSweepDurationSeconds — latency histogram for one
// asset-volume rollup sweep (the trailing-24h base-OR-quote SUM over
// prices_1m + one upsert + one prune), labelled by outcome so operators
// chart `ok` p95/p99 separately from the fail-fast error path.
//
// Buckets span 50 ms → 60 s: this is the heaviest of the two #43
// rollups (an all-asset prices_1m scan), so watching its p95 here is
// how an operator learns the served-tier volume scan is getting heavier
// — long before it would have shown up as a slow /v1/assets endpoint.
var AssetVolumeRollupSweepDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_asset_volume_rollup_sweep_duration_seconds",
		Help:    "Asset-volume rollup sweep latency, labelled by outcome (ok|refresh_error).",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
	},
	[]string{"outcome"},
)

// PriceAlertEvalTotal — per-sweep outcome counter for the aggregator's
// price-alert evaluator (internal/pricealerts, BACKLOG #60), which
// checks every enabled price_alerts row against the latest closed 1m
// VWAP each tick and enqueues account-scoped `price.alert` webhook
// deliveries when a threshold is crossed. Labels:
//
//   - `ok`            — sweep completed cleanly (including the no-rows
//     and nothing-fired cases).
//   - `list_error`    — the ListEnabledPriceAlerts read failed; the
//     whole sweep was skipped and retried next tick.
//   - `partial_error` — the sweep ran but at least one alert hit a
//     price-read, parse, or enqueue error. Other alerts in the same
//     sweep were still evaluated.
//
// A sustained `list_error` rate means NO alerts are being evaluated —
// customers stop getting notified. `partial_error` is narrower (a
// subset of alerts affected). Alerting: divergence-refresh-shaped
// `list_error` > `ok` guard in the price-alerts rule group.
var PriceAlertEvalTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_price_alert_eval_total",
		Help: "Price-alert evaluator sweep outcomes (ok|list_error|partial_error).",
	},
	[]string{"outcome"},
)

// PriceAlertEvalDurationSeconds — latency histogram for one price-alert
// evaluation sweep (one enabled-alerts read + per-alert VWAP lookups +
// per-fire webhook enqueues), labelled by outcome (matches the counter
// labels) so operators chart `ok` p95/p99 separately from the
// fail-fast `list_error` path.
//
// Buckets span 5 ms → 30 s: a healthy sweep over a handful of alerts is
// ≤ 50 ms; hundreds of alerts each doing a VWAP point-read plus a fan of
// enqueues can reach seconds.
var PriceAlertEvalDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_price_alert_eval_duration_seconds",
		Help:    "Price-alert evaluation sweep latency, labelled by outcome (ok|list_error|partial_error).",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
)

// SignupReaperRunsTotal — per-sweep outcome counter for the API
// binary's speculative-account reaper (internal/signupreaper, F-1255),
// which deletes orphan `accounts` rows left behind when two concurrent
// /v1/auth/callback provisions raced for the same just-verified email:
// the loser's account is marked Suspended with a `signup-race:` reason
// and never gets a user attached. Labels:
//
//   - `ok`    — the reap DELETE ran (deleting 0-N rows; a no-op sweep
//     with nothing to reap is still `ok`).
//   - `error` — the DELETE failed (Postgres unreachable / query error);
//     retried next tick, orphans stay put until it recovers.
//
// A sustained `error` rate means orphans accumulate unbounded — a slow
// leak, not an outage. Alert: divergence-shaped `error` > `ok` guard in
// the signup-reaper rule group.
var SignupReaperRunsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_signup_reaper_runs_total",
		Help: "Speculative-account reaper sweep outcomes (ok|error).",
	},
	[]string{"outcome"},
)

// SignupReaperRunDurationSeconds — latency histogram for one reaper
// sweep (a single bounded DELETE), labelled by outcome (matches the
// counter). Buckets 5 ms → 30 s: the DELETE touches a tiny, indexed
// set (suspended signup-race orphans) so a healthy sweep is a few ms;
// the wide tail catches a degraded / lock-contended Postgres.
var SignupReaperRunDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_signup_reaper_run_duration_seconds",
		Help:    "Speculative-account reaper sweep latency, labelled by outcome (ok|error).",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
)

// SignupReaperRowsDeletedTotal — cumulative count of orphan accounts
// the reaper has deleted. Unlabelled: a monotonically-climbing counter
// operators chart as a rate to see the signup-race orphan production
// rate (steady non-zero = a race is firing regularly; investigate the
// /v1/auth/callback provisioning path per F-1255).
var SignupReaperRowsDeletedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_signup_reaper_rows_deleted_total",
		Help: "Cumulative speculative (signup-race) orphan accounts deleted by the reaper.",
	},
)

// MEVDetectRunsTotal — per-run outcome counter for the aggregator's
// MEV detection worker (internal/aggregate/mev). Labels:
//
//   - `ok`          — scan + detection completed (any inserts counted
//     separately via MEVEventsInsertedTotal).
//   - `scan_error`  — the trades scan failed (Postgres unreachable /
//     slow). The run is skipped; retried next tick.
//   - `write_error` — an mev_events insert failed mid-run.
//
// A sustained non-`ok` rate means the /v1/mev feed is going stale.
// Not alert-worthy on its own (analytics, not an SLO).
var MEVDetectRunsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_mev_detect_runs_total",
		Help: "MEV detection run outcomes (ok|scan_error|write_error).",
	},
	[]string{"outcome"},
)

// MEVEventsInsertedTotal — count of NEW (non-duplicate) MEV events
// written across all runs. The detector re-scans overlapping windows
// and dedups on write, so this counts genuine first-detections, not
// re-observations.
var MEVEventsInsertedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_mev_events_inserted_total",
		Help: "New MEV events persisted (post-dedup) across detection runs.",
	},
)

// MEVDetectDurationSeconds — per-run latency, labelled by outcome.
// The run is a bounded ts-window trades scan + in-memory grouping +
// per-candidate inserts; healthy runs are sub-second, a slow Postgres
// scan stretches the `ok`/`scan_error` tail.
var MEVDetectDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_mev_detect_duration_seconds",
		Help:    "MEV detection run latency, labelled by outcome (ok|scan_error|write_error).",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
)

// TradeInsertsTotal — per-source counter, broken out by whether the
// trade's `usd_volume` column was populated at insert time.
//
// Operators flipping on `[trades].usd_pegged_classic_assets` (the
// L2.2 phase 1 surface — see `internal/storage/timescale.Store.WouldPopulateUSDVolume`)
// use this to verify their allow-list actually covers the trades
// the indexer is seeing. A configured deployment with steady-state
// `usd_volume_populated="no"` on a USDC-quoting venue means the
// operator's classic asset_key doesn't match what the decoder
// stamps — typically an issuer mismatch or a missing entry.
//
// Cardinality: one source × two outcomes per registered source
// (low-tens of series at maturity).
var TradeInsertsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_trade_inserts_total",
		Help: "Trade-insert attempts, labelled by source and whether usd_volume was populated (yes|no). Counts attempts not unique-row inserts (ON CONFLICT DO NOTHING dedupe is invisible to this counter).",
	},
	[]string{"source", "usd_volume_populated"},
)

// TradeInsertOutcomeTotal — per-source counter of trade-insert
// outcomes. Distinguishes "row actually persisted" (`new`) from
// "ON CONFLICT DO NOTHING dedupe path" (`duplicate`).
//
// TradeInsertsTotal counts attempts and is silent about dedupe; on
// a healthy live indexer the two counters track 1:1, but a stuck
// cursor or replay loop (live evidence on r1, 2026-05-28: 157
// SDEX insert-attempts/min while the trades hypertable's max(ts)
// is 11 h old) produces a fast-growing `duplicate` rate with zero
// `new`. Pairing the two lets operators alert on
// `rate(new[5m]) == 0 AND rate(duplicate[5m]) > 0` — the exact
// signature of a duplicate-flood. Cardinality: one source × two
// outcomes per registered source (low-tens of series at maturity).
var TradeInsertOutcomeTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_trade_insert_outcome_total",
		Help: "Trade-insert outcomes per source. outcome=new when a fresh row landed; outcome=duplicate when ON CONFLICT DO NOTHING short-circuited (indicates cursor replay or stuck-tip).",
	},
	[]string{"source", "outcome"},
)

// DexTradeUnitRatioTotal — the unit-ratio trade sentinel (2026-07-07
// Phoenix incident). A decoder field-mapping bug swapped/collapsed
// base_amount and quote_amount for every Phoenix trade — 237k rows
// landed with base_amount == quote_amount (an exact 1:1 price) and
// went unnoticed for MONTHS because completeness checks (ADR-0033)
// verify presence — a row exists per event — not economic
// plausibility — whether the row's numbers make sense. This counter
// is the cheap plausibility check: it bumps per (landed, on-chain)
// trade whose base_amount exactly equals its quote_amount, both
// nonzero.
//
// Emitted from the STORAGE layer (`internal/storage/timescale`
// InsertTrade + BatchInsertTrades), not the pipeline sink. See the
// godoc on isDexUnitRatioTrade for why that's the one seam that sees
// every landed trade exactly once regardless of which upstream path
// (dispatcher live batch, projector single-event, or a
// stellarindex-ops ch-rebuild/backfill re-derive) produced it — same
// reasoning as the neighbouring TradeInsertOutcomeTotal.
//
// Off-chain (CEX/FX) trades are EXCLUDED: those sources normalise
// amounts onto a fixed integer scale (10^8 for CEX/reference-
// aggregator sources, 10^6 for the FX pollers — CLAUDE.md
// "External-source amount scaling is NOT uniform") where a base==quote
// reading doesn't carry the same "the decoder is broken" signal an
// on-chain 1:1 does, and an occasional genuine equal-value
// cross-asset fill is unremarkable. Excluded via ledger==0, the
// off-chain marker every external connector deliberately stamps
// (migration 0004_relax_trades_ledger_for_offchain).
//
// Label set (`source`) is bounded (one series per registered DEX
// connector) but NOT pre-seeded — same rationale as
// DEXTradeNonstandardDecimalsTotal: healthy steady-state is zero
// trades matching the predicate, the alert is a plain
// increase()-over-threshold (not an absent()/==0 check), and a
// series should exist only once a source actually produces a
// unit-ratio trade.
var DexTradeUnitRatioTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_dex_trade_unit_ratio_total",
		Help: "On-chain DEX trades landed with base_amount == quote_amount (both nonzero) — the signature of the 2026-07-07 Phoenix decoder field-mapping bug that silently collapsed 237k trades to a 1:1 price for months. Labels: source. CEX/FX (ledger==0) excluded — they scale amounts differently. A sustained stream from one source (not an occasional hit) indicates a decoder bug; see runbook dex-trade-unit-ratio.md.",
	},
	[]string{"source"},
)

// TradeInsertRetriesTotal — counter of the trade sink's blocking
// retry loop (2026-07-06 Postgres-outage fix), labelled by `outcome`:
//
//   - "retry"     — one backoff retry attempt after an infrastructure-
//     classified insert failure (connection refused/reset, PG
//     restarting, too-many-connections). Each attempt increments; a
//     sustained nonzero rate means the served-tier write path is
//     blocked and the on-chain ledger cursor is NOT advancing
//     (backpressure holding, no data lost). The
//     `trade_insert_backpressure` alert fires on this.
//   - "recovered" — a previously-blocked insert (batch or row) finally
//     landed after ≥1 retry. Pairs with "retry": a healthy recovery
//     shows a burst of "retry" then one "recovered".
//   - "abandoned" — a blocked insert gave up because the context was
//     cancelled mid-retry (shutdown). On-chain rows are re-derivable
//     from the CH lake (ADR-0034); the exact ledger range is logged at
//     ERROR alongside this bump.
//
// Distinct from genuine drops (data-fault skips + external-buffer
// overflow), which are counted on
// [SourceInsertErrorsTotal]{kind="trade"|"dropped"} — see ADR-0041.
var TradeInsertRetriesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_trade_insert_retries_total",
		Help: "Trade-sink blocking-retry events by outcome (retry|recovered|abandoned) — the 2026-07-06 Postgres-outage backpressure path.",
	},
	[]string{"outcome"},
)

// TradeInsertBufferDepth — gauge of the number of external
// (CEX/FX) trades currently held in the bounded in-memory retry
// buffer, waiting to land after an infrastructure-classified insert
// failure (ADR-0041 / 2026-07-06 outage fix).
//
// External trades have no ledger cursor and are vendor-refillable, so
// they are NOT allowed to block the pipeline: on an infra fault they
// are buffered here and retried by a background goroutine. When the
// buffer's bound is exceeded the OLDEST entry is dropped (counted on
// [SourceInsertErrorsTotal]{kind="dropped"}). A depth that climbs and
// stays high means Postgres has been unreachable long enough that
// external price freshness is degrading. On-chain trades do NOT use
// this buffer — they block-and-retry instead (cursor gating), so this
// gauge is external-only.
var TradeInsertBufferDepth = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_trade_insert_buffer_depth",
		Help: "External (CEX/FX) trades currently held in the bounded retry buffer pending durable insert (ADR-0041).",
	},
)

// StreamPublishTotal — per-stream counter of envelopes the API
// binary's [streampublish.Publisher] fanned out to a streaming Hub.
// Increments only on a NEW closed bucket (the publisher
// short-circuits when ObservedAt hasn't advanced).
//
// Operators read this alongside per-pair subscriber counts to
// validate the closed-bucket fanout path: a steady stream of
// publishes with zero subscribers means clients aren't connecting;
// zero publishes with active subscribers means the upstream
// reader isn't seeing new buckets.
//
// Cardinality: one series per stream surface — low single-digit at
// maturity.
var StreamPublishTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_stream_publish_total",
		Help: "Closed-bucket envelopes published to the streaming Hub, labelled by stream surface (e.g. price_stream).",
	},
	[]string{"stream"},
)

// ─── Pricing / oracle metrics ────────────────────────────────────

// PriceStalenessSeconds — per-asset gauge showing how old our
// latest aggregated-price observation is. Alert fires when >120s.
//
// CARDINALITY WARNING: Stellar has tens of thousands of classic
// assets. Writers MUST restrict emission to an allow-list (top-N
// by volume, per-asset-quality tier, or similar) — never emit for
// every asset seen. Prometheus recommends <10^4 series per metric;
// unrestricted per-asset emission blows past that on a busy chain.
// The aggregator owns this allow-list; see
// docs/architecture/aggregation-plan.md.
var PriceStalenessSeconds = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_price_staleness_seconds",
		Help: "Age of the most recent aggregated price per asset (seconds). Writers MUST restrict to a top-N allow-list.",
	},
	[]string{"asset"},
)

// OracleLastUpdateUnix — per-(source, asset) gauge with the Unix
// timestamp of the most recent oracle observation for that pair.
//
// Cardinality: Reflector/Band/Redstone each track O(30) assets, so
// the shipped sources together stay well inside Prometheus's
// comfort zone. If we ever wire a "passthrough every asset"
// oracle, revisit — this would need the same allow-list discipline
// as PriceStalenessSeconds.
var OracleLastUpdateUnix = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_oracle_last_update_unix",
		Help: "Timestamp of the most recent oracle observation, per source and asset.",
	},
	[]string{"source", "asset"},
)

// OracleResolutionSeconds — per-source gauge of the oracle's
// declared resolution interval. Used by the oracle-stale alert
// to qualify "no update in > 10× resolution".
var OracleResolutionSeconds = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_oracle_resolution_seconds",
		Help: "Declared resolution interval of each oracle source (seconds).",
	},
	[]string{"source"},
)

// ─── Aggregator orchestrator metrics ─────────────────────────────

// AggregatorTicksTotal — count of orchestrator ticks completed,
// labelled by outcome ("ok" when the tick ran without surfacing an
// error, "error" when at least one (pair, window) refresh failed).
// Per-pair errors are still recorded as soft warnings; this counter
// is the tick-level rollup.
var AggregatorTicksTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_ticks_total",
		Help: "Aggregator orchestrator tick count, labelled by outcome (ok|error).",
	},
	[]string{"outcome"},
)

// AggregatorVWAPWritesTotal — count of (pair, window) Redis writes
// performed by the orchestrator. Unlabelled to keep cardinality
// bounded — the per-pair lens lives in the Redis key namespace.
var AggregatorVWAPWritesTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_vwap_writes_total",
		Help: "Cumulative VWAP cache writes performed by the aggregator.",
	},
)

// AggregatorEmptyWindowsTotal — count of (pair, window) refreshes
// that produced zero VWAP-eligible trades after class filtering /
// stablecoin expansion / outlier filtering. Unlabelled for the same
// reason as VWAPWritesTotal.
var AggregatorEmptyWindowsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_empty_windows_total",
		Help: "Aggregator (pair, window) refreshes that produced zero eligible trades.",
	},
)

// AggregatorWindowTruncatedTotal — count of (pair, window) fetches
// whose trade count hit MaxTradesPerWindow, i.e. the window held more
// trades than the per-query cap and the VWAP was computed over only the
// newest `cap` trades. A non-zero rate means a busy pair/window is
// being aggregated over a partial slice (F-1319) — chart
// `rate(...)` against AggregatorVWAPWritesTotal to see how often it
// fires; sustained firing means the cap (or window) needs raising or a
// SQL-side aggregate. Unlabelled to keep cardinality bounded, matching
// the sibling aggregator counters.
var AggregatorWindowTruncatedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_window_truncated_total",
		Help: "Aggregator (pair, window) fetches that hit MaxTradesPerWindow — VWAP computed over a partial (newest-N) trade slice.",
	},
)

// AggregatorVWAPCacheWriteErrorsTotal — count of failed Redis SET
// attempts during the VWAP cache write step. The orchestrator
// returns an error and the next tick retries; from the customer
// surface, sustained failures here mean /v1/price returns 404 on
// every cached pair (rewritten/triangulated/stablecoin-proxy
// paths) while the Timescale-direct paths still serve.
//
// Surfaces the May-10 incident class
// (internal/incidents/data/2026-05-10-redis-writes-blocked-disk-full.md)
// where Redis BGSAVE failed for ~9h and the only customer signal
// was 404s on rewritten pairs while flags.stale stayed off
// (because the aggregator was running, just unable to publish).
// Operators alert on rate(_total[5m]) > 0 for ≥ 2 m as the
// upstream-of-stale signal.
var AggregatorVWAPCacheWriteErrorsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_vwap_cache_write_errors_total",
		Help: "Aggregator VWAP cache writes that returned a Redis error. Cumulative since process start.",
	},
)

// AggregatorStreamPublishTotal — count of closed-bucket events the
// orchestrator handed to the configured StreamPublisher (Redis
// pub/sub fan-out for /v1/price/stream subscribers per L3.9).
// Labelled by outcome:
//
//   - "ok" — Publish returned nil; subscribers (if any) receive the event.
//   - "error" — Publish returned a non-nil error; the next tick retries
//     and the VWAP cache write itself is unaffected.
//
// Unset when no StreamPublisher is wired (no fan-out path).
var AggregatorStreamPublishTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_stream_publish_total",
		Help: "Closed-bucket stream publishes attempted by the aggregator, labelled by outcome.",
	},
	[]string{"outcome"},
)

// APIStreamSubscribeTotal — count of closed-bucket Redis pub/sub
// messages the API binary's subscriber processed, labelled by
// outcome:
//
//   - "ok" — message decoded and republished on the local Hub for
//     /v1/price/stream subscribers.
//   - "decode_error" — JSON unmarshal failed; message dropped, next
//     message processed normally. Indicates wire-format drift between
//     aggregator's Publisher and this Subscriber.
//   - "malformed" — JSON decoded but Asset or Quote was empty;
//     message dropped without Hub publish (no valid topic to route to).
//
// Unset when no Subscriber is wired (the API binary's
// /v1/price/stream returns 503 instead of fanning out).
var APIStreamSubscribeTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_api_stream_subscribe_total",
		Help: "Closed-bucket Redis pub/sub messages processed by the API subscriber, labelled by outcome.",
	},
	[]string{"outcome"},
)

// CustomerWebhookDeliveryAttemptsTotal — outcome of every
// customer-webhook delivery attempt, labelled by:
//
//   - delivered      — 2xx response, MarkDelivered succeeded
//   - server_error   — 5xx response, scheduled for retry
//   - client_error   — 4xx response, terminally failed
//   - exhausted      — retry budget hit, terminally failed
//   - network_error  — TCP/TLS/timeout error, scheduled for retry
//   - webhook_missing — GetWebhook returned ErrNotFound mid-flight
//   - disabled       — webhook.Enabled=false, silently terminated
//   - build_error    — http.NewRequestWithContext failed (malformed URL)
//   - list_error     — ListPendingDeliveries failed (db transport)
//   - mark_error     — Mark{Delivered,AttemptFailed} failed
//
// Operators alert on:
//
//	rate(...{outcome="server_error"}[5m]) > 0.1
//	  — one customer's URL is sustained-failing, raise a ticket
//	rate(...{outcome="exhausted"}[1h]) > 0
//	  — a delivery permanently failed, drag the deliveries table
//
// F-1270 (audit-2026-05-12).
var CustomerWebhookDeliveryAttemptsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_customer_webhook_delivery_attempts_total",
		Help: "Customer-webhook delivery attempts, labelled by outcome.",
	},
	[]string{"outcome"},
)

// CustomerWebhookDeliveryDurationSeconds — latency histogram for
// the outbound HTTP POST inside the customer-webhook delivery
// worker (parallel to the inbound Stripe webhook's free
// `http_request_duration_seconds` from the API HTTP middleware;
// the OUTBOUND worker is a goroutine, not an HTTP handler, so the
// standard histogram doesn't cover it).
//
// Labelled by outcome (same enum as the attempts counter) so
// operators can chart p95/p99 latency separately for `delivered`
// (the happy path) vs `server_error`/`client_error` (which often
// run hot or slow when a customer's endpoint is misbehaving).
//
// Buckets span 10 ms → 60 s — covers fast LANs (≤ 20 ms),
// typical TLS-terminated webhook endpoints (~100-500 ms), and
// the worst-case 60 s context timeout the delivery worker
// enforces before treating a request as a network_error.
var CustomerWebhookDeliveryDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_customer_webhook_delivery_duration_seconds",
		Help:    "Customer-webhook outbound HTTP POST latency, labelled by outcome (delivered|server_error|client_error|network_error|build_error). Body-drain time is included.",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
	},
	[]string{"outcome"},
)

// APICORSDecisionsTotal — per-request CORS outcome counter.
//
// Outcomes:
//   - "no_origin"        — request had no Origin header (server-to-server, curl).
//     Middleware passes through; no CORS headers emitted.
//   - "allowed_origin"   — Origin matched a configured allow-list entry.
//     Allow-Origin echoed back.
//   - "allowed_wildcard" — wildcard policy ("*") was configured and matched.
//     Allow-Origin: * emitted.
//   - "denied"           — request had an Origin header that did NOT match
//     the allow-list; no Allow-Origin emitted (browser
//     will block the response).
//
// Why a counter, not a startup-only warning: the startup warning in
// warnOpenCORS (cmd/stellarindex-api/main.go) fires once at boot and
// is forgotten. Per-request visibility lets operators dashboard
// actual cross-origin traffic patterns and alert when a wildcard
// policy starts handling real cross-origin requests in production
// — the silent failure mode of `STELLARINDEX_ALLOWED_ORIGINS=*`
// slipping into prod with credentialed auth_mode. F-1244.
var APICORSDecisionsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_api_cors_decisions_total",
		Help: "Per-request CORS decisions. Outcome ∈ {no_origin, allowed_origin, allowed_wildcard, denied}.",
	},
	[]string{"outcome"},
)

// AggregatorDroppedTradesTotal — count of trades the orchestrator
// removed from the VWAP input set, labelled by reason. "class" =
// removed by the ClassExchange-only filter; "outlier" = removed by
// the σ-threshold filter. Operators alert on a sudden spike in
// "class" (a new venue mis-registered) or "outlier" (a market in
// distress flooding the window with anomalies).
var AggregatorDroppedTradesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_dropped_trades_total",
		Help: "Trades removed from the VWAP input set, labelled by reason (class|outlier).",
	},
	[]string{"reason"},
)

// AggregatorDroppedWindowsTotal — count of (pair, window) refreshes
// where the post-class + post-outlier trade set was non-empty but
// the window was suppressed by a window-level filter. Labelled by
// reason: "min_usd_volume" = window's total USD-equivalent volume
// fell below `aggregate.min_usd_volume`. Distinct from
// AggregatorEmptyWindowsTotal (which means literally zero trades);
// drives the launch-readiness L2.1 caveat audit.
var AggregatorDroppedWindowsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_dropped_windows_total",
		Help: "Windows the orchestrator suppressed at the window-level filter step, labelled by reason (min_usd_volume).",
	},
	[]string{"reason"},
)

// ─── Supply-derivation metrics ────────────────────────────────────

// SupplyCrossCheckDivergenceStroops — gauge of the stroop divergence
// between a classic asset's Algorithm 2 supply and its SAC-wrapped
// Algorithm 3 supply. The alert in deploy/monitoring/rules/supply.yml
// fires when this exceeds 1 stroop.
//
// Labelled by classic_key (CODE:ISSUER) so a per-asset dashboard +
// runbook can identify the offending asset without log dive, AND by
// wrap_class (2026-07-08 decision, BACKLOG #59 — see
// internal/supply.WrapClass) so operators can see which invariant
// produced a given reading:
//
//   - wrap_class="full_wrap": the value is the ORIGINAL ADR-0011
//     equality compare, |classic_total − sac_total|. Only used for a
//     pair the operator has attested is genuinely 100% SAC-
//     represented (`[supply].fully_wrapped_sacs`); none configured
//     as of 2026-07-08.
//   - wrap_class="partial_wrap" (the default): the value is
//     max(0, sac_total − classic_total) — zero in the normal,
//     expected state for a partially-wrapped classic asset (most of
//     its supply lives outside the SAC), positive only when the SAC
//     reports MORE than the classic total could possibly back, which
//     is impossible under correct accounting and is therefore a
//     genuine corruption signal.
//
// The alert threshold (`> 1`) is unchanged and does NOT need to
// filter on wrap_class: the metric itself is already zero in the
// benign partial-wrap case (fixing the 8 standing false positives
// this label was introduced to explain), and still fires on a
// genuine violation for either class.
//
// Cardinality bound by the curated asset set with deployed SAC
// contracts (low dozens at launch, hundreds at maturity) × the
// 2-value wrap_class set.
//
// Emitted by `cmd/stellarindex-aggregator/main.go::buildCrossCheckRefresher`
// once per `[supply].aggregator_refresh_cadence` tick when both the
// classic side and the SAC side of a wrapper are in the watched-sets.
// The CLI `stellarindex-ops supply audit <asset> -cross-check <counterpart>`
// path remains for ad-hoc operator inspection but does not update the
// gauge — only the aggregator's periodic refresher does.
var SupplyCrossCheckDivergenceStroops = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_supply_cross_check_divergence_stroops",
		Help: "Stroop divergence between classic and SAC-wrapped supply for the same asset, labelled by wrap_class (full_wrap: |classic-sac| equality; partial_wrap: max(0, sac-classic) subset bound). Alert when > 1.",
	},
	[]string{"classic_key", "wrap_class"},
)

// SupplyCrossCheckTotal — counter of cross-check evaluations per
// outcome (within | over | missing_snapshot | read_error) and
// wrap_class (full_wrap | partial_wrap — 2026-07-08, BACKLOG #59).
// Drives the alert's rate-of-failure view and gives operators a "is
// the cross-checker even running" check orthogonal to the gauge.
//
// `missing_snapshot` is emitted while either side of the pair has no
// snapshot in `asset_supply_history` yet — the bootstrap state.
// `read_error` covers transient storage failures so a sustained-rate
// regression on this label surfaces a different failure mode than
// genuine divergence.
var SupplyCrossCheckTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_supply_cross_check_total",
		Help: "Cross-check evaluations, labelled by outcome (within|over|missing_snapshot|read_error) and wrap_class (full_wrap|partial_wrap).",
	},
	[]string{"outcome", "wrap_class"},
)

// ─── Supply-divergence cross-check metrics ────────────────────────────
//
// DISTINCT from the SupplyCrossCheck* pair above: that pair is an
// INTERNAL consistency check (a classic asset's Algorithm 2 sum vs its
// SAC-wrapped Algorithm 3 sum — both OUR OWN numbers). The
// SupplyDivergence* set below cross-checks OUR served circulating
// supply against an EXTERNAL authoritative reference (the Stellar
// Network Dashboard for XLM; CoinGecko when a Pro key is configured).
// It catches a genuinely-stale SDF-reserve exclusion list — the drift
// that a manual "is our supply right?" investigation is otherwise the
// only defense against (docs/methodology/xlm-circulating-supply.md).
//
// Emitted by `cmd/stellarindex-aggregator/main.go` (obsSupplyDivergenceEmitter,
// driven by `internal/divergence.SupplyService.Tick`) once per
// `[divergence.supply].refresh_interval` when the check is enabled.

// SupplyDivergenceRatio — gauge of the absolute relative divergence
// |our − reference| / reference between OUR served circulating supply
// and an external reference's, per (asset, reference).
//
// The primary alert target: `stellarindex_supply_divergence_high`
// fires when this exceeds the operator threshold (default 0.01 = 1%,
// well above the ~0.03% XLM Fee-Pool noise floor —
// docs/methodology/xlm-circulating-supply.md). Labelled by `asset`
// (canonical wire form, e.g. "native") and `reference`
// ("stellar-dashboard" / "coingecko"). Cardinality bound by the tiny
// flagship check set × reference set (single digits).
//
// NOT updated on the no_reference / refresh_error outcomes — a frozen
// gauge (last-known value) is the correct behaviour when a reference
// goes dark (the no_reference counter carries that signal), so a dead
// reference never manufactures a divergence reading.
var SupplyDivergenceRatio = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_supply_divergence_ratio",
		Help: "Absolute relative divergence |our − reference| / reference of served circulating supply, per (asset, reference). Alert when > 1% (well above the ~0.03% XLM noise floor).",
	},
	[]string{"asset", "reference"},
)

// SupplyDivergenceTotal — per-outcome counter for the supply
// cross-check, one increment per (asset, tick):
//
//   - `ok`            — served figure agreed with every responding
//     reference within the threshold.
//   - `divergent`     — a responding reference disagreed by more than
//     the threshold. The ratio gauge carries the magnitude.
//   - `no_reference`  — served figure loaded but every reference was
//     unreachable / didn't publish the asset (CoinGecko 429, Dashboard
//     outage). Graceful-degrade — deliberately NOT paged, so a dead
//     reference isn't a false divergence alarm.
//   - `refresh_error` — OUR served snapshot couldn't be read
//     (bootstrap, storage error). Nothing to compare.
//
// The `no_reference` rate is the "checker running blind" signal (the
// CS-088 analogue on the supply path); operators watch it but it does
// not page.
var SupplyDivergenceTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_supply_divergence_total",
		Help: "Supply cross-check evaluations per (asset, tick), labelled by outcome (ok|divergent|no_reference|refresh_error).",
	},
	[]string{"outcome"},
)

// SupplyDivergenceDurationSeconds — latency histogram for one
// (asset, tick) supply cross-check evaluation, including the served
// read + the HTTP fan-out to every reference. Labelled by outcome
// (matches the counter) so operators chart the healthy `ok` path
// separately from the slow-vendor / timeout `no_reference` path.
//
// Buckets span 10 ms → 30 s: a warm served read is single-digit ms;
// a single slow reference (Dashboard / CoinGecko) is ~1-10 s; the
// worst case is the per-reference timeout (default 10s) compounded
// across the reference set.
var SupplyDivergenceDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_supply_divergence_duration_seconds",
		Help:    "Per-(asset, tick) supply cross-check latency, labelled by outcome (ok|divergent|no_reference|refresh_error).",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
)

// ─── verify-archive metrics ───────────────────────────────────────
//
// Emitted by `stellarindex-ops verify-archive` when the operator
// passes -metrics-listen ADDR. One-shot diagnostic command, but the
// run can take hours on full pubnet sweeps — live metrics let
// operators dashboard the bottleneck during the run rather than
// guessing from log tails.
//
// All vectors labelled by chunk_idx (decimal string) so a parallel
// run with -workers 8 produces per-chunk series. Cardinality bound
// by the -workers cap (currently [1,16]).

// AnomalyFreezeEngagedTotal — counter of ActionFreeze decisions
// the aggregator's anomaly checker emitted, labelled by the asset
// class that drove the threshold lookup. Each increment means the
// orchestrator declined to publish a fresh VWAP (kept the prior
// bucket's last-known-good value); the API's /v1/price for the
// affected pair will surface flags.frozen=true on the next read.
//
// Pair-specific freeze details live in the freeze marker JSON
// (deviation_pct, reason) — labelled by class only here so
// cardinality stays bound to the small AssetClass enum.
var AnomalyFreezeEngagedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_freeze_engaged_total",
		Help: "ActionFreeze decisions emitted by the aggregator anomaly checker, labelled by asset class.",
	},
	[]string{"class"},
)

// AnomalyFreezeRecoveredTotal — counter of freeze rows the recovery
// worker closed (Redis marker TTL elapsed → MarkRecovered stamped
// recovered_at on the durable `freeze_events` row). Steady-state
// rate trails AnomalyFreezeEngagedTotal by the freeze TTL plus the
// recovery worker's poll interval.
var AnomalyFreezeRecoveredTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_freeze_recovered_total",
		Help: "Freeze rows closed by the recovery worker after the Redis marker TTL elapsed.",
	},
)

// AnomalyFreezeRecoverySweepsTotal — counter of recovery-worker
// poll cycles, labelled by outcome (ok / partial / error). Sustained
// `error` indicates the lister or Redis transport is broken; sustained
// `partial` means MarkRecovered is failing for one or more rows
// (postgres write path issue).
var AnomalyFreezeRecoverySweepsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_freeze_recovery_sweeps_total",
		Help: "Freeze recovery-worker sweep cycles. Outcome ∈ {ok, partial, error}.",
	},
	[]string{"outcome"},
)

// AnomalyFreezeRecoverySweepDurationSeconds — latency histogram
// for the freeze recovery worker's per-sweep tick. Pairs with the
// counter above. The sweep does ListOpen (Postgres read) plus,
// per open row, a Redis GET and possibly MarkRecovered (Postgres
// write). Fast path is sub-100 ms when there are zero open rows;
// climbs proportionally with the open-row count.
//
// Latency degradation typically means Postgres pressure or Redis
// lag rather than a freeze-policy issue. The 60-second sweep
// cadence means even a multi-second sweep doesn't lose
// correctness — the next tick catches up — but sustained
// slowness is worth investigating before the freeze_events
// table accumulates open rows the operator UI shows as
// permanently firing.
//
// Buckets span 10 ms → 30 s. No alert wired today; the
// existing recovery-sweep error counter covers correctness.
var AnomalyFreezeRecoverySweepDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_anomaly_freeze_recovery_sweep_duration_seconds",
		Help:    "Freeze recovery-worker sweep latency, labelled by outcome (ok|partial|error). Sweep does ListOpen (Postgres) + per-row Redis GET + maybe MarkRecovered (Postgres write).",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
)

// AggregatorTriangulationsTotal — counter of triangulation
// computations per outcome. The aggregator runs one row per
// (chain, window) per tick; steady state is mostly `ok` with
// periodic `missing_leg` entries when a leg's window was empty
// this tick. Sustained `parse_error` or `redis_error` rates >
// baseline indicate upstream regression worth investigating.
var AggregatorTriangulationsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_triangulations_total",
		Help: "Aggregator triangulation outcomes per tick × chain × window. Outcome ∈ {ok, missing_leg, parse_error, redis_error}.",
	},
	[]string{"outcome"},
)

// AggregatorFXSnapFallbackTotal — counter of triangulation legs that
// fell back from the X2.5 forex-snap rule to the cached-VWAP path
// because FXQuoteAtOrBefore returned no row at-or-before the bucket
// end. Steady state should be near-zero once FX ingestion is warm.
// Sustained > 50% of triangulations indicates an FX-source health
// issue (polygon-forex / exchangeratesapi) — see the matching alert
// in deploy/monitoring/rules/aggregator.yml.
//
// Label `leg` is the canonical pair string of the FX leg that fell
// back (e.g. "fiat:USD/fiat:EUR"); cardinality is bounded by the
// operator-configured triangulation chain set.
var AggregatorFXSnapFallbackTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_fx_snap_fallback_total",
		Help: "Triangulations that fell back to cached VWAP for an FX leg because FXQuoteAtOrBefore returned no row at-or-before the bucket end.",
	},
	[]string{"leg"},
)

// AggregatorBaselineRefreshTotal — counter of baseline refresh
// outcomes per pair, per refresh cycle (ADR-0019 Phase 2). One
// increment per pair per cycle; outcome ∈ {ok, not_enough_samples,
// read_error, write_error}. Steady state is mostly `ok`; sustained
// `not_enough_samples` indicates pairs in bootstrap (ADR-0019
// §"Bootstrap policy"); sustained `read_error` / `write_error`
// indicate the storage layer needs investigation.
var AggregatorBaselineRefreshTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_baseline_refresh_total",
		Help: "Baseline refresh outcomes per pair × refresh cycle. Outcome ∈ {ok, not_enough_samples, read_error, write_error}.",
	},
	[]string{"outcome"},
)

// AggregatorSupplyRefreshTotal — counter of supply-snapshot refresh
// outcomes per cycle (ADR-0011 / ADR-0021 / ADR-0022 / ADR-0023).
// One increment per (asset, tick); labels:
//
//   - asset_key: supply.AssetKey form ("XLM", "CODE:ISSUER" for
//     classic credits, the bare contract C-strkey for SEP-41).
//   - outcome ∈ {ok, dormant, no_ledger, no_observation,
//     compute_error, stale_component, missing_freshness,
//     write_error}. `dormant` (F-1320) is a benign accept: a
//     dormant asset whose component anchor is unchanged but current.
//     `stale_component` is a real rejection (the freshness producer
//     lagged); the supply-refresh alert excludes `dormant` and is
//     keyed by asset_key so one stuck asset isn't masked.
//
// Steady-state is mostly `ok` per asset. Sustained `no_observation`
// means the AccountEntry observer hasn't backfilled the watched
// accounts yet (the chain-reader fell through to static config and
// that also missed) — expected briefly post-deploy, alarming
// sustained. Per-asset rates let operators chart bootstrap
// progress per watched asset rather than as one aggregate.
var AggregatorSupplyRefreshTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_supply_refresh_total",
		Help: "Supply-snapshot refresh outcomes per (asset_key, outcome). Outcome ∈ {ok, dormant, no_ledger, no_observation, compute_error, stale_component, missing_freshness, write_error}.",
	},
	[]string{"asset_key", "outcome"},
)

// AggregatorSupplyRefreshDurationSeconds — latency histogram for
// the supply.Refresher.Tick call per supply-refresh cycle. Pairs
// with the per-asset_key counter above; this metric labels by
// outcome only (NOT asset_key) to keep cardinality manageable
// when many assets are watched.
//
// Tick does Postgres reads (ledger lookup + per-component
// freshness queries) plus a Postgres write (snapshot insert).
// Steady-state ~50-200 ms; a p99 climb past 1 s typically means
// the snapshot inserter is contending with another writer or one
// of the per-component freshness readers fell off its index.
//
// Buckets span 10 ms → 30 s. The per-tick log line emitted by
// supply.Refresher.Tick names the asset; correlate from the
// histogram + log timestamp when per-asset latency matters.
var AggregatorSupplyRefreshDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_aggregator_supply_refresh_duration_seconds",
		Help:    "Supply-snapshot refresh tick latency, labelled by outcome. Asset-level granularity available via per-tick log timestamps.",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
)

// SEP41SupplyRollupAdvancesTotal — counter of sep41_supply_rollup
// incremental-advance passes (migration 0085, incident 2026-07-06).
// The rollup is what keeps the SEP-41 Algorithm-3 supply reader cheap:
// each pass folds a contract's newly-settled mint/burn/clawback events
// into a per-contract running checkpoint so the reader never re-sums
// the full per-contract history. One increment per (contract_id, tick);
// labels:
//
//   - contract_id: the watched SEP-41 C-strkey being advanced.
//   - outcome ∈ {ok, noop, error}. `ok` folded new settled rows;
//     `noop` ran cleanly with nothing new to settle (steady state for
//     a dormant token); `error` is a failed advance (Postgres issue).
//
// Sustained `error` for a contract means its checkpoint is frozen and
// the reader is silently back on the slow full-sum fallback for that
// contract — correlate with a p99 climb on
// `stellarindex_aggregator_supply_refresh_duration_seconds`.
var SEP41SupplyRollupAdvancesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_sep41_supply_rollup_advances_total",
		Help: "SEP-41 supply rollup incremental-advance passes per (contract_id, outcome). Outcome ∈ {ok, noop, error}.",
	},
	[]string{"contract_id", "outcome"},
)

// SEP41SupplyRollupAdvanceDurationSeconds — latency histogram for one
// AdvanceSEP41SupplyRollup pass. Pairs with the per-contract counter
// above; labelled by outcome only (NOT contract_id) to keep cardinality
// bounded across deployments watching many contracts.
//
// Steady-state is sub-second (a bounded tail sum on the
// (contract_id, ledger DESC) index). The one exception is a cold
// contract's FIRST fold — that pass sums the whole per-contract history
// once and can take seconds→minutes on a hundreds-of-millions-row
// table; every subsequent pass is incremental. A sustained high p99
// after warm-up means the tail delta stopped being bounded (worker
// starved / checkpoint not advancing).
var SEP41SupplyRollupAdvanceDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_sep41_supply_rollup_advance_duration_seconds",
		Help:    "SEP-41 supply rollup advance-pass latency, labelled by outcome. Steady-state sub-second; the cold first fold is the exception.",
		Buckets: []float64{0.005, 0.025, 0.1, 0.5, 1, 2.5, 5, 15, 60, 300},
	},
	[]string{"outcome"},
)

// AggregatorConfidenceComputeTotal — counter of confidence-score
// compute outcomes per (pair, window) per tick (ADR-0019 §"Multi-
// factor confidence score"). Outcome labels:
//
//   - ok                       — score computed + cached cleanly
//   - skipped                  — first-tick / no prev-VWAP comparator
//   - baseline_missing         — MultiBaseline absent or in full bootstrap
//   - marshal_error            — score JSON encode failed (unreachable in practice)
//   - write_error              — Redis write of confidence: key failed
//   - divergence_read_error    — Redis Get on div:<asset> errored (best-effort; sentinel passed)
//   - divergence_decode_error  — div:<asset> JSON decode failed
//
// `skipped` and `baseline_missing` are normal during pair bring-up;
// `ok` should dominate in steady state. divergence_* errors are
// non-fatal (the confidence step continues with the "no data"
// sentinel) but sustained rates indicate the divergence worker /
// Redis is misbehaving.
var AggregatorConfidenceComputeTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_confidence_compute_total",
		Help: "Confidence-score compute outcomes per (pair, window) × tick. See package docs for the full label vocabulary.",
	},
	[]string{"outcome"},
)

// VerifyArchiveLedgersVerified — counter of ledgers successfully
// walked + verified. Rate over time gives ledgers/sec per chunk —
// the primary signal for spotting a stalled chunk vs a slow one.
var VerifyArchiveLedgersVerified = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_verify_archive_ledgers_verified_total",
		Help: "Ledgers walked + verified by verify-archive, per chunk_idx.",
	},
	[]string{"chunk_idx"},
)

// VerifyArchiveCurrentLedger — gauge of the most-recent ledger
// position per chunk. Together with the chunk's [from,to] range
// (operator-known) gives a percent-complete view; together across
// chunks gives a ledger-distance-fan picture of which chunks are
// leading vs trailing.
var VerifyArchiveCurrentLedger = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_verify_archive_current_ledger",
		Help: "Most-recent ledger sequence verified by each chunk_idx.",
	},
	[]string{"chunk_idx"},
)

// VerifyArchiveCheckpointsTotal — counter of Tier B checkpoint
// outcomes (matched | missed). missed=archive file absent (warning
// or hard fail under -fail-on-missed); matched=hash-equal proof.
var VerifyArchiveCheckpointsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_verify_archive_checkpoints_total",
		Help: "Tier B checkpoint outcomes per verify-archive chunk_idx, labelled by outcome (matched|missed).",
	},
	[]string{"chunk_idx", "outcome"},
)

// VerifyArchiveMismatchesTotal — counter of chain-break /
// checkpoint-mismatch / sequence-gap incidents. Any non-zero
// reading is a hard failure; the counter exists so dashboards can
// distinguish "mismatch fired and the run aborted at second X"
// from "chunk aborted for an unrelated reason (canceled context)".
var VerifyArchiveMismatchesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_verify_archive_mismatches_total",
		Help: "Chain breaks, sequence gaps, and checkpoint mismatches per verify-archive chunk_idx + reason (chain|sequence|checkpoint).",
	},
	[]string{"chunk_idx", "reason"},
)

// PostgresPingTotal — counter of resilience probes the indexer's
// `watchPostgresPing` goroutine fires (every 60 s) against the
// Timescale pool. Outcome label is `ok` for a successful Ping and
// `error` for any failure mode (timeout, connection refused, dead
// pool, network blip).
//
// F-0151 (audit-2026-05-26): the 2026-05-26 cascade left the
// indexer's *sql.DB pool with stale conns AFTER postgres@15-main
// recovered. Live ingest silently stalled for ~14 h until a manual
// restart. The pool now retires conns every `PoolConnMaxLifetime`
// regardless of liveness; this counter is the OBSERVABILITY signal
// so the next cascade surfaces in minutes via
// `stellarindex_postgres_ping_failing` instead of hours of silent
// drift.
//
// Alert on `rate(stellarindex_postgres_ping_total{outcome="error"}[5m]) > 0`
// for 2 m → page. A handful of failures during postgres restart is
// expected; a sustained non-zero rate means the pool is wedged.
var PostgresPingTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_postgres_ping_total",
		Help: "Indexer postgres-resilience ping outcomes. Sustained error rate ⇒ pool is dead and the lifetime safety-net hasn't refreshed yet (F-0151).",
	},
	[]string{"outcome"},
)

// PostgresPingFailureStreak — gauge tracking the consecutive
// failed-ping count. Resets to 0 on a successful ping. Used by the
// indexer's resilience goroutine to log a structured warning at
// every 3-failure threshold, and exposed so dashboards can chart
// the live streak length alongside the cumulative
// [PostgresPingTotal].
//
// Pair with the rate-based alert: a sustained streak > 0 for >2 m
// is the page signal. F-0151.
var PostgresPingFailureStreak = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_postgres_ping_failure_streak",
		Help: "Indexer postgres-ping consecutive failure count (resets to 0 on the next success). F-0151.",
	},
)

// TLSCertNotAfterUnix — per-host gauge of the public TLS cert's
// NotAfter timestamp (Unix seconds). Set by the API binary's
// self-probe goroutine on a 6 h cadence: a `tls.Dial(host:443)`
// captures the cert chain, the leaf's NotAfter is emitted here.
//
// F-0051 (audit-2026-05-26): Caddy auto-renews Let's Encrypt 30 d
// before expiry, but if renewal fails (DNS, rate limit, ACME
// quota) we historically discovered only at cert expiry. This
// gauge gives the alert rule a producer: fire on
// `(TLSCertNotAfterUnix - time()) < 14*24*3600` to catch a stuck
// renewal cycle with 2-week head room.
//
// Cardinality: one host per series; the operator-curated list is
// typically the apex + 1–2 subdomains (api / status). Probe
// failures DO NOT clear the gauge — the last-known value stays in
// place until the next successful probe, so a transient outage
// doesn't blank the alert input. Separate counter
// [TLSCertProbeTotal] tracks probe outcome.
var TLSCertNotAfterUnix = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_tls_cert_not_after_unix",
		Help: "Unix-seconds NotAfter timestamp of the leaf TLS cert observed at the configured host. Set by the API binary's self-probe (F-0051). Probe failures keep the last-known value; pair with stellarindex_tls_cert_probe_total{outcome=error} to detect a stuck probe.",
	},
	[]string{"host"},
)

// TLSCertProbeTotal — per-(host, outcome) probe outcome counter.
// outcome ∈ {ok, dial_error, no_cert, timeout}. A growing `ok`
// rate while [TLSCertNotAfterUnix] stays flat is the success
// signal; an `error` outcome alongside a stale gauge means the
// probe is failing and the operator should investigate before
// the gauge ages out via the alert rule's `14 day` threshold.
var TLSCertProbeTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_tls_cert_probe_total",
		Help: "TLS cert self-probe outcomes per host. outcome ∈ {ok, dial_error, no_cert, timeout}. F-0051.",
	},
	[]string{"host", "outcome"},
)

// StripePlatformSyncErrorsTotal — counter of failures inside the
// Stripe webhook's platform-store side-effects path
// (`internal/api/v1/stripe_webhook.go::applyPlatformSideEffects`).
// The webhook deliberately does NOT 5xx on platform-store failures
// (Stripe retries would just keep applying the same Redis rate-
// limit without making the platform-store path any healthier), so
// this counter is the operator-visible signal that the bridge is
// degraded. Any non-zero reading is alertable — the customer's
// dashboard / Postgres-backed key state is drifting from their
// Stripe billing state.
//
// Labels:
//   - operation: which step failed
//     (get_account|upsert_subscription|account_update|list_keys|
//     key_update|key_cache_invalidate)
var StripePlatformSyncErrorsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_stripe_platform_sync_errors_total",
		Help: "Stripe webhook platform-store side-effect failures, labelled by operation. Non-zero = bridge degraded; customer dashboard state drifting from Stripe billing state.",
	},
	[]string{"operation"},
)

// ChLiveSinkLedgersTotal — count of ledgers processed by the
// ClickHouse real-time dual-sink (ADR-0034 #18), labelled by
// `outcome`:
//   - "written"  — durably flushed to ClickHouse (post-Flush).
//   - "buffered" — accepted into the in-memory buffer (pre-flush);
//     written - buffered ≈ the unflushed backlog and is the
//     early-warning signal of a CH write stall.
//   - "dropped"  — bounded-dropped: a full channel (live ingest
//     out-paced the worker) or a full Sink buffer during a
//     sustained CH outage (G12-01). The ch-live-catchup gap-scan
//     timer heals dropped ledgers; a steady non-zero climb means
//     the live edge of the lake is degrading.
//   - "errored"  — a failed Add / Flush operation. A climb is a
//     CH write-path fault (down / wedged / disk-full).
//
// The indexer's periodic stats goroutine samples the LiveSink's
// monotonic counters and emits the per-tick DELTA. Pre-seeded with
// all four label values so the series exist at boot when the sink
// is enabled.
var ChLiveSinkLedgersTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_ch_live_sink_ledgers_total",
		Help: "Ledgers processed by the ClickHouse real-time dual-sink, labelled by outcome (written|buffered|dropped|errored).",
	},
	[]string{"outcome"},
)

// MarketsSkippedRowsTotal — count of trades rows the /v1/markets
// scanner skipped because their base_asset / quote_asset failed
// to parse as canonical asset strings. The ingest pipeline only
// emits canonical asset codes, so any non-zero reading means
// something bypassed the normal write path (manual SQL insert,
// integration test residue, etc.). 2026-06-01 incident: a single
// row with base_asset='test' tripped a page-tier api_error_rate
// alert because the handler returned 500 on the unparseable row;
// the handler now skips + bumps this counter instead, but a
// rising value should still trigger a `DELETE FROM trades` clean-
// up. Bounded label set (none) so the metric is always emitted.
var MarketsSkippedRowsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_markets_skipped_rows_total",
		Help: "Count of trades rows skipped by the /v1/markets scanner because base/quote did not parse as canonical asset strings. Non-zero indicates non-pipeline writes; investigate and clean up.",
	},
)

// DEXTradeNonstandardDecimalsTotal — the decimals-assumption landmine
// detector (adversarial-review HIGH-latent, decoder-correctness audit
// Finding 2). Emitted by the aggregator's decimals-guard sweep
// (internal/decimalsguard) once per (source, asset) the FIRST time a
// DEX trade is observed for a Soroban-contract token whose ON-CHAIN
// decimals() != 7.
//
// Why it matters: the served price is Σ(quote_amount)/Σ(base_amount) on
// RAW smallest-unit integers — in the prices_* continuous aggregates
// (migrations/0002) and in aggregate.VWAP. The per-asset decimals CANCEL
// in that ratio ONLY when base and quote share the same scale. Every
// DEX-traded Stellar token today is 7-decimal (SACs are always 7;
// pure-SEP-41 tokens observed so far all declare decimals=7), so the
// ratio is correct. The moment a non-7-decimal SEP-41 token (an
// 18-decimal bridged asset, a 6-dp token, …) gains DEX liquidity, every
// served price for a pair involving it silently skews by 10^(7−decimals)
// with NO other signal. This counter turns that silent landmine into a
// loud, per-asset signal so the operator can apply the decimals
// normalization (deferred follow-up — see the runbook) BEFORE customers
// consume a wrong price.
//
// Labels: `source` (the DEX connector that traded it — soroswap /
// phoenix / aquarius / comet / …) and `asset` (the token's C-strkey
// contract id). The label set is unbounded in principle but near-empty
// in practice (offenders should be zero), so it is NOT pre-seeded and a
// series exists ONLY once a real offender is detected — the alert is a
// bare `> 0`. The actual decimals value is logged (ERROR) at detection,
// not carried as a label.
var DEXTradeNonstandardDecimalsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_dex_trade_nonstandard_decimals_total",
		Help: "DEX trades observed for a Soroban token whose on-chain decimals() != 7 — the served price for pairs involving this asset is silently skewed by 10^(7-decimals). Labels: source, asset (C-strkey). Any non-zero value is an unmitigated mispricing landmine; see runbook dex-nonstandard-decimals.md.",
	},
	[]string{"source", "asset"},
)

// PriceServeDeclinedNonstandardDecimalsTotal — the READ-TIME enforcement
// half of the dex-nonstandard-decimals guard (API binary; the sweep above
// is the aggregator-side DETECTION half). Confirmed production bug
// 2026-07-08: token CC2RB…(decimals()=9) served a price exactly 100x wrong
// on its aquarius/USDC pair for 35 trades because nothing stopped serving
// once the assumption broke. `internal/api/v1`'s NonstandardDecimalsCache
// mirrors `nonstandard_decimals_assets` (migration 0093, upserted by
// internal/decimalsguard on confirmation) and declines to serve /v1/price,
// /v1/vwap, /v1/history, /v1/ohlc — 422 problem+json — for any pair with a
// leg in that table. This counter fires once per DECLINED request (not
// latched like the detector counter above): it tracks live customer
// impact, so operators can see it taper to zero once the durable decimals
// normalization ships and the offending row is removed.
//
// Label: `asset` — the flagged leg's C-strkey contract id (whichever of
// base/quote matched; if both are flagged, the base wins — see
// nonstandardDecimalsLeg in internal/api/v1). Unbounded in principle,
// near-empty in practice, so NOT pre-seeded.
var PriceServeDeclinedNonstandardDecimalsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_price_serve_declined_nonstandard_decimals_total",
		Help: "Price-surface requests (/v1/price, /v1/vwap, /v1/history, /v1/ohlc) declined because a leg of the requested pair is a confirmed non-7-decimal Soroban asset. Label: asset (C-strkey). Non-zero means real requests are being honestly declined instead of served a wrong price; see runbook dex-nonstandard-decimals.md.",
	},
	[]string{"asset"},
)

// NonstandardDecimalsCacheRefreshFailuresTotal counts failed background
// refreshes of the API's in-process NonstandardDecimalsCache
// (internal/api/v1) — the read-time mirror of `nonstandard_decimals_assets`
// that /v1/price, /v1/vwap, /v1/history, /v1/ohlc consult before serving.
// The cache is fail-open on a refresh error (serves the last-good snapshot
// rather than clearing it — availability wins over the guard for infra
// blips), so a rising value is an infra-health signal, not a pricing-
// correctness one: it means the cache is coasting on a stale snapshot, not
// that a wrong price is being served. No dedicated alert — a sustained
// climb is visible via this counter and the underlying Postgres-health
// alerts already cover the infra failure itself.
var NonstandardDecimalsCacheRefreshFailuresTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_nonstandard_decimals_cache_refresh_failures_total",
		Help: "Failed background refreshes of the API's in-process nonstandard-decimals serving-guard cache. Fail-open: the previous snapshot keeps serving. Infra-health signal, not a pricing-correctness one.",
	},
)

// ─── hashdb (ADR-0016 drift detector) ───────────────────────────────

// HashdbAppendTotal — per-outcome counter for the indexer's hashdb
// append call, made once per ledger from the live LCM read loop
// (cmd/stellarindex-indexer/main.go). Labels:
//
//   - `ok`    — hashdb.Append succeeded; the ledger's sha256(LCM) is
//     durably recorded.
//   - `error` — hashdb.Append failed (disk full, permission error,
//     out-of-range seq). Append is deliberately failure-tolerant —
//     an error here logs + increments this counter and NEVER stalls
//     or fails ingest (see the recordHashdb docstring). A sustained
//     `error` rate means hashdb is silently not recording anything —
//     the periodic verify sweep would then find nothing to compare
//     against (Missing, not Drifted) rather than catching real
//     drift, so this counter is the operator's only signal that the
//     detector has gone blind.
var HashdbAppendTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_hashdb_append_total",
		Help: "Indexer hashdb.Append outcomes per ledger, labelled by outcome (ok|error). error means the drift detector is silently not recording — see the hashdb-drift-detected runbook.",
	},
	[]string{"outcome"},
)

// HashdbAppendDurationSeconds — latency histogram for the per-ledger
// hashdb.Append call. hashdb.Append is a single O(1) positional
// WriteAt (fixed 32-byte record, no seek-to-end, no fsync) — this
// runs synchronously on the ingest hot path (see recordHashdb's
// docstring for why that's an acceptable trade), so a latency
// regression here (a slow disk, an unexpectedly large hashdb file
// hitting page-cache pressure) would directly show up as ingest lag.
// Buckets span 10 µs → 10 ms — generous for a page-cache-resident
// single-record write; anything reaching the top bucket is worth
// investigating.
var HashdbAppendDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_hashdb_append_duration_seconds",
		Help:    "Per-ledger hashdb.Append latency, labelled by outcome (ok|error). Runs synchronously on the ingest hot path — a regression here is ingest lag.",
		Buckets: []float64{0.00001, 0.00005, 0.0001, 0.0005, 0.001, 0.005, 0.01},
	},
	[]string{"outcome"},
)

// HashdbVerifyRunsTotal — per-outcome counter for the indexer's
// periodic hashdb verify sweep (re-reads a trailing window from the
// same galexie bucket and compares against hashdb; see
// internal/archivecompleteness.HashDBWindowVerifier). Labels:
//
//   - `ok`    — the sweep completed with zero drifted ledgers in the
//     window (Missing/OutOfRange ledgers don't count against this —
//     they're expected while the window predates hashdb's coverage).
//   - `drift` — the sweep completed and found at least one drifted
//     ledger. See HashdbDriftTotal for the per-ledger drift count
//     this alerts on.
//   - `error` — the sweep itself failed (datastore read error,
//     hashdb I/O error) before it could finish comparing the window.
//     Distinct from `drift`: this means "we don't know", not "we
//     found a mismatch".
var HashdbVerifyRunsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_hashdb_verify_runs_total",
		Help: "Indexer periodic hashdb verify-sweep outcomes, labelled by outcome (ok|drift|error).",
	},
	[]string{"outcome"},
)

// HashdbVerifyRunDurationSeconds — latency histogram for one full
// verify sweep (re-reads [VerifyWindowLedgers] ledgers from the
// bucket, one hashdb.Verify call per ledger). Labelled by outcome so
// operators can chart `ok` p95/p99 (bucket-fetch-bound; the same
// shape as a bounded backfill walk over the window size) separately
// from `error` (often a fast-fail on the first missing/unreadable
// object). Buckets span 1 s → 30 min — a multi-thousand-ledger S3
// re-read is not cheap, unlike the append side.
var HashdbVerifyRunDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_hashdb_verify_run_duration_seconds",
		Help:    "Per-sweep latency of the hashdb periodic verify pass, labelled by outcome (ok|drift|error).",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200, 1800},
	},
	[]string{"outcome"},
)

// HashdbDriftTotal is the dedicated alert-driving counter: the total
// number of DRIFTED LEDGERS (not sweeps) hashdb's periodic verify has
// found across every sweep since process start. Deliberately a plain
// (unlabelled) Counter, not a Vec — the natural label candidate
// (ledger sequence) is unbounded per-region cardinality, which
// Prometheus labels must never be. `stellarindex_hashdb_drift_total
// > 0` is the alert condition (see
// docs/operations/runbooks/hashdb-drift-detected.md); the per-run
// breakdown lives in HashdbVerifyRunsTotal{outcome="drift"} and the
// loudly-logged WARN/ERROR line (which does name the drifted
// sequences) at the point of detection.
var HashdbDriftTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_hashdb_drift_total",
		Help: "Cumulative count of ledgers hashdb's periodic verify sweep found drifted (recorded hash != freshly-observed hash). Any nonzero value means upstream history was rewritten or our lake object is corrupted — see the hashdb-drift-detected runbook.",
	},
)
