package obs

import (
	"net/http"
	"time"

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

		SourceEventsTotal,
		SourceLastEventUnix,
		SourceLastInsertUnix,
		SourceEnabled,
		SourceMatchedEventsTotal,
		SourceDecodeErrorsTotal,
		SourceUnknownSymbolsTotal,
		SourceOrphanEventsTotal,
		AMMSelfPairSwapTotal,
		ExternalPollerPollsTotal,
		ExternalPollerLastSuccessUnix,
		ExternalFXLastQuoteUnix,
		ExternalFXRateRejectedTotal,
		ExternalFXBaselineHealedTotal,
		ExternalDustDroppedTotal,
		CEXStreamDisconnectTotal,
		DiscoveryDroppedHitsTotal,
		DiscoverySkippedHitsTotal,
		DiscoveryRecordFailuresTotal,
		MetricsRegistryPresent,
		SourceInsertErrorsTotal,
		RateLimitFailOpenTotal,
		MonthlyQuotaFailOpenTotal,
		MonthlyQuotaFailClosedTotal,
		AdminAuditWriteFailuresTotal,
		AdminKeyBudgetClampsTotal,
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
		OracleStreamRowsUnparsedTotal,
		OracleResolutionSeconds,

		AggregatorTicksTotal,
		AggregatorVWAPWritesTotal,
		AggregatorVWAPCacheWriteErrorsTotal,
		AggregatorEmptyWindowsTotal,
		AggregatorWindowTruncatedTotal,
		AggregatorStreamPublishTotal,
		AnomalyWarnTotal,
		CustomerWebhookDeliveryAttemptsTotal,
		CustomerWebhookFanoutFailuresTotal,
		AggregatorDroppedTradesTotal,
		AggregatorVenueVWAP, AggregatorWindowTrades,
		AggregatorDroppedWindowsTotal,
		AggregatorMinUSDVolumeUnvaluableTotal,
		PriceServeSubstanceWithheldTotal,
		PriceServeScamWithheldTotal,

		SupplyCrossCheckDivergenceStroops,
		SupplyCrossCheckTotal,
		SupplyDivergenceRatio,
		SupplyDivergenceTotal,

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

		ChLiveSinkLedgersTotal,

		MarketsSkippedRowsTotal,
	)
	registerProjectorMetrics()
	registerAPIServingMetrics()
	registerFreezeLifecycleMetrics()
	registerAppMetricsTail()
}

// registerProjectorMetrics registers the ADR-0032 projector family — lag,
// cycle outcomes, decoded events, cycle latency, and the two operator-facing
// flags (wedge + replay window). Peeled off [registerAppMetrics] for the same
// reason [registerFreezeLifecycleMetrics] was: the set only grows, and the
// funlen ceiling is what forces the split rather than a silently ever-longer
// function.
func registerProjectorMetrics() {
	Registry.MustRegister(
		ProjectorLagLedgers,
		ProjectorRunsTotal,
		ProjectorEventsDecoded,
		ProjectorCycleDurationSeconds,
		ProjectorWedged,
		ProjectorReplayWindowActive,
	)
}

// registerAPIServingMetrics registers the API serving-path family — the
// read-path caches, the per-row sparkline result counter, the stream
// subscription counter, and the CORS decision counter. Peeled off
// [registerAppMetrics] for the same reason as the freeze-lifecycle group:
// the set only grows, and the funlen ceiling is what forces the split
// rather than a silently ever-longer function.
func registerAPIServingMetrics() {
	Registry.MustRegister(
		APICacheOpsTotal,
		APISparkline7dRowsTotal,
		APIStreamSubscribeTotal,
		APICORSDecisionsTotal,
	)
}

// registerFreezeLifecycleMetrics registers the ADR-0019 freeze-lifecycle
// family — fire / extend / escalate / release, the live-freeze gauge, and
// the recovery worker's own counters. A self-contained group, peeled off
// [registerAppMetrics] for the same reason that function was peeled off
// init(): the set only grows, and the funlen ceiling is what forces the
// split rather than a silently ever-longer function.
func registerFreezeLifecycleMetrics() {
	Registry.MustRegister(
		AnomalyFreezeEngagedTotal,
		AnomalyFreezeEscalatedTotal,
		AnomalyFreezeExtensionsTotal,
		AnomalyFreezeReleasedTotal,
		AnomalyFreezeActive,
		AnomalyFreezeRecoveredTotal,
		AnomalyFreezeLadderRehydratedTotal,
		AnomalyFreezeLadderWriteFailuresTotal,
		AnomalyFreezeRecoverySweepsTotal,

		// Composite-reference corroboration of the phase-2 verdict
		// (2026-08-29) — freeze-decision metrics, so they live here.
		AggregatorCompositeCorroboration,
		AggregatorCompositeReferenceLegSources,
		AggregatorCompositeReferenceLegDispersionBps,
		AggregatorCompositeFreezeSuppressedTotal,
	)
}

// registerAppMetricsTail registers the remainder of the app metric set
// — split out of registerAppMetrics purely to keep each function under
// the funlen ceiling as the metric set grows (same reason init() calls
// registerAppMetrics).
func registerAppMetricsTail() {
	Registry.MustRegister(
		// Readiness-check gauge (#371 F2) — the only alertable signal
		// ClickHouse has, since it is the one dependency on r1 with no
		// Prometheus exporter of its own.
		DependencyUp,

		// Source-family counter (#291). It belongs beside
		// SourceUnknownSymbolsTotal in [registerAppMetrics] and is
		// registered here only because that function already sat exactly
		// on the funlen ceiling, so one more line made it lint-red —
		// the "or registerAppMetricsTail(), whichever keeps funlen happy"
		// branch of docs/contributing/add-metric.md. Registration, not
		// placement, is what puts it on /metrics: see
		// TestHandler_ExposesMetrics, which scrapes for this name.
		SourceUnrepresentableSymbolsTotal,

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

		AssetCharacterRollupSweepsTotal,
		AssetCharacterRollupSweepDurationSeconds,

		PriceAlertEvalTotal,
		PriceAlertEvalDurationSeconds,

		AssetsPopularPriceless,
		PricelessCoverageCheckRunsTotal,
		PricelessCoverageCheckLastSuccessUnix,

		SignupReaperRunsTotal,
		SignupReaperRunDurationSeconds,
		SignupReaperRowsDeletedTotal,

		LoginCodeLockoutRows,
		LoginCodeLockoutRowsDeletedTotal,
		LoginCodeLockoutErrorsTotal,

		MagicLinkTokenRows,
		MagicLinkTokenRowsDeletedTotal,
		MagicLinkTokenErrorsTotal,
		NotifySendsTotal,

		DEXTradeNonstandardDecimalsTotal,
		PriceServeDeclinedNonstandardDecimalsTotal,
		NonstandardDecimalsCacheRefreshFailuresTotal,

		HashdbAppendTotal,
		HashdbAppendDurationSeconds,
		HashdbVerifyRunsTotal,
		HashdbVerifyRunDurationSeconds,
		HashdbDriftTotal,

		LedgerstreamTierReadTotal,
		LedgerstreamColdReadDurationSeconds,

		DEXTVLRefreshTotal,
		DEXTVLRefreshDurationSeconds,
		SDEXOrderBookMaintainTotal,
		SDEXOrderBookMaintainDurationSeconds,
		SDEXOrderBookCrossedPairs,
		SDEXOrderBookPendingOffers,
		SDEXOrderBookUndecodableOffersTotal,
		ExplorerSWRRefreshTotal,
		ExplorerSWRRefreshDurationSeconds,
		ProtocolDetailRefreshTotal,
		ProtocolDetailRefreshDurationSeconds,
	)

	seedBoundedLabelSeries()
}

// seedBoundedLabelSeries pre-registers the zero-valued label combinations
// alert rules select on. Split out of [registerAppMetrics] for the same
// reason that function was split out of init(): the set only grows, and the
// funlen ceiling is what forces the split rather than a silently
// ever-longer function.
func seedBoundedLabelSeries() {
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
	// `frozen_leg` (MNY-22) landed after the original four and was not
	// seeded with them — so the one outcome that means "we refused to
	// publish a derived price because a leg was frozen" was the one
	// outcome an operator could not distinguish from "this metric is
	// dead" until it first fired.
	for _, outcome := range []string{"ok", "missing_leg", "parse_error", "redis_error", "frozen_leg", "low_confidence"} {
		AggregatorTriangulationsTotal.WithLabelValues(outcome)
	}
	// The self-pair exploit detector is EXPECTED to sit at zero indefinitely
	// (comet emitted none before the 2026-08-25 window), so without seeding an
	// operator could not tell "armed but quiet" from "dead metric / never
	// deployed" — the exact F-0033 ambiguity. Its `source` label is bounded to
	// the single known producer (comet).
	AMMSelfPairSwapTotal.WithLabelValues("comet")
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
		AssetCharacterRollupSweepsTotal.WithLabelValues(outcome)
	}
	for _, outcome := range []string{"ok", "list_error", "partial_error"} {
		PriceAlertEvalTotal.WithLabelValues(outcome)
	}
	for _, outcome := range []string{"ok", "error"} {
		SignupReaperRunsTotal.WithLabelValues(outcome)
	}
	// C3-032: the durable login-code lockout's three fail-soft paths.
	// Seeded because all three are silent at the HTTP layer — a
	// fail-open control with an ABSENT series is indistinguishable from
	// one that has never failed, which is the exact ambiguity that let
	// this class of defect through before.
	for _, op := range []string{
		LoginCodeLockoutOpStatusCheck, LoginCodeLockoutOpRegister, LoginCodeLockoutOpSweep,
	} {
		LoginCodeLockoutErrorsTotal.WithLabelValues(op)
	}
	// PRV-2: the magic-link retention sweep's single fail-soft path.
	// Seeded so a background janitor that has never failed and one that
	// emits no series are distinguishable — same reasoning as the
	// login-code lockout reaper above.
	for _, op := range []string{MagicLinkTokenOpSweep} {
		MagicLinkTokenErrorsTotal.WithLabelValues(op)
	}
	seedNotifySeries()
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
	// the hashdb_verify_failing alert expr and ad-hoc append-error-rate
	// charts (no alert exists on the append counter — a documented
	// decision, see docs/reference/metrics/README.md) read a real zero
	// (not "no data") on a freshly-enabled region before the first
	// tick / first ledger.
	for _, outcome := range []string{"ok", "error"} {
		HashdbAppendTotal.WithLabelValues(outcome)
		HashdbVerifyRunsTotal.WithLabelValues(outcome)
	}
	HashdbVerifyRunsTotal.WithLabelValues("drift")
	seedLedgerstreamTierSeries()
	// ADR-0019 freeze-lifecycle release modes. Bounded set of two;
	// `operator` in particular is the one an on-call reads as "the
	// calibration is producing freezes humans keep undoing", and until
	// the first manual unfreeze it would otherwise be absent from
	// scrape output — indistinguishable from a dead metric.
	for _, mode := range []string{"auto", "operator"} {
		AnomalyFreezeReleasedTotal.WithLabelValues(mode)
	}
	// Durable-ladder write sites (migration 0119). Bounded set of two;
	// seeded because this counter's whole purpose is to make a SILENT
	// failure visible, and an absent series is itself a silence.
	for _, op := range []string{"mark_hold", "clear"} {
		AnomalyFreezeLadderWriteFailuresTotal.WithLabelValues(op)
	}
	// C3-067: privileged-mutation surfaces whose audit row can fail to
	// land. Bounded and enumerated at the six call sites; seeded so the
	// alert's increase() reads a real zero instead of "no data" on a
	// freshly-deployed API — the state that is otherwise
	// indistinguishable from "this metric is dead", which is exactly the
	// gap that let the audit-write failure go unobserved in the first place.
	for _, surface := range []string{
		"account_override", "key_mint", "key_revoke",
		"status_notice",
		// C3-056: the staff customer look-up is a PII READ rather than a
		// mutation, but the accountability gap is identical — the row
		// that records who read whose data is the only trace it happened.
		"staff_customer_lookup",
	} {
		AdminAuditWriteFailuresTotal.WithLabelValues(surface)
	}
	// Tier-clamp outcomes — bounded set of two. `failed` in particular
	// means paid throughput stayed live past a downgrade, so it must be
	// distinguishable from "nothing has been clamped yet".
	for _, outcome := range []string{"lowered", "failed"} {
		AdminKeyBudgetClampsTotal.WithLabelValues(outcome)
	}
	// C3-023: producer-side webhook fan-out losses. The event-type set
	// is platform.WebhookEventType's closed enum (kept as literals here
	// so internal/obs stays free of an internal/platform import); the
	// reason set is the three FanoutFailure* constants above. Seeded
	// because a lost fan-out leaves NO delivery row to count later —
	// "this series has never fired" and "nothing publishes this metric"
	// would otherwise be the same scrape.
	for _, eventType := range []string{
		"incident.sev1", "incident.resolved", "anomaly.freeze",
		"divergence.firing", "price.alert",
	} {
		for _, reason := range []string{
			FanoutFailureInvalidPayload, FanoutFailureListSubscribers, FanoutFailureEnqueue,
		} {
			CustomerWebhookFanoutFailuresTotal.WithLabelValues(eventType, reason)
		}
	}
	// C2-030: FX sanity-band rejections. Bounded (one source × three
	// reasons) and seeded because the alertable state is a SUSTAINED
	// non-zero rate — an absent series would make "the band has never
	// rejected anything" and "the worker never started" identical.
	for _, reason := range []string{"deviation", "non_positive", "non_finite"} {
		ExternalFXRateRejectedTotal.WithLabelValues("massive", reason)
	}

	seedBoundedLabelSeriesTail()
}

// seedBoundedLabelSeriesTail continues seedBoundedLabelSeries — split
// for the same gocognit ceiling that split registerAppMetrics.
func seedBoundedLabelSeriesTail() {
	// Both divergence guards (stellarindex_divergence_no_reference and
	// _refresh_error_dominant) compare a FAILURE outcome's rate against
	// the `ok` outcome's rate. Without seeding, a process that has never
	// had a successful refresh has no `ok` child at all, the comparison
	// is the empty vector, and BOTH alerts are silent in exactly the
	// total-outage case they exist to catch — CoinGecko and Chainlink
	// both unreachable, the aggregator restarted during the outage (as
	// deploys routinely do), so `ok` is never registered while
	// flags.divergence_warning serves frozen and a live depeg goes
	// unflagged (wave-D ALERT-06). This was the one alert-referenced
	// outcome counter missing from this list.
	//
	// Values mirror internal/aggregate/orchestrator/divergence_refresh.go
	// exactly: no_vwap, parse_error, the refresh_error/no_reference pair,
	// and ok.
	for _, outcome := range []string{"ok", "no_vwap", "parse_error", "refresh_error", "no_reference"} {
		DivergenceRefreshTotal.WithLabelValues(outcome)
	}
	// v0.21.4 background cache workers. Seeded so the DEX-TVL /
	// order-book failure-rate queries read a real zero (not "no data")
	// from process start; the load_* pair matters most — a process
	// whose initial book load never even attempted looks identical to
	// a healthy one without the zero series.
	for _, outcome := range []string{"ok", "error"} {
		DEXTVLRefreshTotal.WithLabelValues(outcome)
	}
	for _, outcome := range []string{"load_ok", "load_error", "advance_ok", "advance_error"} {
		SDEXOrderBookMaintainTotal.WithLabelValues(outcome)
	}
	for _, cache := range []string{
		"accounts_wealth", "asset_holders", "contract_detail", "contracts_dir",
		"network_throughput", "op_type_stats", "protocol_bespoke", "ttl_liveness",
	} {
		for _, outcome := range []string{"ok", "error"} {
			ExplorerSWRRefreshTotal.WithLabelValues(cache, outcome)
		}
	}
}

// seedLedgerstreamTierSeries pre-registers the ADR-0027 tiered-read
// outcomes so the ledgerstream-tier `both_missing` page's
// `increase(...) > 0` query reads a real zero (not "no data") before the
// first cold read. Without this the both_missing series is absent until
// the cold path first runs, which is precisely the "looks dead vs is
// dead" ambiguity W5-mon-3 closed by making this metric always-registered.
// Peeled into its own helper for the same gocognit ceiling that split
// seedBoundedLabelSeries.
func seedLedgerstreamTierSeries() {
	for _, outcome := range []string{"hot", "cold", "both_missing"} {
		LedgerstreamTierReadTotal.WithLabelValues(outcome)
	}
}

// seedNotifySeries pre-registers the Resend transactional-email send outcomes
// (task #33 / W8 recon 9c). Bounded: the two notify.Sender call sites
// (magic-link login, signup verification) × {sent, failed}. Seeded so the
// send-failure-ratio alert reads a real 0 before the first login/signup email
// — an absent series would make "no mail has ever failed" and "the mailer is
// dead" the same scrape (the exact silence this counter closes). Peeled into
// its own helper for the same gocognit ceiling that split
// seedLedgerstreamTierSeries.
func seedNotifySeries() {
	for _, template := range []string{NotifyTemplateMagicLink, NotifyTemplateSignupVerify} {
		for _, result := range []string{NotifySendResultSent, NotifySendResultFailed} {
			NotifySendsTotal.WithLabelValues(template, result)
		}
	}
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
// DependencyUp reports the result of each /v1/readyz readiness check
// as an alertable gauge: 1 when the dependency answered, 0 when it did
// not.
//
// The API has always CHECKED its dependencies — computeReadyz pings
// postgres, schema, redis and clickhouse on every readiness round — but
// the outcome existed only as JSON on an HTTP endpoint. Nothing scraped
// it, so nothing could alert on it.
//
// That left ClickHouse with no health signal at all (#371 F2).
// Postgres, Redis and MinIO each have a Prometheus exporter on r1;
// ClickHouse has none, and it is the raw lake — the substrate the
// ADR-0033 completeness claim rests on. If it went away, the only
// symptom would be endpoints failing one by one.
//
// Exposing the existing check is deliberately cheaper than adding a
// ClickHouse exporter: it needs no new scrape target, no new package on
// the host, and it measures the thing that actually matters — whether
// the API can reach the dependency — rather than whether a sidecar can.
// It also covers every dependency at once rather than just the one that
// prompted it.
//
// Gauge semantics: overwritten on every readiness round, so it reflects
// the most recent check rather than a historical high-water mark. A
// dependency that disappears from the check set stops being reported;
// alert on `== 0`, never on `absent()` alone, or a renamed check reads
// as an outage.
var DependencyUp = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_dependency_up",
		Help: "Result of each /v1/readyz readiness check: 1 = the dependency answered, 0 = it did not. Covers postgres, schema, redis and clickhouse.",
	},
	[]string{"dependency"},
)

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
// `outcome` ∈ {ok, error, idle, sink_retry, decode_degraded}; rate is
// the alive-check (zero rate sustained 5+ minutes means the source's
// loop wedged). `decode_degraded` (DATA-6 / NS-2) marks a cycle that
// advanced the cursor but dropped at least one decode-failed row — a
// clean-looking advance that is NOT "ok"; a sustained per-source
// decode_error rate on those cycles is a decoder regression.
var ProjectorRunsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_projector_runs_total",
		Help: "Per-source projector cycle outcomes (ok, error, idle, sink_retry, decode_degraded). Rate goes to zero if the source's loop has wedged.",
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

// ProjectorWedged flags a per-source cursor WEDGE: the adaptive window
// has bottomed out at the MinBatchLimit floor AND the source has failed
// to commit forward progress for `projector.WedgeCycles` consecutive
// cycles. This is the ONE shrink-to-floor stall the adaptive window
// cannot escape on its own — a floor-sized (25-ledger) range that stays
// over PerSourceTimeout (a dense + compressed chunk) retries the
// identical range every cycle forever. The shrink halves the window on a
// deadline, but at the floor there is nothing left to halve, so lag stops
// falling and the ONLY prior signal was a flat
// stellarindex_projector_runs_total{outcome="error"} rate (which a busy
// error-storm from OTHER sources can mask). 1 = wedged; 0 = healthy.
// Cleared on any advancing cycle. Remediation is MANUAL and documented in
// the runbook (raise the per-cycle budget or decompress the range) — the
// projector deliberately does not change the shrink logic itself.
var ProjectorWedged = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_projector_wedged",
		Help: "Per-source projector wedge flag: 1 = the adaptive window is floored at MinBatchLimit and the source has failed to advance for WedgeCycles consecutive cycles (a stuck cursor that retries the identical range forever); 0 = healthy. Manual remediation — see the projector-wedged runbook.",
	},
	[]string{"source"},
)

// ProjectorReplayWindowActive flags that a source's projector cursor is
// still INSIDE an operator-recorded projection dirty window — i.e. a
// `stellarindex-ops projector-replay` deliberately rewound the cursor and
// it has not yet climbed back to where it was. 1 = inside the recorded
// rewind, 0 = outside it (the normal state).
//
// Why it exists (2026-08-29, reflector-fx): a replay is an INTENDED lag.
// The 2,574,496-ledger rewind that repaired the VES/XAU served-row deficit
// put `stellarindex_projector_lag_high` into a ~4h ticket that carried no
// information the operator did not already have — and, worse, MASKED a
// genuine lag on that same source for the whole window. This gauge is the
// discriminator the lag rule joins against (`unless … == 1`), so the
// expected lag is silent while the replay is climbing and the paired
// `stellarindex_projector_replay_stalled` rule tickets if it STOPS
// climbing (the failure that actually matters during a replay).
//
// THREE bounds keep the excuse narrow — a suppression is only ever as
// good as the proof it stays narrow (projector.replayWindowCovers holds
// them):
//
//  1. PROVENANCE. Only a window written by `projector-replay` counts. The
//     table's other writer, `projected-rebuild -write`, does NOT keep its
//     range below the live cursor: `-to` defaults to the live cursor, its
//     one-writer guard admits `liveLastLedger >= to` (equality), and
//     `-allow-live-overlap` bypasses the guard entirely (used on r1
//     2026-07-27). A rebuild window therefore routinely covers the
//     cursor's own position, and keying on the cursor alone would hold
//     this flag at 1 while a source is HELD there — the exact state the
//     lag ticket exists to catch, with no operator rewind on record to
//     explain the silence.
//  2. UPPER BOUND, EXCLUSIVE. Deliberately NOT "a dirty window row
//     exists": the row survives until compute-completeness re-verifies the
//     range (up to a day later). The flag clears the moment the cursor
//     REGAINS the window's `to_ledger` (its pre-rewind position); a
//     projector wedged exactly at that ledger has finished replaying and
//     stays fully alertable.
//  3. LOWER BOUND. `projector-replay` parks the cursor at
//     `from_ledger`-1, so a cursor below that was not put there by this
//     recorded rewind.
//
// Known residual (accepted, bounded): the table holds ONE row per source
// and the upsert WIDENS it (LEAST/GREATEST) while keeping the newest
// reason, so a replay recorded while a rebuild window is still pending
// yields a replay-reasoned row whose `to_ledger` may be the rebuild's.
// The flag then expires at that higher ledger instead of the replay's own
// pre-rewind position. It is still provenance-gated, still cursor-bounded
// and still expires; it needs both tools pending on the SAME source at
// once.
//
// Fails OPEN toward alerting: if the dirty-window read errors the gauge is
// forced to 0 for every source, so a monitoring-side failure can never
// silence a real lag ticket.
var ProjectorReplayWindowActive = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_projector_replay_window_active",
		Help: "1 while a source's projector cursor is inside an operator-recorded projector-replay rewind window (intended lag); 0 otherwise. Suppresses stellarindex_projector_lag_high and arms stellarindex_projector_replay_stalled.",
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
// The availability SLO (http_requests_total{status=~"5.."} — the
// label is `status`, NOT `status_class`, which does not exist on this
// CounterVec; corrected 2026-08-04, a selector copied from the old
// text would have matched nothing)
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
// this counter. The `result` label is a READ outcome — `hit`
// (returned cached value), `stale` (served a stale value while a
// refresh runs) or `miss` (called upstream) — or one of the two
// side-events the wrappers also record: `refresh_error` (a background
// refresh failed) and `evicted` (a bounded cache dropped its oldest
// entry to admit a new key). The `op` label names the cached method
// (e.g. `all_pools`, `distinct_pairs`, `list_coins`).
//
// Why: prewarm goroutines warm cache keys that MUST match what
// handlers look up. If those keys drift (different filter shape,
// different limit, different order), every user request becomes a
// miss while the prewarm slot sits unread. The bug is invisible to
// tests + log-greps, so an operator dashboard on hit-rate is the
// cheapest detector.
//
// Alert idea: `rate(stellarindex_api_cache_ops_total{result="miss"}
// [5m]) / rate(stellarindex_api_cache_ops_total{result=~"hit|miss|
// stale"}[5m]) > 0.5` sustained 10 min on any (cache, op) is
// suspicious — prewarm should keep hot ops > 90% hit. The denominator
// MUST stay filtered to the read outcomes: `evicted` fires once per
// admitted key, so an unfiltered ratio caps at 0.5 under exactly the
// key-enumeration storm the alert exists to catch.
var APICacheOpsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_api_cache_ops_total",
		Help: "Cache operations in API in-memory cache wrappers, labelled by cache name + op + result (read outcomes hit|miss|stale, side-events refresh_error|evicted).",
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
// source (i.e. InsertTrade returned with rowsInserted==1). Since INV-3
// a generation-guarded corrective UPDATE also returns 0 here, so this
// stamp does not climb during a re-derive that only corrects existing
// rows — see [TradeInsertOutcomeTotal]'s conflation note.
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

// SourceUnknownSymbolsTotal — per-source counter of asset slots in
// an otherwise-decoded oracle event whose symbol/feed id isn't in
// our canonical asset allow-list (ADR-0010 fiat / ADR-0014 crypto /
// ADR-0028 RWA + RedStone feed registry). Since the oracle
// capture-totality change (PR-2, docs/design/oracle-capture-
// totality-design.md) such a slot is RECORDED verbatim as a
// `raw:<symbol>` row (canonical.AssetOracleRaw) rather than dropped;
// the counter now means "recorded as raw" — a mapping gap the
// allow-list / feed-registry owner has to close so the row can be
// promoted in place, not data lost. Distinct from
// SourceDecodeErrorsTotal because the rest of the event decodes
// cleanly.
//
// F-1234 (codex audit-2026-05-12): upstream oracle coverage can
// expand while we silently omit the new asset; without this counter
// operators have no signal that a feed is unmapped. Reflector,
// Redstone, and Band all increment this on their unmapped-symbol
// (`!Asset.IsMapped()` / feed-registry miss) branches.
//
// Alert consumer: `stellarindex_ingestion_oracle_unknown_symbols`
// (deploy/monitoring/rules/ingestion.yml + the R1 overlay; runbook
// docs/operations/runbooks/oracle-unknown-symbols.md). The cold audit of
// 2026-08-04 found NO rule evaluated this counter — an earlier version
// of this comment claimed one in external-pollers.yml that never
// existed — while r1 already carried
// source_unknown_symbols_total{source="reflector"} 7794, i.e. 7,794
// oracle asset slots silently dropped from the price surface. The
// oracle capture-totality design (docs/design/oracle-capture-totality-
// design.md) now records those slots verbatim under
// `canonical.AssetOracleRaw` (decoders switched from skip to emit in
// PR-2); this counter keeps incrementing, because a raw row is still
// a mapping gap the allow-list owner has to close. Name deliberately
// unchanged (dashboards + the alert key off it).
var SourceUnknownSymbolsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_source_unknown_symbols_total",
		Help: "Asset slots in a decoded oracle event whose symbol/feed id isn't in the canonical allow-list; recorded verbatim as raw:<symbol> rows rather than dropped.",
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

// AMMSelfPairSwapTotal — per-source counter of AMM swap events decoded as a
// SELF-PAIR swap (token_in == token_out) and dropped to zero rows. A self-pair
// swap has NO honest purpose: it moves no value between distinct assets, and
// it is the primitive the 2026-08-25 Blend/Comet exploit ran 390 times to
// walk a pool's spot price. Historically comet emitted ZERO self-pair swaps
// before that window, so any sustained count is an exploit-shaped signal, not
// noise — the tripwire the freeze/divergence guards were blind to because the
// self-pair rows never reach the served `trades` table (they decode to
// (nil,nil); the raw event still lands in soroban_events for forensics).
// Incremented at the decoder drop point (e.g. comet dispatcher_adapter). This
// is a DETECTION metric only — it changes no serving or freeze decision.
var AMMSelfPairSwapTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_amm_self_pair_swap_total",
		Help: "AMM swaps dropped as self-pair (token_in==token_out) — an exploit primitive with no honest purpose. By source.",
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
// (`massive`, the internal/sources/external/forex worker). Stamped only after
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

// ExternalFXRateRejectedTotal — per-source counter of upstream FX rates
// the forex worker refused to persist, by reason (C2-030,
// audit-2026-07-23).
//
// The forex worker's fx_quotes rows are the denominator of every
// fiat-quoted `usd_volume` the X2.5 triangulation derives, so ONE bad
// upstream bar (a decimal shift, a unit-scale change, a zeroed field)
// mis-scales an entire currency's history. persistSnapshot now gates each
// per-ticker rate on a deviation band against the last accepted value for
// that ticker and drops the outlier instead of writing it.
//
// Reasons (bounded — extend the const block, not the call sites):
//   - "deviation"    — moved more than the band vs the last accepted rate
//     for that ticker, and no second fetch has confirmed it yet.
//   - "non_positive" — rate <= 0 (an upstream field that came back empty).
//   - "non_finite"   — NaN or ±Inf (a parse that produced garbage).
//
// Deliberately NOT labelled by ticker: ~150 currencies × sources would be
// pure cardinality for a signal whose actionable question is "is the feed
// producing junk", and the rejected ticker is on the WARN log line.
//
// A single rejection is expected and self-healing — the guard is
// two-strike, so a genuine devaluation confirmed by the next fetch is
// accepted one refresh later. A SUSTAINED non-zero rate is the alertable
// state: it means a ticker is wedged on a stale rate
// (see stellarindex_external_fx_rate_rejections).
var ExternalFXRateRejectedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_external_fx_rate_rejected_total",
		Help: "Upstream FX rates refused by the forex worker's sanity band before reaching fx_quotes, per source and reason (deviation/non_positive/non_finite). Sustained non-zero means a currency is wedged on its last accepted rate.",
	},
	[]string{"source", "reason"},
)

// ExternalFXBaselineHealedTotal — the forex worker re-pointed a ticker's
// sanity-band baseline at the median of an agreeing trailing-7d history
// majority that refuted it (the 2026-08-24 Massive UZS poisoned-bootstrap
// incident). Rare by design; each increment is one wrong baseline
// corrected without operator action. The ticker is in the WARN log line.
var ExternalFXBaselineHealedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_external_fx_baseline_healed_total",
		Help: "FX sanity-band baselines re-pointed at an agreeing history majority that refuted them, per source. Each increment is one poisoned/stale baseline self-corrected.",
	},
	[]string{"source"},
)

// SourceUnrepresentableSymbolsTotal — per-source counter of oracle
// asset slots DROPPED because the published symbol / feed id cannot be
// held even by the record layer's verbatim `raw:` namespace: empty,
// longer than 64 bytes, or carrying a byte outside printable ASCII
// 0x21–0x7E (canonical.NewOracleRawAsset / validateRawSymbol).
//
// Deliberately NOT folded into SourceUnknownSymbolsTotal. That counter
// means "recorded as raw:<symbol>" — the row exists and a later
// allow-list / feed-registry entry promotes it in place. A slot on THIS
// counter is a HOLE: nothing was written, so closing it needs the
// registry entry AND a replay of the affected ledgers. Sharing one
// series would send operators hunting for raw rows that do not exist.
//
// Only an ScString-keyed oracle can reach it in practice: RedStone
// feed_ids are `ScString` (arbitrary bytes, unbounded length), while
// Reflector/Band symbols are `ScSymbol`. Refusal is per-SLOT, not
// per-event (#291): write_prices batches every updated feed into one
// event, so refusing the event would take all ~19 feeds dark — the
// inverse of the oracle capture-totality goal.
//
// Alert consumer: `stellarindex_ingestion_oracle_unrepresentable_symbols`
// (deploy/monitoring/rules/ingestion.yml + the R1 overlay; runbook
// docs/operations/runbooks/oracle-unknown-symbols.md).
var SourceUnrepresentableSymbolsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_source_unrepresentable_symbols_total",
		Help: "Oracle asset slots dropped because the published symbol/feed id cannot be represented even as a raw: asset; each increment is a row the record layer could not write.",
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

// DiscoveryDroppedHitsTotal — count of discovery hits (SEP-41 token
// sightings AND the broader oracle-suggestive event/call sightings
// added per docs/architecture/generic-oracle-sep-onboarding.md
// §3(b) — internal/canonical/discovery.Sniff / SniffOracleEvent /
// SniffOracleCall all share the one sink) that were dropped because
// the async sink buffer was full. Discovery is intentionally
// best-effort, but operators still need a live signal when the
// buffer starts shedding records under write pressure.
var DiscoveryDroppedHitsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_discovery_dropped_hits_total",
		Help: "Discovery hits dropped because the async discovery sink buffer was full.",
	},
)

// DiscoverySkippedHitsTotal — count of discovery hits (same shared
// sink as [DiscoveryDroppedHitsTotal] — SEP-41 + oracle-suggestive
// event/call sightings) whose dedup key had already been enqueued in
// this process and were therefore deduplicated before reaching the
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

// DiscoveryRecordFailuresTotal — count of discovery hits whose
// Recorder.Record write FAILED (Postgres error / timeout), distinct
// from [DiscoveryDroppedHitsTotal] (buffer-full pre-write drop) and
// [DiscoverySkippedHitsTotal] (in-process dedup). Before this counter,
// a Record failure in the async sink was only logged (a Warn line in
// internal/canonical/discovery/sink.go) — so a persistent recorder
// outage silently stopped discovered_assets from growing with no
// metric or alert. The discovered contract will re-appear on a later
// event (best-effort policy), so this counts write ATTEMPTS that failed,
// not permanent loss — but a sustained non-zero rate means discovery
// coverage is degrading and must be visible. Bridged from the sink's
// FailedCount() by the indexer's discovery-metrics goroutine, mirroring
// how DiscoveryDroppedHitsTotal is fed.
var DiscoveryRecordFailuresTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_discovery_record_failures_total",
		Help: "Discovery hits whose Recorder.Record write failed (recorder outage/timeout); logged-only before, now countable + alertable.",
	},
)

// MetricsRegistryPresent — boot-time gauge (0/1) recording whether a
// component that CAN run without a Prometheus Registry actually got one
// wired (audit-2026-07-16 C4-4). The concrete case: ledgerstream's SDK
// BufferedStorageBackend buffer metrics (buffer_fetch_latency_seconds
// etc., registered via the SDK's WithMetrics / ApplyLedgerMetadata) only
// register when Config.Registry != nil. The production builder
// (pipeline.LedgerstreamConfig) leaves it nil ON PURPOSE — the
// archive→live→catch-up path calls Stream repeatedly and the SDK's
// registration is not idempotent, so a second identical registration
// panics — hence those SDK buffer metrics are absent in production. This
// gauge makes that state observable: the indexer sets
// {component="ledgerstream"} to 1 when the config carries a Registry, 0
// when it doesn't. Only set on binaries that actually build the affected
// config; absent series = "component not used here", not an alert.
//
// NOTE: the TieredDataStore's own tier_read_total +
// cold_read_duration_seconds metrics are NO LONGER gated on this. They
// are the package-level [LedgerstreamTierReadTotal] /
// [LedgerstreamColdReadDurationSeconds] below, registered unconditionally
// at boot, so the ledgerstream-tier `both_missing` page is live in
// production regardless of this gauge's value (W5-mon-3 fix). This gauge
// now tracks only the SDK buffer-metric coverage, which stays nil-gated.
var MetricsRegistryPresent = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_metrics_registry_present",
		Help: "1 if the named component was wired with a Prometheus Registry (its SDK buffer metrics are live), 0 if it is running Registry-less (those SDK buffer metrics are absent). Set per component at boot.",
	},
	[]string{"component"},
)

// LedgerstreamTierReadTotal — tiered-datastore reads partitioned by which
// tier served the request (ADR-0027). `outcome` is one of:
//
//	hot          — served from the local galexie-archive MinIO (hot tier).
//	cold         — hot missed; served from the AWS public bucket (cold tier).
//	both_missing — neither tier has the object; the reader is stalled.
//
// A sustained `both_missing` increase is a data-integrity page
// (deploy/monitoring/rules/ledgerstream-tier.yml). This is a PACKAGE-LEVEL
// metric registered unconditionally at boot — NOT gated on
// ledgerstream.Config.Registry — because the production builder leaves
// that registry nil (the SDK's non-idempotent registration panics across
// the archive→live→catch-up Stream calls). Emitted by
// internal/ledgerstream/tiered.go's TieredDataStore. Before W5-mon-3 this
// lived as a per-TieredDataStore CounterVec that only registered when a
// registry was passed, so it was nil in production and the `both_missing`
// page could never fire.
var LedgerstreamTierReadTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_ledgerstream_tier_read_total",
		Help: "Tiered datastore reads partitioned by which tier served the request. hot=local MinIO; cold=AWS public bucket fallback; both_missing=neither tier has the object.",
	},
	[]string{"outcome"},
)

// LedgerstreamColdReadDurationSeconds — latency of cold-tier (AWS public
// bucket) reads. `outcome` is `ok` (cold hit) / `miss` (cold not-found,
// i.e. a both_missing read) / `error` (cold transient failure). The
// paired-histogram sibling of [LedgerstreamTierReadTotal]; same
// package-level, always-registered rationale (W5-mon-3).
var LedgerstreamColdReadDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name: "stellarindex_ledgerstream_cold_read_duration_seconds",
		Help: "Latency of cold-tier (AWS public bucket) reads. Includes hot miss → cold attempt; does not include hot-tier reads.",
		// Wider buckets than the default — cold reads are cross-Atlantic +
		// spread across whole-partition fetches. Range covers 5ms
		// (cache-warm CDN hit) to 30s (transient AWS slowdown).
		Buckets: []float64{0.005, 0.025, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
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

// MonthlyQuotaFailOpenTotal — counter of requests that skipped the
// per-key monthly-quota ceiling because the month-to-date read errored
// (C3-082, audit-2026-07-23).
//
// The exact sibling of [RateLimitFailOpenTotal], and deliberately shaped
// identically. `internal/api/v1/middleware/monthly_quota.go` fails OPEN on a
// backing-store error — the cap is a billing-fairness mechanism, not a
// security boundary, so a Redis blip must not 429 paying customers — but
// that means the metered-plan spend ceiling silently switches itself off,
// and until this counter existed the only trace was a `logger.Debug` line
// (below the API's default log level, so in production: no trace at all).
//
// The exposure is the mirror image of the rate limiter's: this one is
// revenue, not abuse. While it is failing open a metered key can bill past
// its agreed cap, and the overage is unrecoverable after the fact — the
// requests were served.
var MonthlyQuotaFailOpenTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_monthly_quota_fail_open_total",
		Help: "Requests that bypassed the per-key monthly quota ceiling because the month-to-date read errored.",
	},
)

// MonthlyQuotaFailClosedTotal — counter of requests REJECTED with 429
// because the month-to-date counter had been unreadable continuously
// for longer than the fail-open dwell window, so the middleware flipped
// from fail-OPEN to fail-CLOSED (W1-flow-register-4).
//
// The dwell-guarded companion to [MonthlyQuotaFailOpenTotal]: a
// transient counter blip increments the fail-OPEN counter (the request
// is still served), but a SUSTAINED outage past the dwell window
// increments this one instead (the request is denied), so a counter
// outage cannot become an indefinite unmetered billing window for a key
// already at its cap. A non-zero rate here means the usage backend has
// been down long enough that paying customers are now being 429'd —
// alert on it distinctly from the fail-open signal. Pre-seeded at zero
// (unlabelled counter) so "quiet" is distinguishable from "dead".
var MonthlyQuotaFailClosedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_monthly_quota_fail_closed_total",
		Help: "Requests rejected (429) because the month-to-date read stayed unavailable past the fail-open dwell window.",
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

// AssetCharacterRollupSweepsTotal — per-sweep outcome counter for the
// aggregator's asset-volume-character rollup worker
// (internal/aggregate/assetcharacterrollup, wash-and-scam-signals design
// §2), which folds the trailing-window all-asset account-structure roll
// over `trades` (each trade counted on BOTH sides, folded onto canonical
// assets) into the asset_volume_character table so /v1/assets{,/{id}} read
// a keyed-on-PK lookup instead of the ~4s per-request trades roll (measured
// 4.09s on the USDC detail, tripping the 4s per-request timeout → null).
// Labels:
//
//   - `ok`            — sweep completed; rollup rows upserted + pruned.
//   - `refresh_error` — the roll/upsert transaction failed (Postgres
//     unreachable, migration 0149 missing). The rollup keeps its previous
//     rows; volume_character goes stale, not blank.
//
// A sustained `refresh_error` rate means the volume_character overlay stops
// advancing — informational severity (pricing/verification are unaffected;
// the field is analytics-only).
var AssetCharacterRollupSweepsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_asset_character_rollup_sweeps_total",
		Help: "Asset-volume-character rollup worker sweep outcomes (ok|refresh_error).",
	},
	[]string{"outcome"},
)

// AssetCharacterRollupSweepDurationSeconds — latency histogram for one
// asset-volume-character rollup sweep (the all-asset trailing-window trades
// roll + batched upsert + prune), labelled by outcome so operators chart
// `ok` p95/p99 separately from the fail-fast error path.
//
// Buckets span 50 ms → 120 s: this is the heaviest asset rollup (a
// full-window all-asset `trades` scan with unordered account-pair
// aggregation), so watching its p95 here is how an operator learns the roll
// is getting heavier — long before it would surface as a slow endpoint.
var AssetCharacterRollupSweepDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_asset_character_rollup_sweep_duration_seconds",
		Help:    "Asset-volume-character rollup sweep latency, labelled by outcome (ok|refresh_error).",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
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

// AssetsPopularPriceless — the priceless-popular pricing-coverage tripwire
// gauge (task #28 Part B). Set each sweep by the aggregator's
// internal/pricelesscoverage worker to the COUNT of assets that are:
//
//   - above the popularity floor by MARKET-CHARACTER volume (7d priced USD
//     volume > $10k OR 7d trades > 5k, with single-account-pair wash
//     EXCLUDED so a volume-painting scam farm cannot self-select in), AND
//   - priceless (no servable USD/XLM-proxy price), AND
//   - not deliberately withheld (their recent 24h market clears the
//     substance serve floor, so the gate is NOT the reason they're
//     priceless — this is an unexplained coverage gap, not a fail-closed
//     thin market).
//
// > 0 means a genuinely-traded asset has no price and no recorded reason —
// a pricing-coverage gap that should page instead of waiting for an
// operator to notice it while browsing /assets. 0 is the healthy steady
// state (every popular asset is priced or explained).
var AssetsPopularPriceless = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_assets_popular_priceless",
		Help: "Count of market-popular, priceless, non-withheld assets at the most recent coverage-check sweep. > 0 is an unexplained pricing-coverage gap.",
	},
)

// PricelessCoverageCheckRunsTotal — per-sweep outcome counter for the
// priceless-popular tripwire. `ok` = the candidate read + classify pass
// completed; `error` = the catalogue read failed (Postgres unreachable /
// query error), leaving the gauge stale. A sustained `error` rate (or a
// stalled `last_success_unix`) means the tripwire itself is blind — the
// paging-not-browsing guarantee is off until it recovers.
var PricelessCoverageCheckRunsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_priceless_coverage_check_runs_total",
		Help: "Priceless-popular coverage-check sweep outcomes (ok|error).",
	},
	[]string{"outcome"},
)

// PricelessCoverageCheckLastSuccessUnix — wall-clock unix seconds of the
// most recent successful tripwire sweep. `time() - this` powers the
// staleness alert: a wedged worker stops updating the gauge, so a fresh
// coverage gap would go unseen; the staleness guard catches the silent
// worker even when the count gauge itself sits at a stale 0.
var PricelessCoverageCheckLastSuccessUnix = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_priceless_coverage_check_last_success_unix",
		Help: "Unix seconds of the most recent successful priceless-popular coverage-check sweep.",
	},
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

// Operation labels on [LoginCodeLockoutErrorsTotal]. Bounded set of
// three — one per place the durable login-code lockout (C3-032) can
// fail without the sign-in itself failing.
const (
	// LoginCodeLockoutOpStatusCheck — the pre-match lockout read failed.
	// The handler FAILS OPEN here (a locked address is let through to a
	// code comparison), so this is the one that means the control is
	// silently off.
	LoginCodeLockoutOpStatusCheck = "status_check"
	// LoginCodeLockoutOpRegister — a wrong-code attempt was not recorded
	// against the durable per-email counter. The grinder got a free
	// guess.
	LoginCodeLockoutOpRegister = "register"
	// LoginCodeLockoutOpSweep — the retention sweep failed. Rows an
	// unauthenticated caller can create accumulate until it recovers.
	LoginCodeLockoutOpSweep = "sweep"
)

// LoginCodeLockoutRows — current row count of `login_code_lockouts`
// (migration 0122, C3-032), refreshed by each retention sweep.
//
// This table's primary key is ATTACKER-CHOSEN. POST /v1/auth/verify-code
// is unauthenticated and accepts any well-formed address, so one wrong
// guess against a synthetic address inserts a row that the
// clear-on-successful-login path can never remove — nobody can sign in
// as an address that does not exist. `internal/logincodereaper` bounds
// it; this gauge is how an operator sees the bound holding.
//
// Without it, the first signal of a remote table-fill would be the
// volume-level disk-full page — i.e. after the damage, on a metric that
// names the wrong subsystem. A healthy deployment sits in the low tens:
// rows exist only for addresses with recent failures, are deleted on
// any successful sign-in, and are swept once settled.
//
// Gauges register at zero, so no explicit seeding is needed — but the
// value is only meaningful once a sweep has run (the reaper sweeps
// immediately at boot).
var LoginCodeLockoutRows = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_login_code_lockout_rows",
		Help: "Rows in login_code_lockouts. The key is attacker-chosen and the endpoint is unauthenticated, so sustained growth is a remote table-fill, not user behaviour.",
	},
)

// LoginCodeLockoutRowsDeletedTotal — cumulative settled lockout rows
// removed by the retention sweep. Charted as a rate it is the
// production rate of failed-verify addresses; read next to
// [LoginCodeLockoutRows] it distinguishes "the table is small because
// nothing is happening" from "the table is small because the sweep is
// keeping up with a flood".
var LoginCodeLockoutRowsDeletedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_login_code_lockout_rows_deleted_total",
		Help: "Cumulative settled login_code_lockouts rows deleted by the retention sweep.",
	},
)

// LoginCodeLockoutErrorsTotal — failures of the durable login-code
// lockout's own machinery (C3-032), labelled by `op`.
//
// Every one of these paths is deliberately non-fatal to the request:
// the lockout is defence-in-depth over the per-token `maxCodeAttempts`
// cap, and failing a customer's sign-in because a counter table blipped
// would be a worse trade. The consequence is that all three failures are
// SILENT at the HTTP layer — which is precisely the shape of defect this
// audit wave keeps finding (a fail-open control with no counter looks
// identical to a control that is working). `op="status_check"` in
// particular means the lockout is not being enforced at all.
//
// Pre-seeded across the bounded op set so "the control has never failed"
// and "nothing emits this metric" are different scrapes.
var LoginCodeLockoutErrorsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_login_code_lockout_errors_total",
		Help: "Durable login-code lockout machinery failures by op (status_check|register|sweep). status_check non-zero = the lockout is failing open and is not being enforced.",
	},
	[]string{"op"},
)

// Operation labels on [MagicLinkTokenErrorsTotal]. A single op today —
// the retention sweep — declared as a bounded set so a second failure
// mode can join it without changing the metric's shape.
const (
	// MagicLinkTokenOpSweep — the retention sweep failed. Rows an
	// unauthenticated caller can create (POST /v1/auth/login, PRV-2)
	// accumulate until it recovers.
	MagicLinkTokenOpSweep = "sweep"
)

// MagicLinkTokenRows — current row count of `magic_link_tokens`
// (migration 0027), refreshed by each retention sweep.
//
// This table is durable plaintext PII (email + requested_ip) with an
// ATTACKER-CHOSEN key: POST /v1/auth/login is unauthenticated and
// inserts a permanent row for any well-formed address, and a link
// nobody clicks is never consumed. `internal/magiclinkreaper` bounds
// it; this gauge is how an operator sees the bound holding.
//
// Without it, the first signal of a remote table-fill would be the
// volume-level disk-full page — i.e. after the damage. A healthy
// deployment sits low: rows exist only for recent login/verify/invite
// mints and are swept once expired past retention.
//
// Gauges register at zero, so no explicit seeding is needed — but the
// value is only meaningful once a sweep has run (the reaper sweeps
// immediately at boot).
var MagicLinkTokenRows = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_magic_link_token_rows",
		Help: "Rows in magic_link_tokens. The key is attacker-chosen and POST /v1/auth/login is unauthenticated, so sustained growth is a remote table-fill, not user behaviour.",
	},
)

// MagicLinkTokenRowsDeletedTotal — cumulative expired magic-link rows
// removed by the retention sweep. Charted as a rate it is the
// production rate of expired mints; read next to [MagicLinkTokenRows]
// it distinguishes "the table is small because nothing is happening"
// from "the table is small because the sweep is keeping up with a
// flood".
var MagicLinkTokenRowsDeletedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_magic_link_token_rows_deleted_total",
		Help: "Cumulative expired magic_link_tokens rows deleted by the retention sweep.",
	},
)

// MagicLinkTokenErrorsTotal — failures of the magic-link retention
// sweep (PRV-2), labelled by `op`. The sweep is a background janitor
// and a failed pass is silent at the HTTP layer, so — like the
// login-code lockout reaper it mirrors — an ABSENT series and a
// never-failed series must be different scrapes. Pre-seeded across the
// bounded op set.
var MagicLinkTokenErrorsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_magic_link_token_errors_total",
		Help: "Magic-link retention sweep failures by op (sweep). Non-zero = the reaper is not bounding an unauthenticated-writable PII table.",
	},
	[]string{"op"},
)

// Notify template + result label VALUES for [NotifySendsTotal]. Kept as
// constants so the zero-seed loop and the send call sites cannot drift on
// spelling. `template` is the mail's purpose; `result` is delivery outcome.
const (
	// NotifyTemplateMagicLink — the dashboard sign-in magic-link email
	// (internal/api/v1/dashboardauth). A failure here blocks login.
	NotifyTemplateMagicLink = "magic-link"
	// NotifyTemplateSignupVerify — the API-signup email-confirmation link
	// (cmd/stellarindex-api signupVerifyEmailerAdapter). A failure leaves
	// the key usable but never flips email_verified.
	NotifyTemplateSignupVerify = "signup-verify"

	// NotifySendResultSent — Sender.Send returned nil (accepted by Resend).
	NotifySendResultSent = "sent"
	// NotifySendResultFailed — Sender.Send returned an error (validation,
	// provider-rejected, or transient/network). The mail did not go out.
	NotifySendResultFailed = "failed"
)

// NotifySendsTotal counts transactional-email sends per template and result.
// internal/notify (the Resend client) had ZERO prometheus visibility, so a mail
// outage was silent — it surfaced only as users unable to sign in or confirm
// their signup. This counter is incremented at every notify.Sender.Send call
// site: `result=sent` on success, `result=failed` on any returned error. A
// sustained failed ratio drives the notify send-failure alert. Zero-seeded per
// (template, result) so the ratio reads a real 0 before the first email — an
// absent series would make "no mail has ever failed" and "the mailer is dead"
// the same scrape.
var NotifySendsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_notify_sends_total",
		Help: "Transactional-email sends per template and result (sent|failed). A sustained failed ratio means the mail provider (Resend) is failing — magic-link login and signup-verification stop delivering.",
	},
	[]string{"template", "result"},
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

// OracleStreamRowsUnparsedTotal counts oracle_updates rows dropped by
// LatestOracleStreams because their stored asset or quote text would not
// parse as a canonical asset.
//
// The read used to `continue` on a parse failure with no log, metric or
// error, so such a row simply vanished from /v1/oracle/streams and the
// explorer's /oracles page with zero signal (wave-D SI-OC-04).
//
// That matters most exactly when it is most likely: the documented
// remediation for a mislabelled oracle row is an operator-run raw SQL
// UPDATE against that same column, which carries no CHECK constraint. A
// typo there would silently delete the row from the served surface
// rather than erroring — the operator would see the row disappear and
// reasonably conclude the relabel worked.
//
// `field` distinguishes asset from quote so the fix is obvious without
// a query.
var OracleStreamRowsUnparsedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_oracle_stream_rows_unparsed_total",
		Help: "oracle_updates rows dropped from the served stream because their stored canonical asset/quote text would not parse, per source and field.",
	},
	[]string{"source", "field"},
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
		Help: "Trade-insert attempts, labelled by source and whether usd_volume was populated (yes|no). Counts attempts, not unique-row inserts — on-conflict dedupe AND generation-guarded corrective updates are both invisible to this counter.",
	},
	[]string{"source", "usd_volume_populated"},
)

// TradeInsertOutcomeTotal — per-source counter of trade-insert
// outcomes. `new` means a fresh row landed.
//
// ⚠ `duplicate` IS A CONFLATION, and the name now understates it. The
// INV-3 keystone fix replaced the trade upsert's `ON CONFLICT DO NOTHING`
// with a generation-guarded `DO UPDATE`, so the underlying
// `count(*) FILTER (WHERE inserted)` returns 0 for THREE different
// outcomes: a true duplicate, a generation-guarded CORRECTION that
// updated an existing row, and a guard-SKIPPED write (lower generation).
// Only the first is what this label's name suggests.
//
// Two consequences an operator needs to know:
//
//   - The duplicate-flood alert below false-positives during a corrective
//     re-derive. A re-derive legitimately updates rows in place, which
//     scores as `duplicate` with zero `new` — byte-identical to the
//     stuck-cursor signature.
//   - A landing correction is NOT observable here. The whole point of
//     INV-3 is that corrected re-derives take effect, and this counter
//     cannot distinguish "correction applied" from "nothing happened".
//
// Splitting the label into new/updated/skipped is the real fix — the SQL
// already has the `xmax = 0` signal needed to tell them apart — but that
// touches the hot money-path insert and is deliberately not bundled here.
//
// TradeInsertsTotal counts attempts and is silent about dedupe; on
// a healthy live indexer the two counters track 1:1, but a stuck
// cursor or replay loop (live evidence on r1, 2026-05-28: 157
// SDEX insert-attempts/min while the trades hypertable's max(ts)
// is 11 h old) produces a fast-growing `duplicate` rate with zero
// `new`. Pairing the two lets operators alert on a nonzero duplicate
// rate with no new rows — but write that second clause as
// `unless on (source) rate(new[10m]) > 0`, NEVER as
// `and on (source) rate(new[10m]) == 0`: this vector is call-site-
// seeded and `source` is config-dependent (not pre-seeded in
// seedBoundedLabelSeries, per the AggregatorFXSnapFallbackTotal `leg`
// convention), so a source that has landed no new row since process
// start has NO `outcome="new"` child for an `and` join to match and
// the alert goes silent in exactly the post-restart replay flood it
// exists for (#302, 2026-08-29). A duplicate-only stream is the
// signature of a duplicate-flood, BUT see the conflation note above:
// a running corrective re-derive produces the same shape, so correlate
// with whether a re-derive is in flight before treating it as a stuck
// cursor. Cardinality: one source × two outcomes per registered source
// (low-tens of series at maturity).
var TradeInsertOutcomeTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_trade_insert_outcome_total",
		Help: "Trade-insert outcomes per source. outcome=new when a fresh row landed; outcome=duplicate when no row was inserted — which since INV-3 conflates a true duplicate, a generation-guarded corrective UPDATE, and a guard-skipped write. A corrective re-derive therefore looks identical to a cursor replay here.",
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

// Reason labels on [CustomerWebhookFanoutFailuresTotal]. Bounded set
// of three — one per way a producer-side fan-out can lose a customer's
// event before a delivery row exists.
const (
	// FanoutFailureInvalidPayload — the caller handed Publish bytes
	// that are not JSON. Every subscriber loses the event.
	FanoutFailureInvalidPayload = "invalid_payload"
	// FanoutFailureListSubscribers — ListWebhooksSubscribedTo errored,
	// so the subscriber set is unknown and nothing was enqueued.
	FanoutFailureListSubscribers = "list_subscribers"
	// FanoutFailureEnqueue — one subscriber's EnqueueDelivery insert
	// failed. Counted once per LOST delivery.
	FanoutFailureEnqueue = "enqueue"
)

// CustomerWebhookFanoutFailuresTotal — customer events that never
// became a delivery row (C3-023, audit-2026-07-23).
//
// This is the PRODUCER-side counterpart to
// [CustomerWebhookDeliveryAttemptsTotal], which only starts counting
// once a `webhook_deliveries` row exists. A fan-out failure happens
// strictly before that row: the freeze / divergence / incident event
// fired, the customer was subscribed, and no delivery was ever
// enqueued for them. There is nothing to retry and nothing to drain —
// the customer's event is permanently gone.
//
// Pre-fix `Fanout.Publish` had no return value at all, so a fan-out
// that lost every subscriber was indistinguishable from a successful
// one at the call site, and the only trace was a WARN line.
//
// Labels:
//   - event_type: the platform.WebhookEventType that was being
//     published (incident.sev1 | incident.resolved | anomaly.freeze |
//     divergence.firing | price.alert)
//   - reason: invalid_payload | list_subscribers | enqueue
//
// `enqueue` increments once per lost DELIVERY (per subscriber);
// the other two increment once per lost fan-out, where the loss
// covers every subscriber of that event type.
//
// Emitted by the aggregator binary (freeze + divergence hot paths)
// and by `stellarindex-ops emit-incident`; the ops CLI is short-lived
// and unscraped, which is why that call site ALSO returns the error
// to the operator's shell.
//
// Pre-seeded across event_type × reason so a quiet fan-out reads as a
// real zero rather than "no data" — the distinction this whole
// counter exists to make.
var CustomerWebhookFanoutFailuresTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_customer_webhook_fanout_failures_total",
		Help: "Customer events that never became a webhook delivery row, by event type and reason. Non-zero = a subscribed customer permanently lost an event (no retry row exists).",
	},
	[]string{"event_type", "reason"},
)

// CustomerWebhookDeliveryDurationSeconds — latency histogram for
// the outbound HTTP POST inside the customer-webhook delivery
// worker (the OUTBOUND worker is a goroutine, not an HTTP handler,
// so the API middleware's free `http_request_duration_seconds`
// doesn't cover it).
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
// removed from the VWAP input set, labelled by reason and by the
// CONFIGURED target pair. "class" = removed by the ClassExchange-only
// filter; "outlier" = removed by the σ-threshold filter. Operators
// alert on a sudden spike in "class" (a new venue mis-registered) or
// "outlier" (a market in distress flooding the window with anomalies).
//
// `pair` (2026-08-14 outlier_storm: a single-issuer token farm spamming
// SDEX needed ad-hoc SQL to attribute) is the canonical string of the
// configured aggregate pair whose refresh dropped the trade — bounded
// cardinality by construction: only pairs in the orchestrator's
// configured set flow through refreshPairWindow (~12 in production).
// Config-dependent labels are NOT pre-seeded, per the
// AggregatorFXSnapFallbackTotal `leg` convention in
// seedBoundedLabelSeries; the storm/spike alerts sum() across labels,
// so an absent pair series never gates them. Diagnose with
// `topk(5, rate(...{reason="outlier"}[10m]))` by pair.
//
// Semantics caveat (2026-08-28): the orchestrator re-runs the filter
// over the whole trailing window every tick, so a print that stays
// outside the band is counted again on every tick it remains in the
// window, and once per window ([5m,1h,24h]). The rate is therefore
// "band-residents × windows / tick", not "new outliers/s". Since the
// 2026-08-28 redesign outlier_storm no longer gates on this counter
// (it reads AggregatorVenueVWAP; trim-fraction reads
// AggregatorWindowTrades) — but class_drop_spike (reason="class")
// and outlier_trim_rate_legacy (reason="outlier", retires
// 2026-09-04) still do.
var AggregatorDroppedTradesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_dropped_trades_total",
		Help: "Trades removed from the VWAP input set, labelled by reason (class|outlier) and configured target pair.",
	},
	[]string{"reason", "pair"},
)

// AggregatorVenueVWAP — per-source VWAP of the PRE-outlier-filter
// (post-class-filter) trade set for one (pair, window) refresh, on
// the served price scale. Set on every refresh; a source that has
// left the window has its series deleted so a venue that stopped
// trading cannot pin a stale level into the disagreement ratio.
//
// This is the input to `stellarindex_aggregator_outlier_storm`
// (2026-08-28 redesign): `max by (pair) / min by (pair) − 1` over the
// 5m window measures VENUE DISAGREEMENT directly. The previous
// counter-based rule measured how many prints the whole-window MAD
// band trimmed — which, on the same day, fired for hours on
// crypto:XLM/fiat:GBP while every venue agreed within 0.9%, because
// the band trimmed a genuine +2% step (see aggregate.FilterOutliersLocal).
//
// Cardinality: configured pairs × windows × sources that traded in
// the window (≤ ~12 × 3 × 5). Config-dependent, not pre-seeded.
// A float64 gauge is fine here: this is an operator signal, never a
// served value (ADR-0003 applies to the value path only).
var AggregatorVenueVWAP = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_aggregator_venue_vwap",
		Help: "Per-source VWAP of the pre-outlier-filter trade set for one (pair, window) refresh, on the served price scale. Feeds the venue-disagreement outlier_storm alert.",
	},
	[]string{"pair", "window", "source"},
)

// AggregatorWindowTrades — number of trades in one (pair, window)
// refresh after each filter stage: "fetched" (what the store
// returned, post-truncation), "class" (after the ClassExchange-only
// filter), "outlier" (after the outlier filter — the VWAP input).
// A gauge of the CURRENT window, not a counter: the trim fraction
// `1 − outlier/class` is the honest "how much of this window is the
// filter rejecting" signal, whereas the per-tick
// AggregatorDroppedTradesTotal increments re-count the same window
// residents on every 30 s tick, summed across windows.
//
// Feeds `stellarindex_aggregator_outlier_trim_fraction`. Bounded
// cardinality: configured pairs × windows × 3 stages.
var AggregatorWindowTrades = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_aggregator_window_trades",
		Help: "Trades in the current (pair, window) refresh after each filter stage (fetched|class|outlier). outlier/class is the surviving fraction of the VWAP input.",
	},
	[]string{"pair", "window", "stage"},
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

// AggregatorMinUSDVolumeUnvaluableTotal — count of (pair, window)
// refreshes where `aggregate.min_usd_volume` is configured (> 0) but
// the target pair's on-chain quote asset (classic or Soroban) has no
// operator-recognised USD peg, so the manipulation-floor check could
// not be evaluated and the window was DROPPED fail-closed (2026-08-04
// inversion — pre-inversion these windows published unguarded, which
// is the exposure the 2026-08-04 valuation incident closed). Labelled
// by `pair` (bounded — operators configure a small, curated
// aggregate.pairs allow-list; see PriceStalenessSeconds for the same
// cardinality reasoning).
//
// Guard 1 (2026-07-10): before this metric existed, an unvaluable
// on-chain quote pair passed through unguarded SILENTLY — the same
// code path minted no signal either way. A non-zero rate here means
// an operator has a directly-configured Soroban- or classic-quoted
// pair whose quote asset isn't on usd_pegged_classic_assets /
// sac_wrappers — that pair now publishes NOTHING; the fix is adding
// the missing peg, not alerting (no rule wired — see
// docs/reference/metrics/README.md for why this one is
// dashboard-only).
var AggregatorMinUSDVolumeUnvaluableTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_min_usd_volume_unvaluable_total",
		Help: "Windows DROPPED fail-closed because min_usd_volume is set but the target pair's on-chain quote asset has no recognised USD peg (floor unverifiable), labelled by pair.",
	},
	[]string{"pair"},
)

// PriceServeSubstanceWithheldTotal — count of aggregated-price serves
// WITHHELD by the serving-side thin-market substance gate
// (internal/pricingguard.SubstanceGate): the pair has an on-chain leg
// and its trailing market activity (USD volume / distinct 1m buckets /
// wall-clock span) is below the [pricing_guard] serve floor, so no
// "the price of X is P" claim is published for it. Raw surfaces
// (/v1/ohlc, /v1/observations, /v1/history) still serve the pair.
//
// Labelled by `surface` — WHICH serving path withheld: "price_read"
// (the shared /v1/price + batch + assets-enrichment reader), "tip"
// (/v1/price/tip), "oracle" (SEP-40 passthrough), "asset_headline"
// (GlobalAssetView), "price_alert" (customer price-alert evaluator).
// Low-cardinality constants only — NEVER a pair label; the gate is hit
// by arbitrary user-supplied pairs (tens of thousands of assets, see
// the cardinality warning on PriceStalenessSeconds).
//
// A steady non-zero rate is EXPECTED (the long tail of dust pairs is
// large — that is the gate doing its job); what warrants a look is a
// sudden step-change, which usually means either a data outage
// upstream of prices_1m (everything looks thin) or a floor
// misconfiguration. Dashboard-only, no alert rule.
var PriceServeSubstanceWithheldTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_price_serve_substance_withheld_total",
		Help: "Aggregated price serves withheld by the thin-market substance gate, labelled by serving surface.",
	},
	[]string{"surface"},
)

// PriceServeScamWithheldTotal — count of aggregated-price serves withheld
// by the scam-pricing gate because the asset's issuer is flagged
// scam-class (malicious/unsafe/fraud/scam/hack/phishing) in the curated
// account directory. Labelled by serving surface. A non-zero rate here
// with no matching directory change can indicate the gate mis-firing;
// a sudden drop to zero while flagged issuers still trade can indicate
// the gate failing open (see the paired warn log).
var PriceServeScamWithheldTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_price_serve_scam_withheld_total",
		Help: "Aggregated price serves withheld by the scam-pricing gate (directory-flagged issuer), labelled by serving surface.",
	},
	[]string{"surface"},
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

// AnomalyWarnTotal — counter of ActionWarn decisions the aggregator's
// anomaly checker emitted, labelled by asset class, mirroring
// [AnomalyFreezeEngagedTotal].
//
// This exists because ActionWarn was previously computed and then thrown
// away (audit COR-09 / AGT-06): the orchestrator discarded the returned
// Action on the non-freeze path, so a bucket deviating past `warn_pct` —
// enough to be called out, not enough to freeze — left NO trace anywhere.
// The operator's `warn_pct` knob was tunable and completely inert.
//
// Deliberately NOT wired to flags.divergence_warning, which several doc
// comments claimed it fed. That flag is produced by the cross-reference
// divergence service and is meaningful ONLY alongside
// flags.divergence_checked (CS-087: a false warning must not be read as
// "prices agree"). An anomaly warn runs no cross-reference check, so ORing
// it in would publish divergence_warning=true with divergence_checked=false
// — precisely the state CS-087 declares un-interpretable. Surfacing the
// anomaly warn on the wire needs its own flag; that is an API-shape
// decision, and until it is made the signal lives here where an operator
// can alert on it.
var AnomalyWarnTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_warn_total",
		Help: "ActionWarn decisions emitted by the aggregator anomaly checker, labelled by asset class.",
	},
	[]string{"class"},
)

// AnomalyFreezeEscalatedTotal — counter of freezes that exhausted
// ADR-0019's extension ladder (4 × 30 min after the initial hold =
// 2 hours) and escalated to operator review. P1 by construction:
// the freeze does NOT auto-unfreeze once escalated ("freeze stays
// active until manual unfreeze"), so every increment is a pair whose
// /v1/price is pinned to a last-known-good value until a human acts.
//
// One increment per escalation transition, not per frozen tick — so
// `increase(...[15m]) > 0` reads as "a new pair escalated", and the
// steady state of an un-actioned escalation is a flat line rather
// than a climbing one. Pair identity is in the WARN log line and in
// the `freeze:<asset>:<quote>` marker's `state` object; the metric
// stays unlabelled so an escalation storm cannot blow up cardinality
// on the aggregator's hot path.
//
// Unlabelled also means the series exists at zero from process start
// (F-0033): a counter with no label combinations registers
// immediately, so the alert's increase() reads a real 0 rather than
// "no data" before the first escalation.
var AnomalyFreezeEscalatedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_freeze_escalated_total",
		Help: "Freezes that exhausted the ADR-0019 extension ladder (2h) and escalated to operator review. Stays frozen until manual unfreeze.",
	},
)

// AnomalyFreezeExtensionsTotal — counter of ADR-0019 hold extensions
// granted (each +30 min after a hold expired without the pair earning
// its auto-unfreeze). Rate is the operator's early-warning signal
// that freezes are climbing the ladder toward escalation; a sustained
// non-zero rate with no escalations means pairs are recovering in the
// 30–120 minute band, which is normal for a real market event and
// abnormal for a calibration that is firing on noise.
var AnomalyFreezeExtensionsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_freeze_extensions_total",
		Help: "ADR-0019 freeze-hold extensions granted (+30 min each, max 4 before escalation).",
	},
)

// AnomalyFreezeReleasedTotal — counter of freezes that ended,
// labelled by how: `auto` (ADR-0019's auto-unfreeze condition —
// confidence > 0.30 AND z < 3.0 for two consecutive buckets — held at
// hold expiry) or `operator` (the marker was cleared out of band,
// which is the ADR's "operator override always available").
//
// Pairs with AnomalyFreezeEngagedTotal: engaged increments on every
// frozen tick, this one only on the ending transition, so the two are
// NOT expected to balance. What an operator watches here is the
// `operator` label — a rising manual-unfreeze rate means the
// calibration is producing freezes humans keep having to undo.
var AnomalyFreezeReleasedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_freeze_released_total",
		Help: "Freezes ended, by mode. Mode ∈ {auto, operator}.",
	},
	[]string{"mode"},
)

// AnomalyFreezeActive — gauge of (pair, window) freezes the
// aggregator is currently holding, set at the end of every tick.
//
// A gauge, unlike the engaged counter, distinguishes "one pair frozen
// for an hour" from "sixty pairs frozen for one tick each" — the
// counter reads identically for both, which is what made the
// pre-lifecycle `anomaly_freeze_sustained` alert so hard to triage
// (see the anomaly.yml header's "Why a counter (not a gauge)"
// paragraph: the answer was "the orchestrator doesn't track per-pair
// is-this-frozen-now state", and now it does).
//
// Unlabelled by pair on purpose: len(Pairs) × len(Windows) is
// operator-configured and unbounded in principle. Per-pair identity
// lives in the marker JSON.
var AnomalyFreezeActive = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_anomaly_freeze_active",
		Help: "(pair, window) freezes currently held by the aggregator's ADR-0019 lifecycle.",
	},
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

// AnomalyFreezeLadderRehydratedTotal — counter of freeze lifecycles
// restored from the DURABLE ladder (migration 0119) because the Redis
// marker was gone but `freeze_events` still held an open, unlapsed row.
//
// Each increment is one freeze — extension count, escalation flag and all —
// that would have silently RELEASED before 0119: the orchestrator reads a
// missing marker under a live freeze as the ADR-0019 operator override, so
// a Redis flush used to unfreeze every held pair, including ones that had
// climbed the whole 2-hour ladder to escalated ("stays active until manual
// unfreeze") and had already paged a human.
//
// Deliberately not alerted on its own: the healthy steady state is zero,
// and a non-zero reading means the safety net WORKED. What it gives an
// operator is the ability to correlate — a burst here immediately after a
// Redis restart is the expected shape, whereas a slow trickle with Redis
// healthy means markers are being evicted (maxmemory policy) or expiring
// early, which is a real configuration fault worth chasing.
var AnomalyFreezeLadderRehydratedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_freeze_ladder_rehydrated_total",
		Help: "Freeze lifecycles restored from the durable freeze_events ladder after the Redis marker went missing.",
	},
)

// AnomalyFreezeLadderWriteFailuresTotal — counter of durable ADR-0019
// ladder writes (migration 0119) that did not land, by call site
// (`mark_hold` | `clear`).
//
// The failure was always possible; the SILENCE is what this closes. Neither
// freeze.Writer nor timescale.FreezeEventSink holds a logger, so a
// persistently failing ladder write produced no signal on any surface: every
// freeze looked healthy right up until a Redis flush needed the ladder that
// had never been written, at which point the escalated freeze released
// exactly as it did before 0119.
//
// The shape that makes this concrete is a partially-failed deploy. The
// pipeline applies migrations BEFORE swapping the binary, so a new binary
// running against a schema where 0119 did not apply sees EVERY ladder write
// match zero rows — uniformly absent durable state, zero complaints. The
// sink now reports that as ErrNotFound and it is counted here.
//
// Any sustained non-zero value means the durable ladder is not being
// maintained and the Redis-flush protection is inert. `mark_hold` failing is
// the dangerous direction (ladders never recorded); `clear` failing only
// widens the pre-existing recovery-worker window.
//
// Both label values are pre-seeded — a metric that only appears once it
// breaks is indistinguishable from a dead one, which is the exact class of
// gap this counter exists to close.
var AnomalyFreezeLadderWriteFailuresTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_freeze_ladder_write_failures_total",
		Help: "Durable freeze-ladder writes that did not land, by call site (mark_hold|clear). Sustained non-zero = the Redis-flush protection is inert.",
	},
	[]string{"op"},
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
		Help: "Aggregator triangulation outcomes per tick × chain × window (graph-router priced). Outcome ∈ {ok, missing_leg, parse_error, redis_error, frozen_leg, low_confidence}. low_confidence = no route cleared min_route_confidence, so the composite was flagged but NOT published over the direct price.",
	},
	[]string{"outcome"},
)

// AggregatorCompositeCorroboration — per (pair, window) verdict of the
// CURRENT-BUCKET composite-reference corroboration for structurally
// single-venue targets (2026-08-29, orchestrator/composite_reference.go).
// One series per verdict, exactly one of them 1 after each evaluated
// bucket: `corroborated` (the deep-market composite agrees with the
// direct print within tolerance — a phase-2 fire on this bucket is
// suppressed, corroboration_basis=composite), `refuted` (composite
// disagrees — venue-specific, freeze as before) or `unavailable` (a leg
// was too thin / not refreshed / FX stale or not FX-class — freeze as
// before). Cardinality: the allow-list × windows × 3.
var AggregatorCompositeCorroboration = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_aggregator_composite_corroboration",
		Help: "Current-bucket composite-reference verdict per (pair, window): 1 on the active verdict ∈ {corroborated, refuted, unavailable}, 0 on the others. Only structurally single-venue allow-listed targets are evaluated.",
	},
	[]string{"pair", "window", "verdict"},
)

// AggregatorCompositeReferenceLegSources — distinct venue / provider
// count behind each leg of the composite reference on the last
// evaluated bucket (label `leg` = canonical leg pair, e.g.
// "crypto:XLM/fiat:USD"). Shows how STRONG a corroboration was: the
// crypto/USD leg must reach composite_reference.min_leg_sources for the
// composite to count at all, and the FX leg reads 1 (one provider).
var AggregatorCompositeReferenceLegSources = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_aggregator_composite_reference_leg_sources",
		Help: "Distinct sources behind each composite-reference leg on the last evaluated bucket, per (pair, window, leg).",
	},
	[]string{"pair", "window", "leg"},
)

// AggregatorCompositeReferenceLegDispersionBps — max |venue VWAP − leg
// VWAP| / leg VWAP in basis points across the venues on a priced
// composite-reference leg, last evaluated bucket. Above
// composite_reference.leg_dispersion_bps the leg cannot corroborate
// (`composite_unavailable: leg_dispersion=…`): two venues only count
// as two when they agree.
var AggregatorCompositeReferenceLegDispersionBps = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_aggregator_composite_reference_leg_dispersion_bps",
		Help: "Venue dispersion (max |venue VWAP - leg VWAP| / leg VWAP, bps) of each priced composite-reference leg on the last evaluated bucket, per (pair, window, leg).",
	},
	[]string{"pair", "window", "leg"},
)

// AggregatorCompositeFreezeSuppressedTotal — counter of phase-2 freeze
// fires (the 3-signal AND held) that were NOT engaged because the
// current-bucket composite reference corroborated the move. Every
// increment is a bucket that would have frozen before 2026-08-29; read
// it next to stellarindex_anomaly_freeze_engaged_total when judging
// whether the tolerance is too loose.
var AggregatorCompositeFreezeSuppressedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_composite_freeze_suppressed_total",
		Help: "Phase-2 freeze fires suppressed because the current-bucket composite reference corroborated the move (corroboration_basis=composite).",
	},
)

// AggregatorFXSnapFallbackTotal — counter of triangulation legs that
// fell back from the X2.5 forex-snap rule to the cached-VWAP path
// because FXQuoteAtOrBefore returned no row at-or-before the bucket
// end. Steady state should be near-zero once FX ingestion is warm.
// Sustained > 50% of triangulations indicates an FX-source health
// issue (exchangeratesapi) — see the matching alert
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

// AdminAuditWriteFailuresTotal — counter of privileged mutations (and,
// since C3-056, privileged PII READS) that COMPLETED but whose durable
// audit row did not land (C3-067, audit-2026-07-23).
//
// Every one of these call sites appends to the audit log best-effort and
// logs a bare `logger.Warn("… audit append failed (best-effort)")`. The
// mutation itself is already committed by then, so the choice is correct —
// refusing the write after the fact would be worse — but it means a
// money/security change to a customer's account can happen with NO durable
// record, and until this counter existed the only trace was one WARN line
// nobody greps for.
//
// This is the accountability counterpart to the mutation succeeding: a
// non-zero reading means the admin audit trail is incomplete and the gap
// has to be reconstructed from application logs before the retention window
// closes on them. Any sustained non-zero value is alertable.
//
// Labels:
//   - surface: which privileged action lost its audit row
//     (account_override|key_mint|key_revoke|status_notice|
//     staff_customer_lookup)
//
// `staff_customer_lookup` is the one READ in the set: the staff
// customer look-up returns another customer's billing email plus every
// user's email and last-login, so the audit row is the only record that
// a staff member saw it (C3-056).
//
// Bounded, well-known label set — pre-seeded so the alert's increase()
// reads a real zero rather than "no data" before the first failure.
var AdminAuditWriteFailuresTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_admin_audit_write_failures_total",
		Help: "Privileged mutations that succeeded but whose durable audit row failed to append, by surface. Non-zero = the admin audit trail has holes.",
	},
	[]string{"surface"},
)

// AdminKeyBudgetClampsTotal — counter of API credentials whose per-minute
// budget was lowered to their account's tier ceiling by
// `Server.clampKeyBudgetsToTier` (the admin account-override path
// funnels through it).
//
// A clamp silently reduces throughput a customer may still believe they
// have: their existing key keeps working but starts 429-ing sooner, and the
// only prior trace was an INFO line per account. Downgrades are legitimate
// and expected — this is deliberately NOT alerted — but the rate is what
// tells an operator whether a support ticket ("my key started rate-limiting")
// has a billing explanation, and it is what makes an accidental mass-clamp
// (a bad tier map, a mis-sequenced webhook) visible at all.
//
// Labels:
//   - outcome: `lowered` — the credential's budget was written down;
//     `failed`  — the downgrade errored and the key KEPT its old, higher
//     budget (paid throughput still live past the downgrade).
//
// Counts CREDENTIALS, not clamp calls: one account downgrade lowering four
// keys adds 4. Pre-seeded on both outcomes.
var AdminKeyBudgetClampsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_admin_key_budget_clamps_total",
		Help: "API credentials whose per-minute budget was clamped to their account tier ceiling, by outcome (lowered|failed).",
	},
	[]string{"outcome"},
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

// PriceServeDeclinedNonstandardDecimalsTotal — HISTORICAL (permanently
// zero since 2026-07-10). This was the READ-TIME enforcement half of the
// dex-nonstandard-decimals guard: 2026-07-09 → 2026-07-10, /v1/price and
// /v1/ohlc?interval= declined (422) any pair with a confirmed
// non-7-decimals leg, and this counter fired once per declined request.
// The decline guard was REMOVED when decimals normalization reached the
// last CAGG-reading paths (aggregate.AdjustPrice now corrects the served
// value instead of declining — see the runbook's Root cause analysis), so
// nothing increments this counter anymore. Retained (registered, zero)
// one release so dashboards/queries referencing it don't break; remove
// alongside the next metrics cleanup.
//
// Label: `asset` — the flagged leg's C-strkey contract id.
var PriceServeDeclinedNonstandardDecimalsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_price_serve_declined_nonstandard_decimals_total",
		Help: "HISTORICAL, permanently zero since 2026-07-10: price-surface requests declined for a confirmed non-7-decimal pair leg. The decline guard was replaced by read-time decimals normalization (runbook dex-nonstandard-decimals.md); retained one release for dashboard continuity.",
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

// DEXTVLRefreshTotal — per-outcome counter for the API binary's DEX
// TVL snapshot refresher (internal/api/v1.DEXTVLCache), which
// recomputes per-protocol TVL (soroswap / aquarius / phoenix / comet)
// every DEXTVLRefreshInterval. Labels:
//
//   - `ok`    — every wired protocol computed and swapped in.
//   - `error` — at least one protocol's read failed. NOT all-or-
//     nothing: protocols that computed still swapped in, and failed
//     ones keep their previous entry — so a single `error` tick is a
//     degraded refresh, not a blank page.
//
// Operators alert on a sustained `error` rate with no interleaved
// `ok`: that means /v1/dexes TVL (and the per-protocol pages) are
// serving an ever-older carried-forward snapshot while looking
// healthy. An isolated `error` during a lake merge or served-tier
// restart is expected and self-heals on the next tick.
var DEXTVLRefreshTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_dex_tvl_refresh_total",
		Help: "DEX TVL snapshot-cache refresh outcomes (ok|error). `error` means >=1 protocol carried its previous entry forward.",
	},
	[]string{"outcome"},
)

// DEXTVLRefreshDurationSeconds — latency histogram for one full DEX
// TVL refresh, labelled by outcome (matches the counter). A refresh
// is one lake reserve lookup per protocol + a bounded set of
// prices_1m point reads; buckets span 50 ms → 180 s (the worker's
// per-refresh timeout is 3 min, so the top bucket is the hard stop,
// not headroom). Chart `ok` p95 — a creeping ok-latency here is the
// early signal that a reserve read lost its bound (the 40×
// read-amplification class) before it becomes an `error`.
var DEXTVLRefreshDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_dex_tvl_refresh_duration_seconds",
		Help:    "Per-refresh latency of the DEX TVL snapshot cache, labelled by outcome (ok|error).",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 180},
	},
	[]string{"outcome"},
)

// SDEXOrderBookMaintainTotal — per-outcome counter for the in-process
// SDEX live order book behind /v1/sdex/orderbook. Three operation
// classes share the label space because they share a failure surface
// (the lake) but have wildly different costs:
//
//   - `load_ok` / `load_error`       — the initial (or retried)
//     full-slice FINAL load. Runs once per process start; minutes of
//     streaming IO.
//   - `advance_ok` / `advance_error` — the 60s incremental
//     partition-pruned change apply.
//   - `verify_ok` / `verify_error`   — the per-tick quarantine drain:
//     batched (ledger, key) removal probes that graduate version-tie
//     suspect offers into the served book or discard them as
//     proven-dead zombies. Unobserved when the quarantine is empty.
//
// Sustained `advance_error` means the served book is drifting from
// the live ledger while the endpoint keeps answering (it serves the
// last applied state, honestly timestamped). Repeated `load_error`
// means the endpoint is stuck on its 503 warming problem — that is
// the louder, user-visible failure. Repeated `verify_error` means the
// served book stays thinner than the real chain state (quarantined
// offers can't graduate).
var SDEXOrderBookMaintainTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_sdex_orderbook_maintain_total",
		Help: "SDEX live order-book maintenance outcomes (load_ok|load_error|advance_ok|advance_error|verify_ok|verify_error).",
	},
	[]string{"outcome"},
)

// SDEXOrderBookMaintainDurationSeconds — latency histogram for order-
// book maintenance, labelled like the counter. Buckets span 50 ms →
// 30 min: `advance_*` lives in the bottom buckets (a pruned
// incremental read), `load_*` in the top (the worker caps the initial
// load at 30 min). The interesting charts are advance_ok p95 (drift
// risk as offer churn grows) and the raw load_ok observation — the
// wall-time of the one big scan, which the launch plan watches as its
// own acceptance item.
var SDEXOrderBookMaintainDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_sdex_orderbook_maintain_duration_seconds",
		Help:    "SDEX order-book maintenance latency, labelled by outcome (load_ok|load_error|advance_ok|advance_error|verify_ok|verify_error).",
		Buckets: []float64{0.05, 0.25, 1, 5, 15, 60, 300, 900, 1800},
	},
	[]string{"outcome"},
)

// SDEXOrderBookCrossedPairs — the order book's data-quality invariant
// tripwire. Stellar's DEX executes crossing offers at submission, so a
// RESTING classic book can never have best bid >= best ask; a crossed
// pair in the SERVED in-process book means phantom offers (the
// 2026-07-31 zombie class: version-tie survivors of intra-less
// backfill rows served 4.7-year-dead XLM/USDC bids at 0.4327 against
// a 0.1722 ask). The book maintainer quarantines + lake-verifies the
// suspect class, so this should sit at 0; sustained non-zero means a
// zombie whose removal the lake never ingested at all (verification
// cannot disprove it) — a coverage gap to chase, not a serving bug.
var SDEXOrderBookCrossedPairs = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_sdex_orderbook_crossed_pairs",
		Help: "Asset pairs whose served SDEX order book is crossed (best bid >= best ask); any sustained non-zero value is phantom-offer data corruption.",
	},
)

// SDEXOrderBookPendingOffers — offers loaded from the lake's
// current-state projection but quarantined from the served book until
// the change-stream removal probe proves them live (version-tie
// suspect class, intra_ledger_seq == 0). Drains at
// SDEXOrderBookVerifyBatch per advance tick after every process
// start; a value stuck high with verify_error in the maintain
// counters means the book is serving thinner-than-real depth because
// verification can't reach the lake.
var SDEXOrderBookPendingOffers = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_sdex_orderbook_pending_offers",
		Help: "SDEX offers quarantined from the served book awaiting lake removal-verification.",
	},
)

// SDEXOrderBookUndecodableOffersTotal — offer-entry rows the order-book
// reader could not decode (audit 2026-07-31). A non-removed change row
// whose entry_xdr fails to decode is SKIPPED, which silently FREEZES the
// offer key's previously-applied state in the served book (the price/
// amount update it carried is lost until the next decodable change for
// that key). Should sit at 0 — offer entries are core-emitted XDR;
// sustained increments mean a lake ingestion/schema problem upstream of
// the book, and the served depth is quietly stale for the affected keys.
var SDEXOrderBookUndecodableOffersTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_sdex_orderbook_undecodable_offers_total",
		Help: "Offer-entry rows skipped by the SDEX order-book reader because their entry XDR failed to decode; each skip freezes that offer key's previously-applied state.",
	},
)

// ExplorerSWRRefreshTotal — per-cache, per-outcome counter for the
// explorer's detached stale-while-revalidate refreshers (the 2026-07-29
// accounts-routes fix: request handlers serve the previous snapshot
// with flags.stale while a single-flight background goroutine
// recomputes). `cache` is a bounded, code-enumerated set:
//
//   - `accounts_wealth` — /v1/accounts wealth ranking
//     (clickhouse.ExplorerReader wealth cache).
//   - `asset_holders`   — /v1/assets/{id}/holders board.
//   - `contracts_dir`   — /v1/contracts directory.
//   - `op_type_stats`   — operation-type stats strip.
//   - `ttl_liveness`    — the /v1/pools/reserves archived-pair
//     verdict snapshot (clickhouse ttlLivenessCache).
//   - `contract_detail` — the shared per-contract detail cache
//     (recent events / interactions / code-history, route-sweep
//     2026-07-30).
//   - `network_throughput` — the /v1/network/throughput daily series
//     (§2.6b, 2026-08-13).
//   - `protocol_bespoke` — the last-good cache under the
//     /v1/protocols/{name} bespoke analytics block (§2.6b,
//     2026-08-13). Served-tier (Postgres), not lake, but the refresh
//     contract is identical, so it shares this pair rather than
//     minting a fourth near-duplicate metric.
//
// The SWR design makes refresh failures INVISIBLE at the API surface
// by construction (stale-but-real keeps serving) — this counter is
// the only place a persistently-dying refresher shows up before the
// data is hours old. A sustained `error` rate on any one cache is a
// ticket; bursts during lake merges self-heal.
var ExplorerSWRRefreshTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_explorer_swr_refresh_total",
		Help: "Explorer stale-while-revalidate detached refresh outcomes per cache (accounts_wealth|asset_holders|contract_detail|contracts_dir|network_throughput|op_type_stats|protocol_bespoke|ttl_liveness × ok|error).",
	},
	[]string{"cache", "outcome"},
)

// ExplorerSWRRefreshDurationSeconds — latency histogram for one
// detached SWR refresh, labelled like the counter. Buckets span
// 50 ms → 300 s (the widest per-cache refresh timeout). These
// refreshes are exactly the reads that used to time out inline at
// the request deadline — their `ok` p95 per cache is the direct
// measure of how much headroom the detach bought, and a p95 climbing
// toward its cache's refresh timeout predicts the stale-age growing
// user-visible.
var ExplorerSWRRefreshDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_explorer_swr_refresh_duration_seconds",
		Help:    "Explorer stale-while-revalidate detached refresh latency, labelled by cache and outcome.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
	},
	[]string{"cache", "outcome"},
)

// ProtocolDetailRefreshTotal — per-outcome counter for detached
// /v1/protocols/{name} detail rebuilds (internal/api/v1's
// protoDetail cache): both the boot/steady-state prewarm sweep
// (Server.PrewarmProtocolDetails — every protocol × ?days= window) and
// request-kicked stale revalidations share the single-flight and are
// counted here. Labels:
//
//   - `ok`       — the view built fully: lake analytics + bespoke both
//     healthy (analytics.status="ok" on the wire).
//   - `stale`    — the view built COMPLETE, but its bespoke block came
//     from the last-good cache past bespokeStaleAfter
//     (analytics.status="stale" on the wire). Every panel is present;
//     the bespoke numbers are older than the sweep cadence, which means
//     that battery has been failing or starved for a while.
//   - `degraded` — the build completed but at least one analytics
//     component failed/was skipped (the served view carries
//     analytics.status="unavailable"; the page still renders its
//     registry/roster halves).
//   - `timeout`  — the build outran its detached budget. A previously
//     built entry is KEPT (old-but-real beats blank); only a
//     stone-cold key caches the partial view.
//
// Operators: a sustained `degraded`/`timeout` rate means protocol pages
// are serving without their analytics suites — exactly the 2026-07-31
// replay-load failure this worker exists to prevent. Bursts during lake
// merges / replays self-heal on the next sweep.
var ProtocolDetailRefreshTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_protocol_detail_refresh_total",
		Help: "Detached protocol-detail cache rebuild outcomes (ok|stale|degraded|timeout), covering the prewarm sweep and request-kicked stale revalidations.",
	},
	[]string{"outcome"},
)

// ProtocolDetailRefreshDurationSeconds — latency histogram for one
// detached protocol-detail rebuild, labelled like the counter. One
// rebuild is the roster/verdict joins + three parallel lake reads + the
// category's bespoke query battery (measured 2026-07-31 on r1 UNDER
// replay load: soroswap 90d bespoke ~1.9s, cctp ~0.4s). Buckets span
// 50 ms → 90 s — the top bucket is the rebuild's hard budget
// (protocolDetailRefreshTimeout), not headroom. Chart `ok` p95: a creep
// here is the early warning that a bespoke query lost its rollup (the
// raw-trades-scan class) before builds start timing out.
var ProtocolDetailRefreshDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_protocol_detail_refresh_duration_seconds",
		Help:    "Detached protocol-detail rebuild latency, labelled by outcome (ok|stale|degraded|timeout).",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 90},
	},
	[]string{"outcome"},
)

// ObserveExplorerSWRRefresh records one detached stale-while-revalidate
// refresh attempt against the paired [ExplorerSWRRefreshTotal] /
// [ExplorerSWRRefreshDurationSeconds] metrics. Shared helper because
// the five refreshers live across three packages (api/v1/explorer,
// storage/clickhouse) and the outcome-derivation must stay identical.
func ObserveExplorerSWRRefresh(cache string, start time.Time, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	ExplorerSWRRefreshTotal.WithLabelValues(cache, outcome).Inc()
	ExplorerSWRRefreshDurationSeconds.WithLabelValues(cache, outcome).Observe(time.Since(start).Seconds())
}

// Alert idea: `rate(stellarindex_api_sparkline7d_rows_total{result=
// "empty"}[15m]) / rate(stellarindex_api_sparkline7d_rows_total[15m])
// > 0.5` sustained 30 min means half the priced rows on the directory
// render an empty chart — a lookup-key or pricing-pipeline regression,
// not a quiet market.
var APISparkline7dRowsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_api_sparkline7d_rows_total",
		Help: "Priced listing rows for which a 7d sparkline was requested, by result (served|empty).",
	},
	[]string{"result"},
)
