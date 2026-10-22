package obs

import (
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds every metric the binary emits, served at /metrics via [Handler]; non-default so tests get isolated registries.
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
		SourceNonDirectionalSwapsTotal,
		AMMSelfPairSwapTotal,
		AMMNonPositiveSwapTotal,
		ExternalPollerPollsTotal,
		ExternalPollerLastSuccessUnix,
		ExternalFXLastQuoteUnix,
		ExternalFXRateRejectedTotal,
		ExternalFXBaselineHealedTotal,
		FXFixingsLastRefreshUnix,
		FXFixingsNewestBarEndUnix,
		FXFixingsQuoteDisagrees,
		FXFixingsWriteTxSeconds,
		FXFixingsWriteErrorsTotal,
		FXFixingsFetchErrorsTotal,
		FXFixingsBarsRefusedTotal,
		ExternalDustDroppedTotal,
		ExternalBadTimestampDroppedTotal,
		ExternalPollerRefusedEntriesTotal,
		CEXStreamDisconnectTotal, CEXStreamLastTradeUnix,
		DiscoveryDroppedHitsTotal,
		DiscoverySkippedHitsTotal,
		DiscoveryRecordFailuresTotal,
		MetricsRegistryPresent,
		SourceInsertErrorsTotal,
		TradesZeroLegAdmittedTotal,
		RateLimitFailOpenTotal,
		MonthlyQuotaFailOpenTotal,
		MonthlyQuotaFailClosedTotal,
		AdminAuditWriteFailuresTotal,
		AdminKeyBudgetClampsTotal,
		MintScopeClampRefusedTotal,
		CursorLastLedger,
		DivergenceRefreshTotal,
		DivergenceRefresherWired,
		TradeInsertsTotal,
		TradeInsertOutcomeTotal,
		DexTradeUnitRatioTotal,
		TradeInsertRetriesTotal,
		TradeInsertBufferDepth,
		StreamPublishTotal,
		StreamPublishStallTotal,
	)
	registerPricingMetrics()
	registerProjectorMetrics()
	registerAPIServingMetrics()
	registerFreezeLifecycleMetrics()
	registerAppMetricsTail()
}

// registerPricingMetrics registers price-serving, aggregator and supply
// cross-check metrics. Peeled off [registerAppMetrics] for the same funlen
// reason [registerProjectorMetrics] was: the set only grows.
func registerPricingMetrics() {
	Registry.MustRegister(
		PriceStalenessSeconds,
		OracleLastUpdateUnix,
		OracleStreamRowsUnparsedTotal,
		OracleResolutionSeconds,
		OracleStalenessBudgetSeconds,

		AggregatorTicksTotal,
		AggregatorVWAPWritesTotal,
		AggregatorVWAPCacheWriteErrorsTotal,
		AggregatorContributionWriteErrorsTotal,
		AggregatorEmptyWindowsTotal,
		AggregatorWindowTruncatedTotal,
		AggregatorStreamPublishTotal,
		AnomalyWarnTotal,
		CustomerWebhookDeliveryAttemptsTotal,
		CustomerWebhookFanoutFailuresTotal,
		AggregatorDroppedTradesTotal,
		AggregatorVenueVWAP, AggregatorWindowTrades, AggregatorWindowBaseVolume,
		AggregatorDroppedWindowsTotal,
		AggregatorMinUSDVolumeUnvaluableTotal,
		PriceServeSubstanceWithheldTotal,
		PriceServeSubstanceUnmeasuredTotal,
		PriceServeThinAdmittedTotal,
		PriceServeScamWithheldTotal,
		PricingGuardTrailingFetchFailedTotal,

		SupplyCrossCheckDivergenceStroops,
		SupplyCrossCheckTotal,
		SupplyDivergenceRatio,
		SupplyDivergenceTotal,

		AggregatorTriangulationsTotal,
		AggregatorFXSnapFallbackTotal,
		AggregatorBaselineRefreshTotal,
		AggregatorSupplyLakeClampLedgers,
		AggregatorSupplyRefreshTotal,
		SupplyWriteBandBreachTotal,
		SEP41SupplyRollupAdvancesTotal,
		AggregatorConfidenceComputeTotal,
		AggregatorBaselineAgeSeconds,
		AggregatorBootstrapCapped,
		AggregatorBaselineDensityDays,

		ChLiveSinkLedgersTotal, ChLiveSinkReadUndercountTotal,

		MarketsSkippedRowsTotal,
	)
}

// registerProjectorMetrics registers the ADR-0032 projector family: lag, cycle outcomes, decoded
// events, cycle latency, and the wedge and replay-window flags. Split out of [registerAppMetrics]
// to stay under the funlen ceiling.
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

// registerAPIServingMetrics registers the API serving-path family: the read-path caches, the
// per-row sparkline result counter, the stream subscription counter, and the CORS decision
// counter. Split out of [registerAppMetrics] to stay under the funlen ceiling.
func registerAPIServingMetrics() {
	Registry.MustRegister(
		PricingGuardDegradedTotal,
		APICacheOpsTotal,
		APICoverageFloorProbesTotal,
		APILCMHomeDomainFallbackTotal,
		APISparkline7dRowsTotal,
		APIStreamSubscribeTotal,
		APICORSDecisionsTotal,
		APITipProducers,
		APITipProducersRefusedTotal,
		APIStreamSubscriberDropsTotal,
		APISSEStreamsActive,
		APISSEStreamsRejectedTotal,
		APIStreamHubTopics,
		APIStreamHubTopicsReapedTotal,
		AfterResponseTasksDroppedTotal,
	)
	for _, reason := range []string{"caller_quota", "global_ceiling"} {
		APITipProducersRefusedTotal.WithLabelValues(reason)
	}
	for _, reason := range []string{"global_cap", "per_ip_cap", "topic_cap"} {
		APISSEStreamsRejectedTotal.WithLabelValues(reason)
	}
}

// registerFreezeLifecycleMetrics registers the ADR-0019 freeze-lifecycle family (fire, extend,
// escalate, release, the live-freeze gauge, the recovery worker's counters). Split out of
// [registerAppMetrics] to stay under the funlen ceiling.
func registerFreezeLifecycleMetrics() {
	Registry.MustRegister(
		AnomalyFreezeEngagedTotal,
		AnomalyFreezeEscalatedTotal,
		AnomalyFreezeExtensionsTotal,
		AnomalyFreezeHeldUnscoredTotal,
		AnomalyFreezeReleasedTotal,
		AnomalyFreezeRefiredAfterOverrideTotal,
		AnomalyFreezeActive,
		AnomalyFreezeRecoveredTotal,
		AnomalyFreezeLadderRehydratedTotal,
		AnomalyFreezeLadderWriteFailuresTotal,
		APIFreezeLookupFailuresTotal,
		AnomalyFreezeRecoverySweepsTotal,

		// Composite-reference corroboration metrics for the phase-2 verdict;
		// they are freeze-decision metrics, so they live here.
		AggregatorCompositeCorroboration,
		AggregatorCompositeReferenceLegSources,
		AggregatorCompositeReferenceLegDispersionBps,
		AggregatorCompositeFreezeSuppressedTotal,
		AggregatorRouteCorroborationCount,
	)
}

// registerAppMetricsTail registers the rest of the app metric set, split out of registerAppMetrics only to keep each function
// under the funlen ceiling.
func registerAppMetricsTail() {
	Registry.MustRegister(
		CEXStreamEntrySkipsTotal, CEXStreamSubscriptionRejected,

		VerifyArchiveLedgersVerified,
		VerifyArchiveCurrentLedger,
		VerifyArchiveCheckpointsTotal,
		VerifyArchiveMismatchesTotal,

		// Passkey sign-in refusal counter; registered here to keep registerAppMetrics under funlen.
		PasskeyLoginRefusalsTotal,
		PasskeyCredentialChangesTotal,

		AMMSwapReceivedDivergenceTotal,
		// Per-reference divergence metrics, here rather than beside
		// DivergenceRefreshTotal in [registerAppMetrics] for funlen.
		DivergenceReferenceTotal,
		DivergencePairQuorumMet,
		DivergenceMaxAbsFraction,
		DivergencePairsOver,
		// Readiness-check gauge: the only alertable signal for ClickHouse, which has no exporter.
		DependencyUp,

		// Scam-gate directory-lookup fail-open counter; registered here for the funlen ceiling.
		ScamGateLookupFailuresTotal,

		// Belongs beside SourceUnknownSymbolsTotal; registered here because registerAppMetrics sits on
		// the funlen ceiling (docs/contributing/add-metric.md). TestHandler_ExposesMetrics scrapes for it.
		SourceUnrepresentableSymbolsTotal,

		// Served-tier shutdown-loss counter, the Postgres twin of ChLiveSinkLedgersTotal{outcome="dropped"}.
		SinkUndrainedRowsTotal,

		// Aggregator catalogue/allow-list gap gauge; registered here for the funlen ceiling.
		AggregatorCatalogueTickersSkipped,

		// Dispatcher-level counters; registered here for the funlen ceiling.
		DispatcherTxReadErrorsTotal,
		DispatcherTxEventReadErrorsTotal,
		DispatcherEntryMetaUnsupportedTotal,
		DispatcherEvictedKeysUnreadableTotal,
		DispatcherLedgerUpgradeEntriesTotal,
		SourceUncorroboratedCallsTotal,

		MEVDetectRunsTotal,
		MEVEventsInsertedTotal,
		MEVDetectDurationSeconds,
		MEVScanTruncatedTotal,
		MEVLakeOrderLookupSkippedTotal,
		TxIndexTagLookupSkippedTotal,
		ExplorerExactFallbackTotal,
		CountsRollupPlanOK,
		CountsRollupStalePartitions,

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
		PriceAlertEvaluatedTotal,
		PriceAlertLastSweepUnix,

		AssetsPopularPriceless,
		PricelessCoverageCheckRunsTotal,
		PricelessCoverageCheckLastSuccessUnix,

		ChangeSummaryPassesTotal,
		ChangeSummaryLastSuccessUnix,

		NotifySendsTotal,

		DEXTradeNonstandardDecimalsTotal,
		DecimalsGuardSweepLastSuccessUnix,
		PriceServeDeclinedNonstandardDecimalsTotal,
		NonstandardDecimalsCacheRefreshFailuresTotal,
		NonstandardDecimalsPartialAliasFamilyTotal,
		NonstandardDecimalsLockstepMismatchTotal,
	)
	registerStorageAndExplorerMetrics()
	registerAuthReaperMetrics()
	registerFailedAuthMetrics()
	registerRedisFailureMetrics()

	seedBoundedLabelSeries()
}

// registerStorageAndExplorerMetrics registers the hashdb, ledgerstream,
// DEX/SDEX and explorer refresh metrics; split out for the funlen ceiling.
func registerStorageAndExplorerMetrics() {
	Registry.MustRegister(
		HashdbAppendTotal,
		HashdbAppendDurationSeconds,
		HashdbVerifyRunsTotal,
		HashdbVerifyRunDurationSeconds,
		HashdbDriftTotal,

		LedgerstreamTierReadTotal,
		LedgerstreamStreamPathTotal,
		LedgerstreamColdReadDurationSeconds,
		LedgerstreamLiveStartRetriesTotal,
		LedgerstreamTrailingMissingToleratedTotal,

		DEXTVLRefreshTotal,
		DEXTVLRefreshDurationSeconds,
		DEXTVLReconcileTotal,
		SDEXOrderBookMaintainTotal,
		SDEXOrderBookMaintainDurationSeconds,
		SDEXOrderBookCrossedPairs,
		SDEXOrderBookPendingOffers,
		SDEXOrderBookUndecodableOffersTotal,
		ExplorerSWRRefreshTotal, ExplorerSWRRefreshDurationSeconds,
		ExplorerRefreshGateSaturatedTotal,
		CHSchemaProbePresent, CHSchemaProbeUnansweredTotal, ExplorerSchemaAbsentEmptyTotal,
		ProtocolDetailRefreshTotal,
		ProtocolDetailRefreshDurationSeconds,
		WorkerPanicsTotal,
		DecoderPanicsTotal,
		SourceAmountDegradedTotal,
	)
}

// registerFailedAuthMetrics registers and zero-seeds [FailedAuthTotal];
// split out of registerAppMetricsTail for funlen.
func registerFailedAuthMetrics() {
	Registry.MustRegister(FailedAuthTotal)
	FailedAuthTotal.WithLabelValues(FailedAuthRejected)
	FailedAuthTotal.WithLabelValues(FailedAuthThrottled)
}

// registerRedisFailureMetrics registers and zero-seeds the signals for a
// failing or write-refusing Redis, so the first failure is a visible
// increase() rather than the silent first sample of a new child.
func registerRedisFailureMetrics() {
	Registry.MustRegister(RateLimitFailClosedTotal, RedisCommandErrorsTotal, UsageUnitsDroppedTotal)
	for _, limiter := range []string{RateLimiterAPI, RateLimiterFailedAuth, RateLimiterSignupIP} {
		RateLimitFailClosedTotal.WithLabelValues(limiter)
	}
	for _, class := range RedisErrorClasses {
		RedisCommandErrorsTotal.WithLabelValues(class)
	}
	for _, counter := range []string{UsageCounterBillable, UsageCounterDetail} {
		UsageUnitsDroppedTotal.WithLabelValues(counter)
	}
}

// registerAuthReaperMetrics registers the platform-table reapers' metrics
// (signup, login-code, magic-link, retention); split out of registerAppMetricsTail for funlen.
func registerAuthReaperMetrics() {
	Registry.MustRegister(
		SignupReaperRunsTotal,
		SignupReaperRunDurationSeconds,
		SignupReaperRowsDeletedTotal,
		AccountRows,
		APIKeyRows,

		LoginCodeLockoutRows,
		LoginCodeLockoutRowsDeletedTotal,
		LoginCodeLockoutErrorsTotal,

		MagicLinkTokenRows,
		MagicLinkTokenRowsDeletedTotal,
		MagicLinkTokenErrorsTotal,

		AuthReaperLastSweepUnix,
		AuthReaperIntervalSeconds,

		RetentionReaperRows,
		RetentionReaperRowsDeletedTotal,
		RetentionReaperErrorsTotal,
	)
	for _, reaper := range []string{AuthReaperSession, AuthReaperWebhookDelivery, AuthReaperRegistration} {
		RetentionReaperRowsDeletedTotal.WithLabelValues(reaper)
		RetentionReaperErrorsTotal.WithLabelValues(reaper)
	}
}

// seedBoundedLabelSeries pre-registers the zero-valued label combinations alert rules select on.
// Split out of [registerAppMetrics] to stay under the funlen ceiling.
func seedBoundedLabelSeries() {
	// Pre-seed zero series for bounded-label counters alerted via rate()/increase(): a CounterVec child
	// does not exist until its first Inc, so the alert reads "no data", indistinguishable from a dead
	// metric. AggregatorFXSnapFallbackTotal's per-pair `leg` label is unbounded, so it stays emit-on-error.
	for _, outcome := range []string{"ok", "missing_leg", "parse_error", "redis_error", "frozen_leg", "frozen_leg_direct_served", "low_confidence", "proxy_pivot"} {
		AggregatorTriangulationsTotal.WithLabelValues(outcome)
	}
	// The self-pair exploit detector is expected to sit at zero indefinitely; seeding lets an
	// operator tell "armed but quiet" from "dead metric". Its `source` label is bounded to the
	// single known producer (comet).
	AMMSelfPairSwapTotal.WithLabelValues("comet")
	// Same dead-metric ambiguity as AMMSelfPairSwapTotal above, same fix.
	AMMNonPositiveSwapTotal.WithLabelValues("comet")
	// Zero-leg fills are rare (tens per day) and sdex is their only known
	// producer; seeded so a fresh deploy shows "armed", not "absent".
	TradesZeroLegAdmittedTotal.WithLabelValues("sdex")
	AMMSwapReceivedDivergenceTotal.WithLabelValues("phoenix")
	for _, outcome := range []string{"written", "buffered", "dropped", "errored"} {
		ChLiveSinkLedgersTotal.WithLabelValues(outcome)
	}
	// The shutdown-loss counter sits at zero on clean deploys; seeding lets an operator tell "no
	// row abandoned" from "counter never wired". Unrolled because a loop tips gocognit.
	SinkUndrainedRowsTotal.WithLabelValues(SinkPersistEvents, "trade")
	SinkUndrainedRowsTotal.WithLabelValues(SinkPersistEvents, "event")
	for _, outcome := range []string{"ok", "scan_error", "write_error"} {
		MEVDetectRunsTotal.WithLabelValues(outcome)
	}
	MEVScanTruncatedTotal.WithLabelValues("trades")
	MEVScanTruncatedTotal.WithLabelValues("oracle_updates")
	MEVScanTruncatedTotal.WithLabelValues("auction_fills")
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
	// The durable login-code lockout's three fail-soft paths. All are silent at the HTTP layer,
	// and a fail-open control with an absent series looks the same as one that never failed.
	for _, op := range []string{
		LoginCodeLockoutOpStatusCheck, LoginCodeLockoutOpRegister, LoginCodeLockoutOpSweep,
	} {
		LoginCodeLockoutErrorsTotal.WithLabelValues(op)
	}
	// The magic-link retention sweep's single fail-soft path, seeded so a janitor that has never
	// failed is distinguishable from one emitting no series.
	for _, op := range []string{MagicLinkTokenOpSweep} {
		MagicLinkTokenErrorsTotal.WithLabelValues(op)
	}
	seedNotifySeries()
	seedPasskeyCredentialChangeSeries()
	// Bounded outcome set for the backpressure retry counter
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
	// hashdb outcomes seeded so alerts read zero, not "no data".
	seedHashdbSeries()
	seedLedgerstreamTierSeries()
	// ADR-0019 freeze-lifecycle release modes. `operator` signals the calibration is producing
	// freezes humans keep undoing; unseeded it is absent until the first manual unfreeze.
	for _, mode := range []string{"auto", "operator", "lapsed"} {
		AnomalyFreezeReleasedTotal.WithLabelValues(mode)
	}
	// Durable freeze-row and ladder write sites (migration 0119). Bounded set of three;
	// seeded because this counter's whole purpose is to make a SILENT
	// failure visible, and an absent series is itself a silence.
	for _, op := range []string{"record_freeze", "mark_hold", "clear"} {
		AnomalyFreezeLadderWriteFailuresTotal.WithLabelValues(op)
	}
	// Privileged-mutation surfaces whose audit row can fail to land, enumerated at the six call
	// sites. Seeded so the alert's increase() reads a real zero instead of "no data" on a fresh
	// API, which looks the same as a dead metric.
	for _, surface := range []string{
		"account_override", "key_mint", "key_revoke",
		"status_notice",
		// The staff customer look-up is a PII READ rather than a
		// mutation, but the accountability gap is identical — the row
		// that records who read whose data is the only trace it happened.
		"staff_customer_lookup",
		// The operator account read (GET /v1/admin/accounts/{id}) returns
		// the same billing email.
		"admin_account_read",
		// Self-service first-factor credential changes, and the passkey
		// sign-in refusals that carry a credential-theft signal.
		"passkey_register", "passkey_delete",
		"passkey_clone_warning", "passkey_login_replay",
	} {
		AdminAuditWriteFailuresTotal.WithLabelValues(surface)
	}
	// Unrolled rather than looped, same reason as SinkUndrainedRowsTotal
	// above: the loop tipped this function over the gocognit ceiling.
	PasskeyLoginRefusalsTotal.WithLabelValues(PasskeyRefusalCloneWarning)
	PasskeyLoginRefusalsTotal.WithLabelValues(PasskeyRefusalCeremonyReplay)
	// Tier-clamp outcomes — bounded set of two. `failed` in particular
	// means paid throughput stayed live past a downgrade, so it must be
	// distinguishable from "nothing has been clamped yet".
	for _, outcome := range []string{"lowered", "failed"} {
		AdminKeyBudgetClampsTotal.WithLabelValues(outcome)
	}
	// The two mint routes that funnel through clampMintToCaller.
	// Unrolled rather than looped: the loop tipped seedBoundedLabelSeries
	// over the gocognit ceiling, and two literal routes read no worse.
	MintScopeClampRefusedTotal.WithLabelValues("/v1/admin/keys")
	MintScopeClampRefusedTotal.WithLabelValues("/v1/account/keys")
	// Producer-side webhook fan-out losses. Event types are platform.WebhookEventType's closed
	// enum, kept as literals so internal/obs avoids importing internal/platform; reasons are the
	// FanoutFailure* constants above. Seeded because a lost fan-out leaves no delivery row to
	// count later, so "never fired" and "nothing publishes" would be the same scrape.
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
	// FX sanity-band rejections. Bounded (one source x three reasons) and seeded because the alertable state is a SUSTAINED
	// non-zero rate; an absent series would make "the band has never rejected anything" and "the worker never started" identical.
	for _, reason := range []string{"deviation", "non_positive", "non_finite"} {
		ExternalFXRateRejectedTotal.WithLabelValues("massive", reason)
	}

	seedBoundedLabelSeriesTail()
}

// seedHashdbSeries pre-registers hashdb append and verify (outcome x window) series.
func seedHashdbSeries() {
	for _, outcome := range []string{"ok", "error", "drift"} {
		if outcome != "drift" {
			HashdbAppendTotal.WithLabelValues(outcome)
		}
		for _, window := range []string{"recent", "history"} {
			HashdbVerifyRunsTotal.WithLabelValues(outcome, window)
		}
	}
}

// seedBoundedLabelSeriesTail continues seedBoundedLabelSeries — split
// for the same gocognit ceiling that split registerAppMetrics.
func seedBoundedLabelSeriesTail() {
	// Both divergence guards compare a failure rate against `ok`. Unseeded, a process that never
	// refreshed successfully (restarted mid-outage) has no `ok` child and BOTH alerts stay silent in
	// the total-outage case they exist for. Values mirror orchestrator/divergence_refresh.go.
	for _, outcome := range []string{"ok", "no_vwap", "parse_error", "refresh_error", "no_reference"} {
		DivergenceRefreshTotal.WithLabelValues(outcome)
	}
	// The customer-webhook sender's full outcome vocabulary, seeded so the first `exhausted` (delivery
	// lost) or `mark_error` (the same request re-POSTed every lease) fires its alert instead of
	// reading 0. TestCustomerWebhookDeliveryOutcomesAreSeeded derives the set from customerwebhook/worker.go.
	for _, outcome := range []string{
		"delivered", "server_error", "client_error", "exhausted", "network_error",
		"webhook_missing", "disabled", "no_secret", "build_error", "list_error",
		"lookup_error", "mark_error",
	} {
		CustomerWebhookDeliveryAttemptsTotal.WithLabelValues(outcome)
	}
	// Background cache workers. Seeded so the DEX-TVL / order-book failure-rate queries read a
	// real zero from process start; the load_* pair matters most, since a process whose initial
	// book load never attempted looks healthy without the zero series.
	for _, outcome := range []string{"ok", "error"} {
		DEXTVLRefreshTotal.WithLabelValues(outcome)
	}
	for _, outcome := range []string{"ok", "divergent"} {
		DEXTVLReconcileTotal.WithLabelValues(outcome)
	}
	for _, outcome := range []string{"load_ok", "load_error", "advance_ok", "advance_error"} {
		SDEXOrderBookMaintainTotal.WithLabelValues(outcome)
	}
	for _, cache := range []string{
		"accounts_wealth", "asset_holders", "contract_detail", "contract_stats", "contracts_dir",
		"network_throughput", "op_type_stats", "protocol_bespoke", "ttl_liveness",
	} {
		for _, outcome := range []string{"ok", "error"} {
			ExplorerSWRRefreshTotal.WithLabelValues(cache, outcome)
		}
	}
	// The guard's trailing-fetch error path is fail-open and
	// EXPECTED to sit at zero on a healthy DB, so an absent series would
	// be indistinguishable from "never wired" — seed both known paths.
	for _, path := range []string{"latest", "at"} {
		PricingGuardTrailingFetchFailedTotal.WithLabelValues(path)
		for _, reason := range []string{"outlier", "unvalidated"} {
			PricingGuardDegradedTotal.WithLabelValues(path, reason)
		}
	}
	for _, reason := range []string{"outlier", "unvalidated"} {
		PricingGuardDegradedTotal.WithLabelValues("series", reason)
	}
	// Zero on a healthy archive; seeded so "never happened" is
	// distinguishable from "never wired".
	for _, kind := range []string{
		"tx_read_errors", "tx_event_read_errors", "entry_meta_unsupported",
		"soroban_fee_meta_unsupported", "evicted_keys_unreadable", "tx_read_errors_census",
		"tx_event_read_errors_census",
	} {
		ChLiveSinkReadUndercountTotal.WithLabelValues(kind)
	}
}

// seedLedgerstreamTierSeries pre-registers the ADR-0027 tiered-read outcomes so the `both_missing`
// page reads a real zero before the first cold read. Split out for the gocognit ceiling.
func seedLedgerstreamTierSeries() {
	for _, outcome := range []string{"hot", "cold", "both_missing"} {
		LedgerstreamTierReadTotal.WithLabelValues(outcome)
	}
	for _, path := range []string{"tiered", "cold_degraded", "hot_single_ledger", "sdk"} {
		LedgerstreamStreamPathTotal.WithLabelValues(path)
	}
}

// seedNotifySeries pre-registers the transactional-email outcomes (2 call sites × {sent, failed}) so
// the send-failure alert can tell "never failed" from "mailer dead". Split out for the gocognit ceiling.
func seedNotifySeries() {
	for _, template := range []string{
		NotifyTemplateMagicLink, NotifyTemplateSignupVerify, NotifyTemplatePasskeyChanged, NotifyTemplateAccountErased,
	} {
		for _, result := range []string{NotifySendResultSent, NotifySendResultFailed, NotifySendResultSuppressed} {
			NotifySendsTotal.WithLabelValues(template, result)
		}
	}
}

func seedPasskeyCredentialChangeSeries() {
	PasskeyCredentialChangesTotal.WithLabelValues(PasskeyChangeAdded)
	PasskeyCredentialChangesTotal.WithLabelValues(PasskeyChangeRemoved)
}

// Handler returns an http.Handler serving Prometheus-formatted metrics from [Registry], mounted at /metrics.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{
		// Compression removes redundant labels from the scrape body
		// at the cost of a tiny bit of CPU per scrape.
		EnableOpenMetrics: true,
	})
}

// ─── HTTP-layer metrics ──────────────────────────────────────────

// HTTPRequestsTotal counts HTTP requests by method, route pattern (not raw URL, for cardinality) and status class; scrape job names differ per host, so alert rules use a regex.
var HTTPRequestsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Count of HTTP requests served by the API, labelled by method, route pattern, and status.",
	},
	[]string{"method", "route", "status"},
)

// HTTPRequestDuration is request latency in seconds, first byte in to last byte out; buckets 1 ms to 10 s put the 200 ms SLA on a boundary.
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

// DependencyUp reports each /v1/readyz check as a gauge (1 answered, 0 not); alert on `== 0`, never on `absent()` alone.
var DependencyUp = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_dependency_up",
		Help: "Result of each /v1/readyz readiness check: 1 = the dependency answered, 0 = it did not. Covers postgres, schema, redis and clickhouse.",
	},
	[]string{"dependency"},
)

// IngestGapLedgers is the total missing ledgers in contiguous gaps at or above the detector threshold, per source and table; reset to 0 when none remain.
var IngestGapLedgers = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_ingest_gap_ledgers",
		Help: "Total missing ledgers in contiguous data-coverage gaps (>= detector min-gap-size) per (source, table). Data-derived; complements cursor-coverage density.",
	},
	[]string{"source", "table"},
)

// IngestGapCount counts contiguous gaps per source, telling one halt (cascade signature) from many small drops at the same missing-ledger total.
var IngestGapCount = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_ingest_gap_count",
		Help: "Number of contiguous data-coverage gaps (>= detector min-gap-size) per (source, table) at the most recent detector cycle.",
	},
	[]string{"source", "table"},
)

// IngestGapMaxSize is the size of the largest contiguous gap per source, so one PromQL query can alert on the biggest hole.
var IngestGapMaxSize = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_ingest_gap_max_size_ledgers",
		Help: "Size of the largest contiguous data-coverage gap per (source, table) at the most recent detector cycle.",
	},
	[]string{"source", "table"},
)

// IngestGapDetectorRunsTotal counts detector cycles by outcome (ok, error); error means the underlying SQL failed.
var IngestGapDetectorRunsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_ingest_gap_detector_runs_total",
		Help: "Periodic data-gap detector runs, by (source, table, outcome). Rate goes to zero if the worker has wedged.",
	},
	[]string{"source", "table", "outcome"},
)

// IngestSourceDistinctLedgers is COUNT(DISTINCT ledger) per (source, table) over the detector's trailing window; deep-history coverage is the ADR-0033 verdict's job.
var IngestSourceDistinctLedgers = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_ingest_source_distinct_ledgers",
		Help: "Distinct-ledger count per (source, table) at the most recent gap-detector cycle. Numerator of the ADR-0031 data-derived density signal.",
	},
	[]string{"source", "table"},
)

// IngestGapDetectorTip is the ledgerstream cursor at the start of the latest gap-detector cycle, shared by every target scan.
var IngestGapDetectorTip = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_ingest_gap_detector_tip_ledger",
		Help: "Live ledgerstream tip ledger at the most recent gap-detector cycle. Upper bound of the per-target trailing scan window; the lower bound is per-source (see source_coverage_snapshots).",
	},
)

// IngestGapDetectorDurationSeconds measures detector-cycle latency by outcome; chart ok separately from error.
var IngestGapDetectorDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name: "stellarindex_ingest_gap_detector_duration_seconds",
		Help: "Wall-clock duration of one data-gap detector cycle, by (source, table, outcome). Buckets extend to 600s because soroban_events scans on r1 measure ~300s against ~50M distinct ledgers.",
		// Buckets reach 600s because the soroban_events scan takes ~300s.
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600},
	},
	[]string{"source", "table", "outcome"},
)

// IngestGapDetectorLastSuccessUnix stamps the latest successful per-(source, table) scan; a timestamp, not rate(ok), because restarts pin 6h-cadence counters at 1.
var IngestGapDetectorLastSuccessUnix = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_ingest_gap_detector_last_success_unix",
		Help: "Wall-clock timestamp (Unix seconds) of the most recent successful gap-detector scan per (source, table). The silent-detector alert keys off its staleness; reset-proof across restarts, unlike rate() over the once-per-6h runs_total counter.",
	},
	[]string{"source", "table"},
)

// ProjectorLagLedgers is ledgerstream tip minus last_projected_ledger per source (ADR-0032), measured against the tip even when the lake watermark clamps the scan.
var ProjectorLagLedgers = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_projector_lag_ledgers",
		Help: "Per-source projector lag in ledgers (ledgerstream tip - last_projected). 0 = caught up. Sustained > 256 = falling behind.",
	},
	[]string{"source"},
)

// ProjectorRunsTotal counts projector cycles per source by outcome; a zero rate for 5+ min means a wedged loop.
var ProjectorRunsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_projector_runs_total",
		Help: "Per-source projector cycle outcomes (ok, error, idle, watermark_held, sink_retry, decode_degraded, gate_widened). Rate goes to zero if the source's loop has wedged.",
	},
	[]string{"source", "outcome"},
)

// ProjectorEventsDecoded counts events the projector emitted through the sink or that failed decode, by outcome; compare rate(ok) with the dispatcher counter.
var ProjectorEventsDecoded = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_projector_events_decoded_total",
		Help: "Per-source events the projector decoded + emitted (ok) or failed to decode (decode_error, reconstruct_error). Compare ok-rate against dispatcher equivalent to gauge parallel-mode parity.",
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

// ProjectorWedged is 1 when the adaptive window sits at MinBatchLimit and the source has not advanced for projector.WedgeCycles cycles; remediation is manual.
var ProjectorWedged = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_projector_wedged",
		Help: "Per-source projector wedge flag: 1 = the adaptive window is floored at MinBatchLimit and the source has failed to advance for WedgeCycles consecutive cycles (a stuck cursor that retries the identical range forever); 0 = healthy. Manual remediation — see the projector-wedged runbook.",
	},
	[]string{"source"},
)

// ProjectorReplayWindowActive is 1 while a source's cursor is inside a `projector-replay` window, so the lag rule skips intended lag; a read error forces 0.
var ProjectorReplayWindowActive = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_projector_replay_window_active",
		Help: "1 while a source's projector cursor is inside an operator-recorded projector-replay rewind window (intended lag); 0 otherwise. Suppresses stellarindex_projector_lag_high and arms stellarindex_projector_replay_stalled.",
	},
	[]string{"source"},
)

// HTTPRequestSuccessDuration is HTTPRequestDuration for non-5xx responses only: the latency-SLO numerator, so a fast 500 cannot count as good.
var HTTPRequestSuccessDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "http_request_success_duration_seconds",
		Help:    "Request latency histogram for non-5xx responses only. Pair with http_request_duration_seconds_count for SLO ratios that burn budget on fast 5xx.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.5, 1, 2.5, 5, 10},
	},
	[]string{"method", "route"},
)

// APICacheOpsTotal counts cache-wrapper reads by op and result (hit, stale, miss, refresh_error, evicted); filter alerts to read outcomes, since `evicted` fires per admitted key.
var APICacheOpsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_api_cache_ops_total",
		Help: "Cache operations in API cache wrappers (in-memory and *_redis read-through), labelled by cache name + op + result (read outcomes hit|miss|stale|error, side-events refresh|refresh_error|evicted).",
	},
	[]string{"cache", "op", "result"},
)

// ─── Ingestion-layer metrics ─────────────────────────────────────

// SourceEventsTotal counts events per source emitted by the consumer to its out-channel.
var SourceEventsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_source_events_total",
		Help: "Total events emitted by each ingestion source.",
	},
	[]string{"source"},
)

// SourceLastEventUnix is the Unix timestamp of each source's most recent observed event.
var SourceLastEventUnix = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_source_last_event_unix",
		Help: "Timestamp of the most recent event per source (Unix seconds).",
	},
	[]string{"source"},
)

// SourceLastInsertUnix stamps the latest trade row actually inserted per source; diverging from SourceLastEventUnix flags a stuck cursor or replay loop.
var SourceLastInsertUnix = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_source_last_insert_unix",
		Help: "Wall-clock timestamp of the most recent successfully-inserted trade row per source (Unix seconds). Stops advancing during a stuck-cursor / duplicate-flood pattern — the gap vs stellarindex_source_last_event_unix is the diagnostic signature.",
	},
	[]string{"source"},
)

// SourceEnabled is a per-source 0/1 gauge of config-time enablement, qualifying the source_stopped alert.
var SourceEnabled = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_source_enabled",
		Help: "1 if source is configured enabled; 0 otherwise.",
	},
	[]string{"source"},
)

// SourceMatchedEventsTotal counts inputs a decoder's Matches() claimed before Decode: the denominator of decoder error rate (numerator SourceDecodeErrorsTotal).
var SourceMatchedEventsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_source_matched_events_total",
		Help: "Inputs each source's decoder Matches() claimed (the denominator of decoder error-rate).",
	},
	[]string{"source"},
)

// SourceDecodeErrorsTotal counts decode failures per source (SCVal parse errors, malformed event schemas).
var SourceDecodeErrorsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_source_decode_errors_total",
		Help: "Events that failed to decode, per source.",
	},
	[]string{"source"},
)

// DecoderPanicsTotal counts decoder panics recovered by the dispatcher, projector or projected-rebuild; each skips an input, so it pages on the raw value.
var DecoderPanicsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_decoder_panics_total",
		Help: "Decoder panics recovered by the dispatcher or projector and skipped as decode errors, per source. Non-zero means a decoder bug is dropping every event of that shape.",
	},
	[]string{"source"},
)

// SourceAmountDegradedTotal counts decoded events per source and field whose money-bearing field failed to parse and was written NULL, which would otherwise silently undercount sums.
var SourceAmountDegradedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_source_amount_degraded_total",
		Help: "Decoded events whose money-bearing field could not be parsed and was recorded NULL, labelled by source and field. Non-zero means served volume is silently under-reporting.",
	},
	[]string{"source", "field"},
)

// DispatcherTxReadErrorsTotal counts malformed transactions skipped during ProcessLedger (dispatcher.Stats.TxReadErrors), process-wide.
var DispatcherTxReadErrorsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_dispatcher_tx_read_errors_total",
		Help: "Malformed transactions skipped during ProcessLedger. A sustained climb means the bad-tx skip is masking a downstream price gap.",
	},
)

// DispatcherTxEventReadErrorsTotal counts transactions whose GetTransactionEvents() failed; non-zero means Soroban event ingestion is broken even if reconcile reads complete.
var DispatcherTxEventReadErrorsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_dispatcher_tx_event_read_errors_total",
		Help: "Transactions whose Soroban events failed to read (e.g. unsupported TransactionMeta version). Non-zero means Soroban ingestion is silently broken.",
	},
)

// DispatcherEntryMetaUnsupportedTotal counts transactions whose entry-change walk was skipped for an unhandled TransactionMeta version; a sustained climb blinds the supply observers.
var DispatcherEntryMetaUnsupportedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_dispatcher_entry_meta_unsupported_total",
		Help: "Transactions whose apply-phase entry changes were skipped for an unhandled TransactionMeta version.",
	},
)

// DispatcherEvictedKeysUnreadableTotal counts ledgers whose evicted-key list failed to read, so their state-archival evictions never reached the entry decoders.
var DispatcherEvictedKeysUnreadableTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_dispatcher_evicted_keys_unreadable_total",
		Help: "Ledgers whose state-archival evicted keys failed to read; their evictions were skipped and the evicted entries stay served as live.",
	},
)

// DispatcherLedgerUpgradeEntriesTotal counts ledger upgrade entries seen (dispatcher.Stats.LedgerUpgradeEntries); observed only, no decoder reads them.
var DispatcherLedgerUpgradeEntriesTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_dispatcher_ledger_upgrade_entries_total",
		Help: "Ledger upgrade entries (protocol version, base reserve, config settings) seen in ingested ledgers.",
	},
)

// SourceUncorroboratedCallsTotal counts oracle-class ContractCall invocations dropped before Decode because they were only declared in the auth tree; the only signal of a price-forgery attempt.
var SourceUncorroboratedCallsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_source_uncorroborated_calls_total",
		Help: "Oracle-class ContractCall invocations refused for lack of execution corroboration, per source. Non-zero means a rejected forged call or a routing-shape change.",
	},
	[]string{"source"},
)

// SourceUnknownSymbolsTotal counts oracle asset slots off the canonical allow-list or feed registry, recorded as `raw:<symbol>` rows: a mapping gap to close, not lost data.
var SourceUnknownSymbolsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_source_unknown_symbols_total",
		Help: "Asset slots in a decoded oracle event whose symbol/feed id isn't in the canonical allow-list; recorded verbatim as raw:<symbol> rows rather than dropped.",
	},
	[]string{"source"},
)

// ExternalPollerPollsTotal counts PollOnce calls by outcome (success, empty, error, skipped, idle); alert on a zero success rate for sources expected to contribute.
var ExternalPollerPollsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_external_poller_polls_total",
		Help: "External poller invocations, labelled by source and outcome (success | empty | error | skipped | idle).",
	},
	[]string{"source", "outcome"},
)

// ExternalDustDroppedTotal counts streamed CEX trades per source dropped at ingest as dust (quote leg below ~$0.001), which would pollute OHLC high/low.
var ExternalDustDroppedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_external_dust_dropped_total",
		Help: "Streamed CEX trades dropped at ingest as sub-$0.001 dust, by source.",
	},
	[]string{"source"},
)

// ExternalBadTimestampDroppedTotal counts CEX trades per source dropped for an implausible vendor timestamp (future-dated beyond skew, or before Stellar existed).
var ExternalBadTimestampDroppedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_external_bad_timestamp_dropped_total",
		Help: "CEX trades dropped at ingest for an implausible vendor timestamp, by source.",
	},
	[]string{"source"},
)

// ExternalPollerRefusedEntriesTotal counts poller entries refused per source and reason (ambiguous symbol, mismatched id); each refusal drops a reference price.
var ExternalPollerRefusedEntriesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_external_poller_refused_entries_total",
		Help: "External poller response entries refused as ambiguous or inconsistent, by source and reason (ambiguous_symbol | id_mismatch).",
	},
	[]string{"source", "reason"},
)

// AMMSelfPairSwapTotal counts AMM swaps decoded as token_in == token_out and dropped; any sustained count is exploit-shaped. Detection only.
var AMMSelfPairSwapTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_amm_self_pair_swap_total",
		Help: "AMM swaps dropped as self-pair (token_in==token_out) — an exploit primitive with no honest purpose. By source.",
	},
	[]string{"source"},
)

// AMMNonPositiveSwapTotal counts AMM swaps with an amount <= 0, dropped; recency-gated so history sweeps do not inflate it. Detection only.
var AMMNonPositiveSwapTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_amm_non_positive_swap_total",
		Help: "AMM swaps dropped for a non-positive amount (in/out <= 0). By source.",
	},
	[]string{"source"},
)

// AMMSwapReceivedDivergenceTotal counts swaps whose pool-received sell amount differs from the taker's offer (fee-on-transfer token); counts decodes, so replays re-count. Detection only.
var AMMSwapReceivedDivergenceTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_amm_swap_received_divergence_total",
		Help: "AMM swaps whose pool-received sell amount differs from the taker's offer (fee-on-transfer sell token). Stored base leg is unchanged. By source.",
	},
	[]string{"source"},
)

// CEXStreamDisconnectTotal counts CEX WebSocket disconnects per source from [wsclient.Loop], by reason (see [CEXStreamDisconnectReasons]); stall is the half-open socket that looks alive.
var CEXStreamDisconnectTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_cex_stream_disconnect_total",
		Help: "CEX WebSocket stream disconnects by source and reason (" +
			strings.Join(CEXStreamDisconnectReasons, " | ") + "). F-0029.",
	},
	[]string{"source", "reason"},
)

// CEXStreamDisconnectReasons is the full "reason" label set of CEXStreamDisconnectTotal, the single source of truth for its Help text.
var CEXStreamDisconnectReasons = []string{
	"stall", "reset", "broken_pipe", "timeout", "dial",
	"server_requested", "subscription_rejected", "other",
}

// CEXStreamLastTradeUnix stamps the latest trade a CEX streamer forwarded; streamers never set [ExternalPollerLastSuccessUnix], so this is their staleness signal.
var CEXStreamLastTradeUnix = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_cex_stream_last_trade_unix",
		Help: "UNIX timestamp of the most recent trade forwarded from a CEX WebSocket streamer, by source.",
	},
	[]string{"source"},
)

// CEXStreamEntrySkipsTotal counts trade entries a CEX parser could not convert inside a well-formed multi-trade frame; the only signal that a venue changed a symbol or encoding.
var CEXStreamEntrySkipsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_cex_stream_entry_skips_total",
		Help: "CEX WebSocket trade entries skipped inside a parsed frame, by source and reason (" +
			strings.Join(CEXStreamEntrySkipReasons, " | ") + ").",
	},
	[]string{"source", "reason"},
)

// CEXStreamEntrySkipReasons is the full label set of CEXStreamEntrySkipsTotal's "reason" label.
var CEXStreamEntrySkipReasons = []string{
	"unknown_symbol", "bad_qty", "bad_price", "bad_timestamp", "bad_trade_id", "other",
}

// CEXStreamSubscriptionRejected is 1 while the venue refuses the trade subscription for (source, symbol); the socket stays healthy and the pair silent, so no other signal sees it.
var CEXStreamSubscriptionRejected = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_cex_stream_subscription_rejected",
		Help: "1 when a CEX WebSocket venue rejected the trade subscription for a symbol, 0 once the venue acknowledges it.",
	},
	[]string{"source", "symbol"},
)

// ExternalPollerLastSuccessUnix is the per-source UNIX timestamp of the last successful PollOnce, zero until the first; staleness is `time() - gauge`.
var ExternalPollerLastSuccessUnix = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_external_poller_last_success_unix",
		Help: "UNIX seconds of the most recent successful PollOnce, per source. Zero = never succeeded since startup.",
	},
	[]string{"source"},
)

// ExternalFXLastQuoteUnix stamps the latest non-empty committed fx_quotes write from the forex worker (not a poller); alert stellarindex_external_fx_feed_stale.
var ExternalFXLastQuoteUnix = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_external_fx_last_quote_unix",
		Help: "UNIX seconds of the most recent successful fx_quotes write per FX source (currently `massive`). Reset-proof liveness for the active fiat-FX feed the triangulation forex-snap depends on; only advances on a committed batch carrying a fresh upstream rate (not the synthetic USD anchor or carried-forward history).",
	},
	[]string{"source"},
)

// ExternalFXRateRejectedTotal counts upstream FX rates the forex worker refused, by reason; two-strike guard, so a sustained rate means a wedged ticker. Not labelled by ticker.
var ExternalFXRateRejectedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_external_fx_rate_rejected_total",
		Help: "Upstream FX rates refused by the forex worker's sanity band before reaching fx_quotes, per source and reason (deviation/non_positive/non_finite). Sustained non-zero means a currency is wedged on its last accepted rate.",
	},
	[]string{"source", "reason"},
)

// ExternalFXBaselineHealedTotal counts times the forex worker re-pointed a ticker's sanity-band baseline at the trailing-7d median; rare by design.
var ExternalFXBaselineHealedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_external_fx_baseline_healed_total",
		Help: "FX sanity-band baselines re-pointed at an agreeing history majority that refuted them, per source. Each increment is one poisoned/stale baseline self-corrected.",
	},
	[]string{"source"},
)

// FX fixings (fx_fixings, migration 0193): the vendor-time hourly series a
// closed derived fiat price binds to. The forex worker appends it after each
// refresh; these carry its liveness and the gate's verdicts.
var (
	FXFixingsLastRefreshUnix = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "stellarindex_fx_fixings_last_refresh_unix",
		Help: "UNIX seconds of the forex worker's last completed fx_fixings append cycle, whether or not it wrote rows.",
	})
	FXFixingsNewestBarEndUnix = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "stellarindex_fx_fixings_newest_bar_end_unix",
		Help: "UNIX seconds of the newest bar_end this process has committed to fx_fixings.",
	})
	FXFixingsQuoteDisagrees = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "stellarindex_fx_fixings_quote_disagrees",
		Help: "1 when an accepted fx_fixings bar sits outside the sanity band of the worker's guarded daily rate for the same ticker and day, else 0.",
	}, []string{"ticker"})
	FXFixingsWriteTxSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "stellarindex_fx_fixings_write_tx_seconds",
		Help:    "Duration of one fx_fixings batch insert transaction.",
		Buckets: prometheus.DefBuckets,
	})
	FXFixingsWriteErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "stellarindex_fx_fixings_write_errors_total",
		Help: "fx_fixings batch inserts that failed.",
	})
	FXFixingsFetchErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stellarindex_fx_fixings_fetch_errors_total",
		Help: "Per-ticker hourly-bar fetches from the FX vendor that failed.",
	}, []string{"ticker"})
	FXFixingsBarsRefusedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stellarindex_fx_fixings_bars_refused_total",
		Help: "Vendor FX bars the fx_fixings gate refused: close outside [0.5, 1.5] of the median of the preceding 96h of raw bars.",
	}, []string{"ticker"})
)

// SourceUnrepresentableSymbolsTotal counts oracle slots dropped because the symbol cannot be held even as `raw:`; needs a registry entry AND a replay.
var SourceUnrepresentableSymbolsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_source_unrepresentable_symbols_total",
		Help: "Oracle asset slots dropped because the published symbol/feed id cannot be represented even as a raw: asset; each increment is a row the record layer could not write.",
	},
	[]string{"source"},
)

// SourceOrphanEventsTotal counts well-formed events that aged out uncorrelated (Soroswap swap/sync half, incomplete Phoenix set); a sustained rate means drops or a contract shape shift.
var SourceOrphanEventsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_source_orphan_events_total",
		Help: "Events that arrived without their required correlation partner, per source.",
	},
	[]string{"source"},
)

// SourceNonDirectionalSwapsTotal counts clean swap+sync pairs that moved value within one token side (a recognised non-trade, ADR-0033 expected-zero); informational.
var SourceNonDirectionalSwapsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_source_non_directional_swaps_total",
		Help: "Completed swap+sync pairs recognized as non-directional (single-side reserve move, not a trade), per source.",
	},
	[]string{"source"},
)

// DiscoveryDroppedHitsTotal counts discovery hits dropped because the async sink buffer was full.
var DiscoveryDroppedHitsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_discovery_dropped_hits_total",
		Help: "Discovery hits dropped because the async discovery sink buffer was full.",
	},
)

// DiscoverySkippedHitsTotal counts discovery hits deduplicated in-process before the sink; a high ratio is healthy.
var DiscoverySkippedHitsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_discovery_skipped_hits_total",
		Help: "Discovery hits skipped because (contract_id, event_type) was already enqueued in this process.",
	},
)

// AggregatorCatalogueTickersSkipped is the number of catalogue tickers with a `coingecko_id` excluded from the aggregator cross-check (not on the ADR-0014 allow-list); set once at startup.
var AggregatorCatalogueTickersSkipped = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_aggregator_catalogue_tickers_skipped",
		Help: "Verified-currency catalogue tickers with a coingecko_id excluded from the aggregator cross-check pair set because they are not on the ADR-0014 crypto allow-list.",
	},
)

// DiscoveryRecordFailuresTotal counts failed discovery Recorder.Record writes; a sustained rate means discovered_assets has stopped growing.
var DiscoveryRecordFailuresTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_discovery_record_failures_total",
		Help: "Discovery hits whose Recorder.Record write failed (recorder outage/timeout); logged-only before, now countable + alertable.",
	},
)

// MetricsRegistryPresent is 1 when a component that can run without a Registry got one; production leaves ledgerstream's nil, so its SDK buffer metrics are absent.
var MetricsRegistryPresent = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_metrics_registry_present",
		Help: "1 if the named component was wired with a Prometheus Registry (its SDK buffer metrics are live), 0 if it is running Registry-less (those SDK buffer metrics are absent). Set per component at boot.",
	},
	[]string{"component"},
)

// LedgerstreamTierReadTotal counts tiered-datastore reads (ADR-0027) by outcome (hot, cold, both_missing); package-level because the production ledgerstream Registry is nil.
var LedgerstreamTierReadTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_ledgerstream_tier_read_total",
		Help: "Tiered datastore reads partitioned by which tier served the request. hot=local MinIO; cold=AWS public bucket fallback; both_missing=neither tier has the object.",
	},
	[]string{"outcome"},
)

// LedgerstreamStreamPathTotal counts ledgerstream.Stream walks by read path; a rising cold_degraded rate is a configured cold tier silently out of service.
var LedgerstreamStreamPathTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_ledgerstream_stream_path_total",
		Help: "ledgerstream.Stream walks by read path: tiered, cold_degraded (configured cold tier failed to open; hot-only), hot_single_ledger, sdk.",
	},
	[]string{"path"},
)

// LedgerstreamColdReadDurationSeconds is cold-tier read latency by outcome (ok, miss, error); package-level sibling of [LedgerstreamTierReadTotal].
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

// LedgerstreamLiveStartRetriesTotal counts live-tail start retries before the first ledger; the only series that moves when the indexer is healthy but cannot open the lake.
var LedgerstreamLiveStartRetriesTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_ledgerstream_live_start_retries_total",
		Help: "Re-attempts of a live-tail stream that failed before delivering any ledger (datastore unreachable / schema unreadable at start). Climbs only while the indexer is up but cannot open the lake.",
	},
)

// LedgerstreamTrailingMissingToleratedTotal counts bounded walks that ended early because TolerateTrailingMissing turned a missing-ledger error into walk-complete.
var LedgerstreamTrailingMissingToleratedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_ledgerstream_trailing_missing_tolerated_total",
		Help: "Bounded ledgerstream walks whose missing-ledger error was converted to walk-complete by TolerateTrailingMissing (the walk returned nil short of its To).",
	},
)

// RateLimitFailOpenTotal counts requests that skipped the rate-limit check because of a Redis error; the middleware fails open so a Redis outage does not take the API down.
var RateLimitFailOpenTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_ratelimit_fail_open_total",
		Help: "Requests that bypassed rate-limiting because Redis errored.",
	},
)

// RateLimitFailClosedTotal counts requests rejected (503 or 429) because a Redis-backed throttle had been erroring past its dwell time.
var RateLimitFailClosedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_ratelimit_fail_closed_total",
		Help: "Requests rejected because a Redis-backed throttle was unavailable past its dwell-time, by limiter.",
	},
	[]string{"limiter"},
)

// Label values for [RateLimitFailClosedTotal].
const (
	RateLimiterAPI        = "api"
	RateLimiterFailedAuth = "failed_auth"
	RateLimiterSignupIP   = "signup_ip"
)

// RedisCommandErrorsTotal counts failed Redis commands from every redisclient-built client, by error class; covers best-effort write paths that otherwise fail silently.
var RedisCommandErrorsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_redis_command_errors_total",
		Help: "Failed Redis commands by error class (Redis error prefix, or io/timeout/canceled for transport failures).",
	},
	[]string{"class"},
)

// RedisErrorClasses is the complete, bounded `class` vocabulary of [RedisCommandErrorsTotal].
var RedisErrorClasses = []string{
	"MISCONF", "OOM", "READONLY", "NOREPLICAS", "EXECABORT", "LOADING",
	"WRONGTYPE", "BUSY", "MASTERDOWN", "NOAUTH", "WRONGPASS", "NOPERM",
	"CLUSTERDOWN", "TRYAGAIN", "MOVED", "ASK", "ERR",
	"io", "timeout", "canceled", "other",
}

// MonthlyQuotaFailOpenTotal counts requests that skipped the monthly-quota ceiling because the month-to-date read errored; a metered key can bill past its cap meanwhile.
var MonthlyQuotaFailOpenTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_monthly_quota_fail_open_total",
		Help: "Requests that bypassed the per-key monthly quota ceiling because the month-to-date read errored.",
	},
)

// MonthlyQuotaFailClosedTotal counts requests 429'd because the quota counter stayed unreadable past the fail-open dwell; non-zero means paying customers are refused.
var MonthlyQuotaFailClosedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_monthly_quota_fail_closed_total",
		Help: "Requests rejected (429) because the month-to-date read stayed unavailable past the fail-open dwell window.",
	},
)

// SourceInsertErrorsTotal counts persistence failures per source by kind (trade, trade_abandoned, oracle, panic, unhandled), separate from decode errors.
var SourceInsertErrorsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_source_insert_errors_total",
		Help: "Events that failed to persist to the store, per source + kind (trade/trade_abandoned/oracle/panic).",
	},
	[]string{"source", "kind"},
)

// SourceInsertErrorsTotal kinds for trades. The any-rate
// stellarindex_ingestion_persist_drop tripwire keys on the dropped kind,
// so an abandon (re-derivable, cursor held) must never share it.
const (
	// InsertErrorKindTradeDropped: a trade permanently dropped on a data
	// fault — the row is not in the served tier.
	InsertErrorKindTradeDropped = "trade"
	// InsertErrorKindTradeAbandoned: a trade whose infra-fault retry was
	// abandoned on ctx cancellation; re-derivable from the CH lake.
	InsertErrorKindTradeAbandoned = "trade_abandoned"
)

// TradesZeroLegAdmittedTotal counts trades stored with exactly one zero leg (unpriceable); SDEX is the only known producer. Detection only.
var TradesZeroLegAdmittedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_trades_zero_leg_admitted_total",
		Help: "Trades admitted to the served tier with exactly one zero leg (stored, unpriceable). By source.",
	},
	[]string{"source"},
)

// CursorLastLedger is the last-committed cursor value per source in ingestion_cursors, used to detect stuck cursors.
var CursorLastLedger = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_cursor_last_ledger",
		Help: "Last ledger committed to the per-source cursor.",
	},
	[]string{"source"},
)

// DivergenceRefreshTotal counts divergence cache refreshes by outcome; only `ok` writes a fresh entry, so alerts also fire when ok stops.
var DivergenceRefreshTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_divergence_refresh_total",
		Help: "Aggregator divergence-cache refresh outcomes per Tick (ok|no_vwap|parse_error|refresh_error|no_reference).",
	},
	[]string{"outcome"},
)

// DivergenceRefresherWired is 1 while the divergence pass has a refresher and windows, 0 when every reference is disabled; gates the no-ok liveness alert.
var DivergenceRefresherWired = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_divergence_refresher_wired",
		Help: "1 when the aggregator's divergence-cache refresh pass is wired (at least one reference configured), 0 when it is disabled and div:<asset> is never written.",
	},
)

// DivergenceRefreshDurationSeconds is per-pair refresh latency by outcome; buckets 10ms-30s.
var DivergenceRefreshDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_divergence_refresh_duration_seconds",
		Help:    "Per-pair divergence-refresh latency, labelled by outcome (ok|no_vwap|parse_error|refresh_error).",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
)

// DivergenceReferenceTotal counts (reference, pair) lookups per refresh by reference and outcome; shows one reference going dark when DivergenceRefreshTotal stays ok.
var DivergenceReferenceTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_divergence_reference_total",
		Help: "Divergence-refresh reference lookups by reference and outcome (ok|asset_unsupported|price_unavailable|too_stale_to_compare|invalid_price|timeout|overall_deadline_exceeded|panicked|error).",
	},
	[]string{"reference", "outcome"},
)

// DivergencePairQuorumMet is 1 when a pair's latest refresh had at least min_sources_for_warning responding references; 0 means detection is disarmed for the pair.
var DivergencePairQuorumMet = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_divergence_pair_quorum_met",
		Help: "1 when the pair's latest divergence refresh met the reference quorum (min_sources_for_warning), 0 when detection was disarmed.",
	},
	[]string{"pair"},
)

// DivergenceMaxAbsFraction is the largest |ours - reference| / reference across pairs a reference prices (0.05 = 5%); labelled by reference only.
var DivergenceMaxAbsFraction = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_divergence_max_abs_fraction",
		Help: "Largest absolute fractional gap between our price and the reference's, over the pairs the reference currently prices (0.05 = 5%).",
	},
	[]string{"reference"},
)

// DivergencePairsOver counts pairs whose worst reference gap exceeds a fixed fraction; threshold is 5pct or 10pct.
var DivergencePairsOver = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_divergence_pairs_over",
		Help: "Pairs whose largest reference gap exceeds the threshold (5pct|10pct).",
	},
	[]string{"threshold"},
)

// UsageRollupSweepsTotal counts internal/usage.Rollup sweeps by outcome (ok, scan_error, sink_error); sustained non-ok makes usage analytics stale, pricing unaffected.
var UsageRollupSweepsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_usage_rollup_sweeps_total",
		Help: "Usage-rollup worker sweep outcomes (ok|scan_error|sink_error).",
	},
	[]string{"outcome"},
)

// UsageUnitsDroppedTotal counts request units served but not recorded in the Redis usage counters; a dropped `billable` unit is unmetered quota and cannot be reconstructed.
var UsageUnitsDroppedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_usage_units_dropped_total",
		Help: "Request units whose usage-counter write failed, by counter (billable|detail).",
	},
	[]string{"counter"},
)

// Label values for [UsageUnitsDroppedTotal].
const (
	UsageCounterBillable = "billable"
	UsageCounterDetail   = "detail"
)

// UsageRollupSweepDurationSeconds is per-sweep usage-rollup latency by outcome; buckets 5ms-30s.
var UsageRollupSweepDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_usage_rollup_sweep_duration_seconds",
		Help:    "Usage-rollup sweep latency, labelled by outcome (ok|scan_error|sink_error).",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
)

// ProtocolEventsRollupSweepsTotal counts protoeventsrollup sweeps feeding protocol_events_24h; refresh_error keeps the previous rows (stale, not blank).
var ProtocolEventsRollupSweepsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_protocol_events_rollup_sweeps_total",
		Help: "Protocol-events rollup worker sweep outcomes (ok|refresh_error).",
	},
	[]string{"outcome"},
)

// ProtocolEventsRollupSweepDurationSeconds is per-sweep latency of the 24h event census by outcome; buckets 10ms-30s.
var ProtocolEventsRollupSweepDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_protocol_events_rollup_sweep_duration_seconds",
		Help:    "Protocol-events rollup sweep latency, labelled by outcome (ok|refresh_error).",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
)

// AssetVolumeRollupSweepsTotal counts assetvolrollup sweeps feeding asset_volume_24h; refresh_error keeps the previous rows (stale, not blank).
var AssetVolumeRollupSweepsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_asset_volume_rollup_sweeps_total",
		Help: "Asset-volume rollup worker sweep outcomes (ok|refresh_error).",
	},
	[]string{"outcome"},
)

// AssetVolumeRollupSweepDurationSeconds is per-sweep latency of the all-asset prices_1m scan by outcome; buckets 50ms-60s.
var AssetVolumeRollupSweepDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_asset_volume_rollup_sweep_duration_seconds",
		Help:    "Asset-volume rollup sweep latency, labelled by outcome (ok|refresh_error).",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
	},
	[]string{"outcome"},
)

// AssetCharacterRollupSweepsTotal counts assetcharacterrollup sweeps feeding asset_volume_character; refresh_error keeps the previous rows; analytics-only.
var AssetCharacterRollupSweepsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_asset_character_rollup_sweeps_total",
		Help: "Asset-volume-character rollup worker sweep outcomes (ok|refresh_error).",
	},
	[]string{"outcome"},
)

// AssetCharacterRollupSweepDurationSeconds is per-sweep latency of the full-window all-asset `trades` roll by outcome; buckets 50ms-120s.
var AssetCharacterRollupSweepDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_asset_character_rollup_sweep_duration_seconds",
		Help:    "Asset-volume-character rollup sweep latency, labelled by outcome (ok|refresh_error).",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
	},
	[]string{"outcome"},
)

// PriceAlertEvalTotal counts pricealerts sweeps by outcome; list_error skips the whole sweep so NO alerts are evaluated, partial_error means at least one alert failed.
var PriceAlertEvalTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_price_alert_eval_total",
		Help: "Price-alert evaluator sweep outcomes (ok|list_error|partial_error).",
	},
	[]string{"outcome"},
)

// PriceAlertEvaluatedTotal counts one increment per alert per sweep by that alert's outcome, separating one failed alert from all failing.
var PriceAlertEvaluatedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_price_alert_evaluated_total",
		Help: "Per-alert price-alert evaluation outcomes (fired|not_crossed|already_fired|no_price|stale|cooling_down|no_subscriber|claim_lost|error|timeout).",
	},
	[]string{"outcome"},
)

// PriceAlertLastSweepUnix is when the evaluator last completed a sweep (any outcome), seeded with its start time; the only signal of a sweep stuck behind a slow alert.
var PriceAlertLastSweepUnix = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_price_alert_last_sweep_unix",
		Help: "Unix time the price-alert evaluator last completed a sweep; seeded with its start time, 0 when the evaluator never started.",
	},
)

// PriceAlertEvalDurationSeconds is the latency of one price-alert sweep by outcome; buckets 5ms-30s.
var PriceAlertEvalDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_price_alert_eval_duration_seconds",
		Help:    "Price-alert evaluation sweep latency, labelled by outcome (ok|list_error|partial_error).",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
)

// AssetsPopularPriceless counts assets above the popularity floor with no servable price and not withheld by the substance floor; > 0 is an unexplained coverage gap that should page.
var AssetsPopularPriceless = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_assets_popular_priceless",
		Help: "Count of market-popular, priceless, non-withheld assets at the most recent coverage-check sweep. > 0 is an unexplained pricing-coverage gap.",
	},
)

// PricelessCoverageCheckRunsTotal counts priceless-popular tripwire sweeps (ok, degraded, error); sustained error/degraded means the tripwire cannot be trusted.
var PricelessCoverageCheckRunsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_priceless_coverage_check_runs_total",
		Help: "Priceless-popular coverage-check sweep outcomes (ok|degraded|error).",
	},
	[]string{"outcome"},
)

// PricelessCoverageCheckLastSuccessUnix is the time of the last successful tripwire sweep; `time() - this` powers the staleness alert.
var PricelessCoverageCheckLastSuccessUnix = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_priceless_coverage_check_last_success_unix",
		Help: "Unix seconds of the most recent successful priceless-popular coverage-check sweep.",
	},
)

// ChangeSummaryPassesTotal counts explorer delta-strip worker passes by outcome (ok, partial, failed); failed means the table is going stale.
var ChangeSummaryPassesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_change_summary_passes_total",
		Help: "change_summary_5m refresh pass outcomes (ok|partial|failed).",
	},
	[]string{"outcome"},
)

// ChangeSummaryLastSuccessUnix is the time of the last pass that upserted at least one entity, seeded at start; powers stellarindex_change_summary_stale.
var ChangeSummaryLastSuccessUnix = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_change_summary_last_success_unix",
		Help: "Unix seconds of the last change_summary_5m refresh pass that upserted at least one entity; seeded at worker start, 0 when the worker is not running.",
	},
)

// SignupReaperRunsTotal counts internal/signupreaper sweeps deleting orphan `accounts` rows left Suspended by a callback race; sustained error is a slow leak, not an outage.
var SignupReaperRunsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_signup_reaper_runs_total",
		Help: "Speculative-account reaper sweep outcomes (ok|error).",
	},
	[]string{"outcome"},
)

// SignupReaperRunDurationSeconds is the latency of one reaper sweep (a single bounded DELETE) by outcome; buckets 5 ms to 30 s.
var SignupReaperRunDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_signup_reaper_run_duration_seconds",
		Help:    "Speculative-account reaper sweep latency, labelled by outcome (ok|error).",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
)

// SignupReaperRowsDeletedTotal is the cumulative orphan accounts the reaper deleted; its rate is the signup-race orphan production rate.
var SignupReaperRowsDeletedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_signup_reaper_rows_deleted_total",
		Help: "Cumulative speculative (signup-race) orphan accounts deleted by the reaper.",
	},
)

// AccountRows is the `accounts` row count, refreshed each signup-reaper sweep; the only signal of registration volume or abuse since the reaper never deletes real registrations.
var AccountRows = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_accounts_rows",
		Help: "Rows in accounts. POST /v1/register is unauthenticated and durable, so sustained growth outside expected traffic is registration-path abuse, not necessarily legitimate signups.",
	},
)

// APIKeyRows is the `api_keys` row count, refreshed each signup-reaper sweep alongside [AccountRows].
var APIKeyRows = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_api_keys_rows",
		Help: "Rows in api_keys. POST /v1/register mints one durable key per account, so this moves in lockstep with stellarindex_accounts_rows.",
	},
)

// Operation labels on [LoginCodeLockoutErrorsTotal]. Bounded set of
// three — one per place the durable login-code lockout can
// fail without the sign-in itself failing.
const (
	// LoginCodeLockoutOpStatusCheck: the pre-match lockout read failed. The handler FAILS OPEN here (a locked address reaches a
	// code comparison), so this is the one that means the control is silently off.
	LoginCodeLockoutOpStatusCheck = "status_check"
	// LoginCodeLockoutOpRegister — a wrong-code attempt was not recorded
	// against the durable per-email counter. The grinder got a free
	// guess.
	LoginCodeLockoutOpRegister = "register"
	// LoginCodeLockoutOpSweep — the retention sweep failed. Rows an
	// unauthenticated caller can create accumulate until it recovers.
	LoginCodeLockoutOpSweep = "sweep"
)

// LoginCodeLockoutRows is the `login_code_lockouts` row count; its key is attacker-chosen, so this gauge shows logincodereaper's bound holding.
var LoginCodeLockoutRows = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_login_code_lockout_rows",
		Help: "Rows in login_code_lockouts. The key is attacker-chosen and the endpoint is unauthenticated, so sustained growth is a remote table-fill, not user behaviour.",
	},
)

// LoginCodeLockoutRowsDeletedTotal is the cumulative settled lockout rows removed by the retention sweep; read next to [LoginCodeLockoutRows].
var LoginCodeLockoutRowsDeletedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_login_code_lockout_rows_deleted_total",
		Help: "Cumulative settled login_code_lockouts rows deleted by the retention sweep.",
	},
)

// LoginCodeLockoutErrorsTotal counts durable login-code lockout failures by `op`, all silent at the HTTP layer; op="status_check" means the lockout is not enforced. Pre-seeded.
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
	// unauthenticated caller can create (POST /v1/auth/login)
	// accumulate until it recovers.
	MagicLinkTokenOpSweep = "sweep"
)

// MagicLinkTokenRows is the `magic_link_tokens` row count (plaintext PII, attacker-chosen key); shows magiclinkreaper's bound holding.
var MagicLinkTokenRows = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_magic_link_token_rows",
		Help: "Rows in magic_link_tokens. The key is attacker-chosen and POST /v1/auth/login is unauthenticated, so sustained growth is a remote table-fill, not user behaviour.",
	},
)

// MagicLinkTokenRowsDeletedTotal is the cumulative expired magic-link rows removed by the retention sweep; read next to [MagicLinkTokenRows].
var MagicLinkTokenRowsDeletedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_magic_link_token_rows_deleted_total",
		Help: "Cumulative expired magic_link_tokens rows deleted by the retention sweep.",
	},
)

// MagicLinkTokenErrorsTotal counts magic-link retention-sweep failures by `op`, silent at the HTTP layer; pre-seeded across the bounded op set.
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
	// NotifyTemplatePasskeyChanged — the notice to a dashboard user that a
	// passkey was added to or removed from their account. A failure means
	// the owner is not told about a first-factor change.
	NotifyTemplatePasskeyChanged = "passkey-changed"
	// NotifyTemplateAccountErased — the confirmation to an account's owners
	// that the account was erased. A failure means the owner is
	// not told about an irreversible deletion.
	NotifyTemplateAccountErased = "account-erased"

	// NotifySendResultSent — Sender.Send returned nil (accepted by Resend).
	NotifySendResultSent = "sent"
	// NotifySendResultFailed — Sender.Send returned an error (validation,
	// provider-rejected, or transient/network). The mail did not go out.
	NotifySendResultFailed = "failed"
	// NotifySendResultSuppressed — recipient is on the suppression list; no
	// provider call was made. Not a failure.
	NotifySendResultSuppressed = "suppressed"
)

// NotifySendsTotal counts transactional-email sends per template and result (sent, failed, suppressed); suppressed is never a failure. Zero-seeded.
var NotifySendsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_notify_sends_total",
		Help: "Transactional-email sends per template and result (sent|failed|suppressed). A sustained failed ratio means the mail provider (Resend) is failing — magic-link login and signup-verification stop delivering.",
	},
	[]string{"template", "result"},
)

// MEVDetectRunsTotal counts internal/aggregate/mev runs by outcome (ok, scan_error, write_error); sustained non-ok means /v1/mev is going stale.
var MEVDetectRunsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_mev_detect_runs_total",
		Help: "MEV detection run outcomes (ok|scan_error|write_error).",
	},
	[]string{"outcome"},
)

// MEVEventsInsertedTotal counts new (non-duplicate) MEV events written across runs; the detector dedups on write, so these are first detections.
var MEVEventsInsertedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_mev_events_inserted_total",
		Help: "New MEV events persisted (post-dedup) across detection runs.",
	},
)

// MEVDetectDurationSeconds is per-run MEV detector latency by outcome; a slow Postgres scan stretches the tail.
var MEVDetectDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_mev_detect_duration_seconds",
		Help:    "MEV detection run latency, labelled by outcome (ok|scan_error|write_error).",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
)

// MEVScanTruncatedTotal counts MEV input scans that hit the worker's ScanLimit, so detection saw only the newest rows; `input` is trades, oracle_updates or auction_fills.
var MEVScanTruncatedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_mev_scan_truncated_total",
		Help: "MEV detector input scans that hit ScanLimit — detection ran over a partial (newest-N) slice of the window.",
	},
	[]string{"input"},
)

// MEVLakeOrderLookupSkippedTotal counts tx hashes left unordered because a tx_hash_index chunk failed.
var MEVLakeOrderLookupSkippedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_mev_lake_order_lookup_skipped_total",
		Help: "tx hashes dropped from ClickHouse tx_hash_index resolution because a lookup chunk failed (e.g. MEMORY_LIMIT_EXCEEDED); the whole call's hashes are left unordered for that tick rather than guessed.",
	},
)

// TxIndexTagLookupSkippedTotal counts tx hashes the trades.tx_index tagger left untagged because a tx_hash_index chunk failed.
var TxIndexTagLookupSkippedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_tx_index_tag_lookup_skipped_total",
		Help: "tx hashes the trades.tx_index tagger (live sweep and tag-tx-index) left untagged because a ClickHouse tx_hash_index lookup chunk failed; the page's rows stay NULL until a later pass resolves them.",
	},
)

// ExplorerExactFallbackTotal counts explorer page reads whose bounded window could not prove a full page and re-ran as the exact query.
var ExplorerExactFallbackTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_explorer_exact_fallback_total",
		Help: "Explorer page reads whose bounded sort-key window could not prove a full page of distinct keys (un-merged duplicates), so the read re-ran as the exact LIMIT 1 BY / dedup query, which is O(account or contract history).",
	},
	[]string{"read"},
)

// CountsRollupPlanOK is 1 while the explorer reader has a usable plan for a counts rollup, 0 while it falls back to the source scan.
var CountsRollupPlanOK = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_counts_rollup_plan_ok",
		Help: "1 while the explorer reader holds a usable read plan for the counts rollup (op-types, movements); 0 while the rollup table is missing, unreadable or has no valid partition, and account panels scan the source instead.",
	},
	[]string{"rollup"},
)

// CountsRollupStalePartitions counts the closed partitions the reader's current plan reads from the source because the rollup no longer matches.
var CountsRollupStalePartitions = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_counts_rollup_stale_partitions",
		Help: "Partitions the explorer reader's current counts-rollup plan reads from the source FINAL because their rollup row count no longer matches (or was never built); ch-optype-rollup rebuilds 4 per run.",
	},
	[]string{"rollup"},
)

// OracleStreamRowsUnparsedTotal counts oracle_updates rows LatestOracleStreams drops because asset or quote will not parse (a bad raw-SQL relabel would otherwise look like success); `field` names which.
var OracleStreamRowsUnparsedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_oracle_stream_rows_unparsed_total",
		Help: "oracle_updates rows dropped from the served stream because their stored canonical asset/quote text would not parse, per source and field.",
	},
	[]string{"source", "field"},
)

// TradeInsertsTotal counts trade inserts per source by whether `usd_volume` was populated; steady "no" on a USDC-quoting venue means the configured asset_key does not match the decoder's.
var TradeInsertsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_trade_inserts_total",
		Help: "Trade-insert attempts, labelled by source and whether usd_volume was populated (yes|no|refused|unroutable|thin|error; error = left NULL because a USD resolver read failed, counted against coverage; unroutable = unpriced trade whose two classic legs share one issuer, or neither of whose legs had any peg or XLM market, thin = unpriced because the only candidate rate came from a market below the substance floor; both excluded from the coverage alerts). Counts attempts, not unique-row inserts — on-conflict dedupe AND generation-guarded corrective updates are both invisible to this counter.",
	},
	[]string{"source", "usd_volume_populated"},
)

// TradeInsertOutcomeTotal counts trade-insert outcomes per source; `duplicate` conflates true duplicates, corrections and guard-skipped writes, and `source` is unseeded, so stuck-cursor rules must use `unless on (source) rate(new[10m]) > 0`.
var TradeInsertOutcomeTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_trade_insert_outcome_total",
		Help: "Trade-insert outcomes per source. outcome=new when a fresh row landed; outcome=duplicate when no row was inserted — which since INV-3 conflates a true duplicate, a generation-guarded corrective UPDATE, and a guard-skipped write. A corrective re-derive therefore looks identical to a cursor replay here.",
	},
	[]string{"source", "outcome"},
)

// DexTradeUnitRatioTotal counts on-chain trades with base_amount == quote_amount, both nonzero: a plausibility check completeness cannot do; emitted in storage, off-chain excluded, `source` unseeded.
var DexTradeUnitRatioTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_dex_trade_unit_ratio_total",
		Help: "On-chain DEX trades landed with base_amount == quote_amount (both nonzero) — the signature of the 2026-07-07 Phoenix decoder field-mapping bug that silently collapsed 237k trades to a 1:1 price for months. Labels: source. CEX/FX (ledger==0) excluded — they scale amounts differently. A sustained stream from one source (not an occasional hit) indicates a decoder bug; see runbook dex.md#stellarindex_dex_trade_unit_ratio_detected.",
	},
	[]string{"source"},
)

// TradeInsertRetriesTotal counts the trade sink's blocking retry loop (retry, recovered, abandoned); genuine drops are SourceInsertErrorsTotal{kind="trade"|"dropped"} (ADR-0041).
var TradeInsertRetriesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_trade_insert_retries_total",
		Help: "Trade-sink blocking-retry events by outcome (retry|recovered|abandoned) — the 2026-07-06 Postgres-outage backpressure path.",
	},
	[]string{"outcome"},
)

// TradeInsertBufferDepth is the number of external (CEX/FX) trades held in the bounded retry buffer after an insert failure (ADR-0041); overflow drops the OLDEST.
var TradeInsertBufferDepth = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_trade_insert_buffer_depth",
		Help: "External (CEX/FX) trades currently held in the bounded retry buffer pending durable insert (ADR-0041).",
	},
)

// StreamPublishTotal counts envelopes streampublish.Publisher fanned out, one per new closed bucket.
var StreamPublishTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_stream_publish_total",
		Help: "Closed-bucket envelopes published to the streaming Hub, labelled by stream surface (e.g. price_stream).",
	},
	[]string{"stream"},
)

// StreamPublishStallTotal counts poll ticks whose reader missed its per-tick deadline while the parent ctx was live: a reader slower than the poll interval.
var StreamPublishStallTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_stream_publish_stall_total",
		Help: "Poll ticks where the reader missed its own per-poll timeout while the parent context was still live, labelled by stream surface (e.g. price_stream).",
	},
	[]string{"stream"},
)

// ─── Pricing / oracle metrics ────────────────────────────────────

// PriceStalenessSeconds is the age of the latest aggregated price per (asset, quote), alerting above 120s; writers MUST emit only an allow-list of assets, or Prometheus cardinality breaks.
var PriceStalenessSeconds = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_price_staleness_seconds",
		Help: "Age of the most recent aggregated price per (asset, quote) pair (seconds). Writers MUST restrict to a top-N allow-list.",
	},
	[]string{"asset", "quote"},
)

// OracleLastUpdateUnix stamps the latest oracle observation per (source, asset); set it only via [RecordOracleUpdate] so label sets match [OracleStalenessBudgetSeconds]; unmapped `raw:` symbols skip it.
var OracleLastUpdateUnix = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_oracle_last_update_unix",
		Help: "Timestamp of the most recent oracle observation, per source and asset.",
	},
	[]string{"source", "asset"},
)

// OracleResolutionSeconds is the per-source gauge of the oracle's declared resolution interval; it seeds the per-asset [OracleStalenessBudgetSeconds].
var OracleResolutionSeconds = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_oracle_resolution_seconds",
		Help: "Declared resolution interval of each oracle source (seconds).",
	},
	[]string{"source"},
)

// OracleStalenessBudgetSeconds is how long a (source, asset) may go unpublished before stellarindex_oracle_stale tickets; default is the source's budget, widened via [SetOracleStalenessOverrides].
var OracleStalenessBudgetSeconds = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_oracle_staleness_budget_seconds",
		Help: "Staleness budget per oracle source and asset (seconds) — the threshold stellarindex_oracle_stale compares against.",
	},
	[]string{"source", "asset"},
)

// ─── Aggregator orchestrator metrics ─────────────────────────────

// AggregatorTicksTotal counts completed orchestrator ticks by outcome (ok, or error when any (pair, window) refresh failed).
var AggregatorTicksTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_ticks_total",
		Help: "Aggregator orchestrator tick count, labelled by outcome (ok|error).",
	},
	[]string{"outcome"},
)

// AggregatorVWAPWritesTotal counts (pair, window) Redis writes by the orchestrator; unlabelled to bound cardinality.
var AggregatorVWAPWritesTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_vwap_writes_total",
		Help: "Cumulative VWAP cache writes performed by the aggregator.",
	},
)

// AggregatorEmptyWindowsTotal counts (pair, window) refreshes with zero VWAP-eligible trades after class, stablecoin and outlier filtering; unlabelled.
var AggregatorEmptyWindowsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_empty_windows_total",
		Help: "Aggregator (pair, window) refreshes that produced zero eligible trades.",
	},
)

// AggregatorWindowTruncatedTotal counts (pair, window) fetches that hit MaxTradesPerWindow, so the VWAP covered only the newest trades; unlabelled.
var AggregatorWindowTruncatedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_window_truncated_total",
		Help: "Aggregator (pair, window) fetches that hit MaxTradesPerWindow — VWAP computed over a partial (newest-N) trade slice.",
	},
)

// AggregatorVWAPCacheWriteErrorsTotal counts failed Redis SETs in the VWAP cache write; sustained, cached pairs 404 on /v1/price while flags.stale stays off.
var AggregatorVWAPCacheWriteErrorsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_vwap_cache_write_errors_total",
		Help: "Aggregator VWAP cache writes that returned a Redis error. Cumulative since process start.",
	},
)

// AggregatorContributionWriteErrorsTotal counts per-(pair, window) contribution batches the ContributionSink failed to persist; atomic, so a failure loses the whole bucket.
var AggregatorContributionWriteErrorsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_contribution_write_errors_total",
		Help: "Aggregator source-contribution batches that failed to persist to price_source_contributions. Cumulative since process start.",
	},
)

// AggregatorStreamPublishTotal counts closed-bucket events handed to the StreamPublisher by outcome (ok, error); unset without a publisher.
var AggregatorStreamPublishTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_stream_publish_total",
		Help: "Closed-bucket stream publishes attempted by the aggregator, labelled by outcome.",
	},
	[]string{"outcome"},
)

// APIStreamSubscribeTotal counts closed-bucket pub/sub messages by outcome (ok, decode_error, malformed, future/stale observed_at, duplicate, dropped_slow_consumer); only subscriber processes export it.
var APIStreamSubscribeTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_api_stream_subscribe_total",
		Help: "Closed-bucket Redis pub/sub messages processed by the API subscriber, labelled by outcome.",
	},
	[]string{"outcome"},
)

// APITipProducers is the number of shared /v1/price/tip/stream producers registered, bounded by api.streaming.max_tip_producers.
var APITipProducers = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "stellarindex_api_tip_producers",
	Help: "Shared tip-stream producers currently registered (running or lingering).",
})

// APITipProducersRefusedTotal counts tip-stream producer mints refused, by reason (caller_quota, global_ceiling).
var APITipProducersRefusedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_api_tip_producers_refused_total",
		Help: "Shared tip-stream producer mints refused, labelled by reason (caller_quota, global_ceiling).",
	},
	[]string{"reason"},
)

// APIStreamSubscriberDropsTotal counts SSE subscribers the Hub disconnected because their queue was full on publish; a sustained rate means events are lost to slow consumers.
var APIStreamSubscriberDropsTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "stellarindex_api_stream_subscriber_drops_total",
	Help: "SSE subscribers disconnected by the streaming Hub because their queue was full (slow consumer).",
})

// AfterResponseTasksDroppedTotal counts post-response bookkeeping tasks (usage counters, last-seen touches) dropped because the worker pool's queue was full.
var AfterResponseTasksDroppedTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "stellarindex_after_response_tasks_dropped_total",
	Help: "Post-response bookkeeping tasks dropped because the shared after-response worker pool queue was full.",
})

// APISSEStreamsActive is the number of SSE connections holding a slot against the global stream ceiling, across every stream endpoint.
var APISSEStreamsActive = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "stellarindex_api_sse_streams_active",
	Help: "SSE connections currently open across every stream endpoint.",
})

// APISSEStreamsRejectedTotal counts SSE connections refused with a 503 by the concurrency caps, by reason (global_cap, per_ip_cap, topic_cap).
var APISSEStreamsRejectedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_api_sse_streams_rejected_total",
		Help: "SSE connections refused by the concurrency caps, labelled by reason (global_cap, per_ip_cap, topic_cap).",
	},
	[]string{"reason"},
)

// APIStreamHubTopics is the number of topics the streaming Hub holds, the quantity its topic ceiling bounds.
var APIStreamHubTopics = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "stellarindex_api_stream_hub_topics",
	Help: "Topics currently held by the streaming Hub.",
})

// APIStreamHubTopicsReapedTotal counts topics the Hub's reaper evicted; flat at zero beside a high APIStreamHubTopics means mistuned retention.
var APIStreamHubTopicsReapedTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "stellarindex_api_stream_hub_topics_reaped_total",
	Help: "Topics evicted by the streaming Hub's reaper.",
})

// CustomerWebhookDeliveryAttemptsTotal counts webhook delivery attempts by outcome (delivered, retried, terminal and error classes); all twelve are seeded, and mark_error re-POSTs every lease.
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

// CustomerWebhookFanoutFailuresTotal counts customer events that never became a delivery row and are permanently lost, by reason; the producer-side counterpart of [CustomerWebhookDeliveryAttemptsTotal].
var CustomerWebhookFanoutFailuresTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_customer_webhook_fanout_failures_total",
		Help: "Customer events that never became a webhook delivery row, by event type and reason. Non-zero = a subscribed customer permanently lost an event (no retry row exists).",
	},
	[]string{"event_type", "reason"},
)

// CustomerWebhookDeliveryDurationSeconds is outbound delivery POST latency by outcome; buckets 10ms-60s reach the worker's 60s timeout.
var CustomerWebhookDeliveryDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_customer_webhook_delivery_duration_seconds",
		Help:    "Customer-webhook outbound HTTP POST latency, labelled by outcome (delivered|server_error|client_error|network_error|build_error). Body-drain time is included.",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
	},
	[]string{"outcome"},
)

// APICORSDecisionsTotal counts CORS outcomes per request (no_origin, allowed_origin, allowed_wildcard, denied), so an alert can catch a wildcard origin reaching prod.
var APICORSDecisionsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_api_cors_decisions_total",
		Help: "Per-request CORS decisions. Outcome ∈ {no_origin, allowed_origin, allowed_wildcard, denied}.",
	},
	[]string{"outcome"},
)

// AggregatorDroppedTradesTotal counts trades removed from VWAP input by reason (class, unpriceable, outlier) and pair; it re-counts band residents every tick, so the rate is not new outliers per second.
var AggregatorDroppedTradesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_dropped_trades_total",
		Help: "Trades removed from the VWAP input set, labelled by reason (class|unpriceable|outlier) and configured target pair.",
	},
	[]string{"reason", "pair"},
)

// AggregatorVenueVWAP is each source's pre-outlier-filter VWAP per (pair, window), deleted when a venue leaves; feeds the outlier_storm alert. float64 is fine: operator signal, never served.
var AggregatorVenueVWAP = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_aggregator_venue_vwap",
		Help: "Per-source VWAP of the pre-outlier-filter trade set for one (pair, window) refresh, on the served price scale. Feeds the venue-disagreement outlier_storm alert.",
	},
	[]string{"pair", "window", "source"},
)

// AggregatorWindowTrades is the trade count per (pair, window) after each stage (fetched, class, outlier); `1 - outlier/class` is the honest trim fraction.
var AggregatorWindowTrades = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_aggregator_window_trades",
		Help: "Trades in the current (pair, window) refresh after each filter stage (fetched|class|outlier). outlier/class is the surviving fraction of the VWAP input.",
	},
	[]string{"pair", "window", "stage"},
)

// AggregatorWindowBaseVolume is base volume in whole units per (pair, window) after the class and outlier stages; `1 - outlier/class` is the share of money trimmed. float64 only at the gauge boundary.
var AggregatorWindowBaseVolume = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_aggregator_window_base_volume",
		Help: "Base-asset volume (whole units) of the current (pair, window) refresh after each filter stage (class|outlier). 1 - outlier/class is the share of traded volume the outlier filter removed.",
	},
	[]string{"pair", "window", "stage"},
)

// AggregatorDroppedWindowsTotal counts non-empty (pair, window) refreshes suppressed by a window-level filter (reason="min_usd_volume"); distinct from AggregatorEmptyWindowsTotal.
var AggregatorDroppedWindowsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_dropped_windows_total",
		Help: "Windows the orchestrator suppressed at the window-level filter step, labelled by reason (min_usd_volume).",
	},
	[]string{"reason"},
)

// AggregatorMinUSDVolumeUnvaluableTotal counts refreshes DROPPED fail-closed because min_usd_volume is set but the quote asset has no recognised USD peg; that pair publishes nothing until added.
var AggregatorMinUSDVolumeUnvaluableTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_min_usd_volume_unvaluable_total",
		Help: "Windows DROPPED fail-closed because min_usd_volume is set but the floor is unverifiable (on-chain quote asset has no recognised USD peg, or a EUR/GBP target has no usable FX rate), labelled by pair.",
	},
	[]string{"pair"},
)

// PriceServeSubstanceWithheldTotal counts aggregated-price claims withheld by the thin-market substance gate by `surface` and first failed `floor`; NEVER a pair label (unbounded). Dashboard-only.
var PriceServeSubstanceWithheldTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_price_serve_substance_withheld_total",
		Help: "Aggregated price decisions withheld by the thin-market substance gate, labelled by serving surface and the floor that failed.",
	},
	[]string{"surface", "floor"},
)

// PriceServeSubstanceUnmeasuredTotal counts substance verdicts not reached (read error or deadline); single lookups then serve unguarded (ADR-0018). Expected zero. Dashboard-only.
var PriceServeSubstanceUnmeasuredTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_price_serve_substance_unmeasured_total",
		Help: "Substance-gate verdicts that could not be measured (store error or deadline), labelled by serving surface.",
	},
	[]string{"surface"},
)

// PriceServeThinAdmittedTotal counts thin-market prices served because the request set `include_thin=true`; labels as PriceServeSubstanceWithheldTotal. Dashboard-only.
var PriceServeThinAdmittedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_price_serve_thin_admitted_total",
		Help: "Thin-market substance verdicts served flagged under the include_thin opt-in, labelled by serving surface and the floor that failed.",
	},
	[]string{"surface", "floor"},
)

// PriceServeScamWithheldTotal counts price serves withheld because the issuer is scam-flagged in the account directory, by surface; failing open is counted by ScamGateLookupFailuresTotal.
var PriceServeScamWithheldTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_price_serve_scam_withheld_total",
		Help: "Aggregated price serves withheld by the scam-pricing gate (directory-flagged issuer), labelled by serving surface.",
	},
	[]string{"surface"},
)

// ScamGateLookupFailuresTotal counts scam-gate lookups that errored so the gate FAILED OPEN and the price served unguarded (alert: stellarindex_scam_gate_fail_open); same `surface` labels.
var ScamGateLookupFailuresTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_scam_gate_lookup_failures_total",
		Help: "Scam-pricing gate directory lookups that failed, so the price was served unguarded (fail-open), labelled by serving surface.",
	},
	[]string{"surface"},
)

// PricingGuardTrailingFetchFailedTotal counts trailing-baseline fetch errors in the serving guard by `path`; the guard fails OPEN, so a sustained rate means the manipulation band is off.
var PricingGuardTrailingFetchFailedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_pricingguard_trailing_fetch_failed_total",
		Help: "Serving-sanity guard trailing-baseline fetches that errored and fell back to serving the candidate unguarded (fail-open), by path (latest|at).",
	},
	[]string{"path"},
)

// PricingGuardDegradedTotal counts guard decisions that did not serve the current bucket as validated, by `path` and `reason` (outlier, unvalidated).
var PricingGuardDegradedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_pricingguard_degraded_total",
		Help: "Serving-sanity guard decisions that did not serve the current bucket as a validated price, by path (latest|at|series) and reason (outlier|unvalidated).",
	},
	[]string{"path", "reason"},
)

// ─── Supply-derivation metrics ────────────────────────────────────

// SupplyCrossCheckDivergenceStroops is the stroop divergence between a classic asset's Algorithm 2 supply and its SAC's Algorithm 3 supply, by classic_key and wrap_class (full_wrap, partial_wrap).
var SupplyCrossCheckDivergenceStroops = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_supply_cross_check_divergence_stroops",
		Help: "Stroop divergence between classic and SAC-wrapped supply for the same asset, labelled by wrap_class (full_wrap: |classic-sac| equality; partial_wrap: max(0, sac-classic) subset bound). Alert when > 1.",
	},
	[]string{"classic_key", "wrap_class"},
)

// SupplyCrossCheckTotal counts cross-check evaluations by outcome and wrap_class; missing_snapshot, read_error and misaligned delete the divergence series and alert as unevaluable.
var SupplyCrossCheckTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_supply_cross_check_total",
		Help: "Cross-check evaluations, labelled by outcome (within|over|missing_snapshot|read_error|misaligned) and wrap_class (full_wrap|partial_wrap).",
	},
	[]string{"outcome", "wrap_class"},
)

// SupplyDivergence* compares OUR served circulating supply against an external reference (Stellar Network
// Dashboard for XLM; CoinGecko with a Pro key), catching a stale SDF-reserve exclusion list
// (docs/methodology/xlm-circulating-supply.md). Unlike SupplyCrossCheck*, which compares our own numbers.

// SupplyDivergenceRatio is |our - reference| / reference per (asset, reference); frozen on no_reference and refresh_error, so a dead reference never manufactures a divergence.
var SupplyDivergenceRatio = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_supply_divergence_ratio",
		Help: "Absolute relative divergence |our − reference| / reference of served circulating supply, per (asset, reference). Alert when > 1% (well above the ~0.03% XLM noise floor).",
	},
	[]string{"asset", "reference"},
)

// SupplyDivergenceTotal counts supply cross-checks per (asset, tick): ok, divergent, no_reference (checker running blind, not paged) or refresh_error.
var SupplyDivergenceTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_supply_divergence_total",
		Help: "Supply cross-check evaluations per (asset, tick), labelled by outcome (ok|divergent|no_reference|refresh_error).",
	},
	[]string{"outcome"},
)

// SupplyDivergenceDurationSeconds is per-(asset, tick) check latency by outcome; buckets 10ms-30s.
var SupplyDivergenceDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_supply_divergence_duration_seconds",
		Help:    "Per-(asset, tick) supply cross-check latency, labelled by outcome (ok|divergent|no_reference|refresh_error).",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
)

// AnomalyFreezeEngagedTotal counts ActionFreeze decisions by asset class: the prior bucket's value is kept and /v1/price shows flags.frozen=true.
var AnomalyFreezeEngagedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_freeze_engaged_total",
		Help: "ActionFreeze decisions emitted by the aggregator anomaly checker, labelled by asset class.",
	},
	[]string{"class"},
)

// AnomalyWarnTotal counts ActionWarn decisions by asset class; NOT wired to flags.divergence_warning, which an anomaly warn never pairs with divergence_checked.
var AnomalyWarnTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_warn_total",
		Help: "ActionWarn decisions emitted by the aggregator anomaly checker, labelled by asset class.",
	},
	[]string{"class"},
)

// AnomalyFreezeEscalatedTotal counts freezes that exhausted ADR-0019's 2-hour extension ladder; stays until manual unfreeze, so each increment is P1. Unlabelled for cardinality.
var AnomalyFreezeEscalatedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_freeze_escalated_total",
		Help: "Freezes that exhausted the ADR-0019 extension ladder (2h) and escalated to operator review. Stays frozen until manual unfreeze.",
	},
)

// AnomalyFreezeExtensionsTotal counts ADR-0019 hold extensions (+30 min each) after a hold expired without auto-unfreeze: early warning of climbing toward escalation.
var AnomalyFreezeExtensionsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_freeze_extensions_total",
		Help: "ADR-0019 freeze-hold extensions granted (+30 min each, max 4 before escalation).",
	},
)

// AnomalyFreezeHeldUnscoredTotal counts hold expiries on a bucket the scorer could not evaluate; a sustained rate means scoring is stuck while extension and escalation counters stay silent.
var AnomalyFreezeHeldUnscoredTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_freeze_held_unscored_total",
		Help: "ADR-0019 freeze holds that expired on an unscored bucket (restart bootstrap or scoring outage).",
	},
)

// AnomalyFreezeRefiredAfterOverrideTotal counts freezes that re-fired inside the ADR-0019 override memory; non-zero means a human force-unfrozen pair was still anomalous.
var AnomalyFreezeRefiredAfterOverrideTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_freeze_refired_after_override_total",
		Help: "ADR-0019 freezes that re-fired after an override and resumed the overridden ladder.",
	},
)

// AnomalyFreezeReleasedTotal counts ended freezes by how (auto, operator, lapsed); a rising `operator` rate means calibration produces freezes humans keep undoing.
var AnomalyFreezeReleasedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_freeze_released_total",
		Help: "Freezes ended, by mode. Mode ∈ {auto, operator, lapsed}.",
	},
	[]string{"mode"},
)

// AnomalyFreezeActive is the number of (pair, window) freezes held at the end of each tick; not labelled by pair (unbounded).
var AnomalyFreezeActive = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_anomaly_freeze_active",
		Help: "(pair, window) freezes currently held by the aggregator's ADR-0019 lifecycle.",
	},
)

// AnomalyFreezeRecoveredTotal counts freeze rows the recovery worker closed; trails AnomalyFreezeEngagedTotal by the freeze TTL plus the poll interval.
var AnomalyFreezeRecoveredTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_freeze_recovered_total",
		Help: "Freeze rows closed by the recovery worker after the Redis marker TTL elapsed.",
	},
)

// AnomalyFreezeLadderRehydratedTotal counts freezes restored from the durable `freeze_events` ladder after the Redis marker vanished; a burst after a Redis restart is expected, not alerted.
var AnomalyFreezeLadderRehydratedTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_freeze_ladder_rehydrated_total",
		Help: "Freeze lifecycles restored from the durable freeze_events ladder after the Redis marker went missing.",
	},
)

// AnomalyFreezeLadderWriteFailuresTotal counts durable freeze-event row and ladder writes that did not land, by call site; `record_freeze` and `mark_hold` are the dangerous directions. All labels are pre-seeded.
var AnomalyFreezeLadderWriteFailuresTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_freeze_ladder_write_failures_total",
		Help: "Durable freeze-event row and freeze-ladder writes that did not land, by call site (record_freeze|mark_hold|clear). Sustained non-zero = SQL readers miss the freeze and the Redis-flush protection is inert.",
	},
	[]string{"op"},
)

// APIFreezeLookupFailuresTotal counts API freeze-marker reads that errored (excluding client aborts); each response carries frozen_checked=false, so sustained means prices serve without a freeze verdict.
var APIFreezeLookupFailuresTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_api_freeze_lookup_failures_total",
		Help: "API freeze-marker lookups that failed (client aborts excluded). Sustained non-zero = price responses are served with frozen_checked=false.",
	},
)

// AnomalyFreezeRecoverySweepsTotal counts recovery-worker poll cycles by outcome (ok, partial, error); partial means MarkRecovered is failing for some rows.
var AnomalyFreezeRecoverySweepsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_anomaly_freeze_recovery_sweeps_total",
		Help: "Freeze recovery-worker sweep cycles. Outcome ∈ {ok, partial, error}.",
	},
	[]string{"outcome"},
)

// AnomalyFreezeRecoverySweepDurationSeconds is per-sweep recovery-worker latency; buckets 10ms-30s, no alert (the error counter covers correctness).
var AnomalyFreezeRecoverySweepDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_anomaly_freeze_recovery_sweep_duration_seconds",
		Help:    "Freeze recovery-worker sweep latency, labelled by outcome (ok|partial|error). Sweep does ListOpen (Postgres) + per-row Redis GET + maybe MarkRecovered (Postgres write).",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
)

// AggregatorTriangulationsTotal counts triangulation computations per outcome, one per (chain, window) per tick; sustained parse_error or redis_error means an upstream regression.
var AggregatorTriangulationsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_triangulations_total",
		Help: "Aggregator triangulation outcomes per tick × chain × window (graph-router priced). Outcome ∈ {ok, missing_leg, parse_error, redis_error, frozen_leg, frozen_leg_direct_served, low_confidence, proxy_pivot}. low_confidence = no route cleared min_route_confidence, so the composite was flagged but NOT published over the direct price. proxy_pivot = a priced leg's stablecoin-proxy prints disagree with its own-quote prints, so the composite was flagged but NOT published over the direct price. frozen_leg_direct_served = a leg froze and the route around it is unreachable, but the target published its own direct print this tick anyway — served unmarked, no freeze inherited.",
	},
	[]string{"outcome"},
)

// AggregatorCompositeCorroboration is the composite-reference verdict per (pair, window) for single-venue targets, one series per verdict set to 1 (corroborated, refuted, unavailable).
var AggregatorCompositeCorroboration = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_aggregator_composite_corroboration",
		Help: "Current-bucket composite-reference verdict per (pair, window): 1 on the active verdict ∈ {corroborated, refuted, unavailable}, 0 on the others. Only structurally single-venue allow-listed targets are evaluated.",
	},
	[]string{"pair", "window", "verdict"},
)

// AggregatorRouteCorroborationCount is the router's independent, tightly-agreeing route count behind the last composite; not path_count, not venue `sources=`.
var AggregatorRouteCorroborationCount = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_aggregator_route_corroboration_count",
		Help: "Router corroboration count (independent, tightly-agreeing, non-diverged routes) behind the last published composite, per (pair, window). Distinct from path_count and from the venue source count in the freeze reason's sources= field.",
	},
	[]string{"pair", "window"},
)

// AggregatorCompositeReferenceLegSources is the distinct venue/provider count behind each composite reference leg (`leg` = canonical pair), showing corroboration strength against composite_reference.min_leg_sources.
var AggregatorCompositeReferenceLegSources = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_aggregator_composite_reference_leg_sources",
		Help: "Distinct sources behind each composite-reference leg on the last evaluated bucket, per (pair, window, leg).",
	},
	[]string{"pair", "window", "leg"},
)

// AggregatorCompositeReferenceLegDispersionBps is max |venue VWAP - leg VWAP| / leg VWAP in bps across a composite leg's venues; above composite_reference.leg_dispersion_bps the leg cannot corroborate.
var AggregatorCompositeReferenceLegDispersionBps = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_aggregator_composite_reference_leg_dispersion_bps",
		Help: "Venue dispersion (max |venue VWAP - leg VWAP| / leg VWAP, bps) of each priced composite-reference leg on the last evaluated bucket, per (pair, window, leg).",
	},
	[]string{"pair", "window", "leg"},
)

// AggregatorCompositeFreezeSuppressedTotal counts phase-2 freeze fires not engaged because the composite corroborated the move, by (pair, window).
var AggregatorCompositeFreezeSuppressedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_composite_freeze_suppressed_total",
		Help: "Phase-2 freeze fires suppressed because the current-bucket composite reference corroborated the move (corroboration_basis=composite), per (pair, window).",
	},
	[]string{"pair", "window"},
)

// AggregatorFXSnapFallbackTotal counts triangulation legs that fell back from the forex-snap rule to cached VWAP for lack of an FX quote by the bucket end, by `leg`.
var AggregatorFXSnapFallbackTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_fx_snap_fallback_total",
		Help: "Triangulations that fell back to cached VWAP for an FX leg because FXQuoteAtOrBefore returned no row at-or-before the bucket end.",
	},
	[]string{"leg"},
)

// AggregatorBaselineRefreshTotal counts baseline refreshes per pair per cycle (ADR-0019 Phase 2) by outcome (ok, ok_unvalued, ok_per_minute_fallback, not_enough_samples, read_error, write_error).
var AggregatorBaselineRefreshTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_baseline_refresh_total",
		Help: "Baseline refresh outcomes per pair × refresh cycle. Outcome ∈ {ok, ok_unvalued, ok_per_minute_fallback, not_enough_samples, read_error, write_error}.",
	},
	[]string{"outcome"},
)

// AggregatorSupplyLakeClampLedgers is how far the ledgerstream cursor leads the newest landed stellar.ledgers row when a supply snapshot resolves; shows the stalled-lake refusal coming. Not labelled by asset.
var AggregatorSupplyLakeClampLedgers = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_aggregator_supply_lake_clamp_ledgers",
		Help: "Ledgers the ingestion cursor leads the landed lake tip by when a supply snapshot is resolved. Rising toward the resolver's stalled-lake bound means supply is about to stop publishing.",
	},
)

// AggregatorSupplyRefreshTotal counts supply refreshes per (asset_key, tick); `dormant` is a benign accept, `stale_component` a real rejection, and the alert keys on asset_key so one stuck asset is not masked.
var AggregatorSupplyRefreshTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_supply_refresh_total",
		Help: "Supply-snapshot refresh outcomes per (asset_key, outcome). Outcome ∈ {ok, dormant, static_reserve, no_ledger, no_observation, compute_error, stale_component, missing_freshness, missing_baseline, write_error}.",
	},
	[]string{"asset_key", "outcome"},
)

// SupplyWriteBandBreachTotal counts supply snapshots whose total_supply moved more than supply.WriteBandFactor against the previous one; the row is still written.
var SupplyWriteBandBreachTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_supply_write_band_breach_total",
		Help: "Supply snapshots written whose total_supply moved more than 10x against the previous snapshot, per (asset_key, direction). Direction ∈ {up, down}. The row is written regardless.",
	},
	[]string{"asset_key", "direction"},
)

// AggregatorSupplyRefreshDurationSeconds is supply.Refresher.Tick latency by outcome only (not asset_key, for cardinality).
var AggregatorSupplyRefreshDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_aggregator_supply_refresh_duration_seconds",
		Help:    "Supply-snapshot refresh tick latency, labelled by outcome. Asset-level granularity available via per-tick log timestamps.",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	},
	[]string{"outcome"},
)

// SEP41SupplyRollupAdvancesTotal counts sep41_supply_rollup advances per (contract_id, tick) by outcome (ok, noop, no_cursor, error); sustained error means that contract's reader is on the slow full-scan fallback.
var SEP41SupplyRollupAdvancesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_sep41_supply_rollup_advances_total",
		Help: "SEP-41 supply rollup incremental-advance passes per (contract_id, outcome). Outcome ∈ {ok, noop, no_cursor, error}.",
	},
	[]string{"contract_id", "outcome"},
)

// SEP41SupplyRollupAdvanceDurationSeconds is per-pass rollup latency by outcome only (not contract_id, for cardinality); a cold contract's first fold can take minutes.
var SEP41SupplyRollupAdvanceDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_sep41_supply_rollup_advance_duration_seconds",
		Help:    "SEP-41 supply rollup advance-pass latency, labelled by outcome. Steady-state sub-second; the cold first fold is the exception.",
		Buckets: []float64{0.005, 0.025, 0.1, 0.5, 1, 2.5, 5, 15, 60, 300},
	},
	[]string{"outcome"},
)

// AggregatorConfidenceComputeTotal counts confidence-score outcomes per (pair, window) per tick (ADR-0019); skipped and baseline_missing are normal at bring-up, divergence_* are non-fatal but sustained means a bad divergence worker or Redis.
var AggregatorConfidenceComputeTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_aggregator_confidence_compute_total",
		Help: "Confidence-score compute outcomes per (pair, window) × tick. See package docs for the full label vocabulary.",
	},
	[]string{"outcome"},
)

// AggregatorBaselineAgeSeconds is the per-pair age of the volatility baseline row the confidence step read; shows a single pair whose refresh keeps failing.
var AggregatorBaselineAgeSeconds = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_aggregator_baseline_age_seconds",
		Help: "Age in seconds of the volatility baseline row the confidence step last read, per pair.",
	},
	[]string{"pair"},
)

// AggregatorBootstrapCapped is 1 while the pair's last confidence was bounded by the ADR-0019 bootstrap ceiling (0.5), 0 once baseline density clears the gate.
var AggregatorBootstrapCapped = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_aggregator_bootstrap_capped",
		Help: "1 when the pair's last computed confidence was bounded by the ADR-0019 bootstrap cap, else 0.",
	},
	[]string{"pair"},
)

// AggregatorBaselineDensityDays is the 30d baseline density the bootstrap cap gates on, in days-equivalent of 1-minute buckets (max 30, negative with no baseline); not calendar age.
var AggregatorBaselineDensityDays = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_aggregator_baseline_density_days",
		Help: "30d baseline density in days-equivalent of 1-minute buckets, per pair; the bootstrap cap releases at 28.5.",
	},
	[]string{"pair"},
)

// verify-archive metrics, served by `stellarindex-ops verify-archive -metrics-listen ADDR` so hours-long
// runs can be watched live. Labelled by chunk_idx, bounded by the -workers cap.

// VerifyArchiveLedgersVerified counts ledgers successfully walked and verified; its rate per chunk separates a stalled chunk from a slow one.
var VerifyArchiveLedgersVerified = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_verify_archive_ledgers_verified_total",
		Help: "Ledgers walked + verified by verify-archive, per chunk_idx.",
	},
	[]string{"chunk_idx"},
)

// VerifyArchiveCurrentLedger is the most recent ledger position per chunk; with the chunk's [from,to] range it gives percent-complete.
var VerifyArchiveCurrentLedger = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_verify_archive_current_ledger",
		Help: "Most-recent ledger sequence verified by each chunk_idx.",
	},
	[]string{"chunk_idx"},
)

// VerifyArchiveCheckpointsTotal counts Tier B checkpoint outcomes (matched, missed); missed means the archive file is absent (hard fail under -fail-on-missed).
var VerifyArchiveCheckpointsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_verify_archive_checkpoints_total",
		Help: "Tier B checkpoint outcomes per verify-archive chunk_idx, labelled by outcome (matched|missed).",
	},
	[]string{"chunk_idx", "outcome"},
)

// VerifyArchiveMismatchesTotal counts chain-break, checkpoint-mismatch and sequence-gap incidents; any non-zero reading is a hard failure.
var VerifyArchiveMismatchesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_verify_archive_mismatches_total",
		Help: "Chain breaks, sequence gaps, and checkpoint mismatches per verify-archive chunk_idx + reason (chain|sequence|checkpoint).",
	},
	[]string{"chunk_idx", "reason"},
)

// PostgresPingTotal counts the indexer's 60s watchPostgresPing probes (ok, error); alert on any error rate for 2m (a few during restart are expected).
var PostgresPingTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_postgres_ping_total",
		Help: "Indexer postgres-resilience ping outcomes. Sustained error rate ⇒ pool is dead and the lifetime safety-net hasn't refreshed yet (F-0151).",
	},
	[]string{"outcome"},
)

// PostgresPingFailureStreak is the consecutive failed-ping count, reset on success; a streak > 0 for > 2m is the page signal beside [PostgresPingTotal].
var PostgresPingFailureStreak = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_postgres_ping_failure_streak",
		Help: "Indexer postgres-ping consecutive failure count (resets to 0 on the next success). F-0151.",
	},
)

// TLSCertNotAfterUnix is the public cert's NotAfter per host from the API's 6h tls.Dial self-probe; failed probes keep the last value so an outage does not blank the alert input.
var TLSCertNotAfterUnix = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_tls_cert_not_after_unix",
		Help: "Unix-seconds NotAfter timestamp of the leaf TLS cert observed at the configured host. Set by the API binary's self-probe (F-0051). Probe failures keep the last-known value; pair with stellarindex_tls_cert_probe_total{outcome=error} to detect a stuck probe.",
	},
	[]string{"host"},
)

// TLSCertProbeTotal counts cert probes per (host, outcome: ok, dial_error, no_cert, timeout, cert_invalid, cert_expired); an error beside a stale [TLSCertNotAfterUnix] means the probe is failing.
var TLSCertProbeTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_tls_cert_probe_total",
		Help: "TLS cert self-probe outcomes per host. outcome ∈ {ok, dial_error, no_cert, timeout}. F-0051.",
	},
	[]string{"host", "outcome"},
)

// AdminAuditWriteFailuresTotal counts privileged mutations and PII reads that completed but whose audit row did not land (best-effort after commit); reconstruct from logs before retention closes. `surface` is bounded and pre-seeded.
var AdminAuditWriteFailuresTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_admin_audit_write_failures_total",
		Help: "Privileged mutations that succeeded but whose durable audit row failed to append, by surface. Non-zero = the admin audit trail has holes.",
	},
	[]string{"surface"},
)

// PasskeyLoginRefusalsTotal counts passkey sign-ins refused after the signature verified, by reason; `clone_warning` is a sign-counter regression (a key in two places), `ceremony_replay` a replayed finish-login.
var PasskeyLoginRefusalsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_passkey_login_refusals_total",
		Help: "Passkey sign-ins refused after the assertion signature verified, by reason (clone_warning|ceremony_replay).",
	},
	[]string{"reason"},
)

// PasskeyCredentialChangesTotal counts passkeys added to or removed from dashboard users' sign-in methods; a burst of `added` across accounts looks like hijacked sessions made durable.
var PasskeyCredentialChangesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_passkey_credential_changes_total",
		Help: "Passkeys added to or removed from dashboard users' sign-in methods, by change (added|removed).",
	},
	[]string{"change"},
)

// PasskeyCredentialChangesTotal change label values.
const (
	PasskeyChangeAdded   = "added"
	PasskeyChangeRemoved = "removed"
)

// PasskeyLoginRefusalsTotal reason label values.
const (
	PasskeyRefusalCloneWarning   = "clone_warning"
	PasskeyRefusalCeremonyReplay = "ceremony_replay"
)

// FailedAuthTotal counts credential rejections by the API's Auth middleware, by outcome; the fleet-wide total catches guessing spread too thinly for the per-IP and per-key throttles.
var FailedAuthTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_failed_auth_total",
		Help: "Credential rejections at the API Auth middleware, by outcome (rejected = answered 401/403; throttled = answered 429 by the failed-auth throttle).",
	},
	[]string{"outcome"},
)

// FailedAuthTotal outcome label values.
const (
	FailedAuthRejected  = "rejected"
	FailedAuthThrottled = "throttled"
)

// AdminKeyBudgetClampsTotal counts credentials whose per-minute budget clampKeyBudgetsToTier lowered to the tier ceiling (`lowered`) or failed to (`failed`, key keeps its higher budget); not alerted.
var AdminKeyBudgetClampsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_admin_key_budget_clamps_total",
		Help: "API credentials whose per-minute budget was clamped to their account tier ceiling, by outcome (lowered|failed).",
	},
	[]string{"outcome"},
)

// MintScopeClampRefusedTotal counts mints clampMintToCaller refused for asking more scope or rate limit than the minting credential holds; labelled by route, never actor.
var MintScopeClampRefusedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_mint_scope_clamp_refused_total",
		Help: "Key-mint requests refused because they exceeded the minting credential's own scope or rate limit, by route.",
	},
	[]string{"route"},
)

// ChLiveSinkLedgersTotal counts ledgers through the ClickHouse live dual-sink (ADR-0041) by outcome (written, buffered, dropped, errored), emitted as per-tick deltas; a steady dropped climb degrades the lake's live edge.
var ChLiveSinkLedgersTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_ch_live_sink_ledgers_total",
		Help: "Ledgers processed by the ClickHouse real-time dual-sink, labelled by outcome (written|buffered|dropped|errored).",
	},
	[]string{"outcome"},
)

// ChLiveSinkReadUndercountTotal counts transactions the indexer's per-ledger read paths could not fully decode, by kind (lake-extract kinds and *_census kinds); each add is a per-ledger transaction count.
var ChLiveSinkReadUndercountTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_ch_live_sink_read_undercount_total",
		Help: "Transactions/ledgers a read path could not fully decode, labelled by kind (tx_read_errors|tx_event_read_errors|entry_meta_unsupported|soroban_fee_meta_unsupported|evicted_keys_unreadable|tx_read_errors_census|tx_event_read_errors_census).",
	},
	[]string{"kind"},
)

// SinkPersistEvents is the `sink` label value of the Postgres served-tier pipeline sink on [SinkUndrainedRowsTotal], declared here so the zero-seed and the emitter cannot drift apart.
const SinkPersistEvents = "persist_events"

// SinkUndrainedRowsTotal counts rows a sink's shutdown drain could not land in budget, by sink and kind: each is a served-tier gap the cursor never revisits (trade: re-derive via `ch-rebuild -sdex-gaps`); the ERROR log stays authoritative.
var SinkUndrainedRowsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_sink_undrained_rows_total",
		Help: "Rows a sink's bounded shutdown drain abandoned unwritten while the ledger cursor had already advanced, by sink and kind (trade|event). Every increment is a served-tier gap to re-derive.",
	},
	[]string{"sink", "kind"},
)

// MarketsSkippedRowsTotal counts trades rows the /v1/markets scanner skipped because an asset failed to parse; non-zero means something bypassed the write path.
var MarketsSkippedRowsTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_markets_skipped_rows_total",
		Help: "Count of trades rows skipped by the /v1/markets scanner because base/quote did not parse as canonical asset strings. Non-zero indicates non-pipeline writes; investigate and clean up.",
	},
)

// DEXTradeNonstandardDecimalsTotal flags once per (source, asset) a DEX trade in a Soroban token whose decimals() != 7, which skews pair prices by 10^(7-decimals); not pre-seeded, alert is a bare `> 0`.
var DEXTradeNonstandardDecimalsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_dex_trade_nonstandard_decimals_total",
		Help: "DEX trades observed for a Soroban token whose on-chain decimals() != 7 — the served price for pairs involving this asset is silently skewed by 10^(7-decimals). Labels: source, asset (C-strkey). Any non-zero value is an unmitigated mispricing landmine; see runbook dex.md#dex-nonstandard-decimals.",
	},
	[]string{"source", "asset"},
)

// DecimalsGuardSweepLastSuccessUnix stamps the last successful decimals-guard Sweep; the offender counters read zero even if the guard never armed, so `time() - this` drives the stale alert.
var DecimalsGuardSweepLastSuccessUnix = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_decimals_guard_sweep_last_success_unix",
		Help: "Unix seconds of the most recent successful decimals-guard Sweep pass, stamped whether or not an offender was found; seeded with the guard's enable time before the first pass, 0 when the guard is disabled (no lake). Staleness (vs the sweep interval) means the guard never armed or has wedged, not that no offenders exist.",
	},
)

// PriceServeDeclinedNonstandardDecimalsTotal is permanently zero (its read-time 422 decline was removed); still registered so dashboards referencing it do not break.
var PriceServeDeclinedNonstandardDecimalsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_price_serve_declined_nonstandard_decimals_total",
		Help: "HISTORICAL, permanently zero since 2026-07-10: price-surface requests declined for a confirmed non-7-decimal pair leg. The decline guard was replaced by read-time decimals normalization (runbook dex.md#dex-nonstandard-decimals); retained one release for dashboard continuity.",
	},
	[]string{"asset"},
)

// NonstandardDecimalsCacheRefreshFailuresTotal counts failed refreshes of the API's NonstandardDecimalsCache, which fails open on the last-good snapshot: infra health, not a wrong price.
var NonstandardDecimalsCacheRefreshFailuresTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_nonstandard_decimals_cache_refresh_failures_total",
		Help: "Failed background refreshes of the API's in-process nonstandard-decimals serving-guard cache. Fail-open: the previous snapshot keeps serving. Infra-health signal, not a pricing-correctness one.",
	},
)

// NonstandardDecimalsPartialAliasFamilyTotal counts flagged assets whose alias family is only partly in `nonstandard_decimals_assets`; lookup does not alias-fold, so non-zero means that asset's served numbers cannot be trusted.
var NonstandardDecimalsPartialAliasFamilyTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_nonstandard_decimals_partial_alias_family_total",
		Help: "Flagged non-7-decimal assets whose alias family is only partly flagged. Expected 0. Nonzero means price and supply for that asset can diverge by a power of ten, because the decimals lookup does not alias-fold.",
	},
)

// NonstandardDecimalsLockstepMismatchTotal counts disagreements between lake decimals() and the nonstandard_decimals_assets projection (invariant: a row exists iff lake decimals != 7); request sites (asset "") refused a figure rather than mix scales.
var NonstandardDecimalsLockstepMismatchTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_nonstandard_decimals_lockstep_mismatch_total",
		Help: "Observations that the lake's on-chain decimals() and the nonstandard_decimals_assets projection disagree for one asset. Labels: site (guard_reconcile = repaired at aggregation time; asset_detail/asset_listing/rwa_contract/lending_reserve = valuation refused at request time), asset (C-strkey, populated for guard_reconcile only — request-driven sites leave it empty to bound cardinality). Expected 0; see runbook dex.md#dex-nonstandard-decimals.",
	},
	[]string{"site", "asset"},
)

// ─── hashdb drift detector ───────────────────────────────

// HashdbAppendTotal counts the indexer's per-ledger hashdb.Append (ok, error); Append never stalls ingest, so a sustained error rate is the only sign the verify sweep has gone blind.
var HashdbAppendTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_hashdb_append_total",
		Help: "Indexer hashdb.Append outcomes per ledger, labelled by outcome (ok|error). error means the drift detector is silently not recording — see the hashdb runbook.",
	},
	[]string{"outcome"},
)

// HashdbAppendDurationSeconds is per-ledger hashdb.Append latency (one O(1) WriteAt, no fsync, on the ingest hot path); buckets 10us-10ms.
var HashdbAppendDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_hashdb_append_duration_seconds",
		Help:    "Per-ledger hashdb.Append latency, labelled by outcome (ok|error). Runs synchronously on the ingest hot path — a regression here is ingest lag.",
		Buckets: []float64{0.00001, 0.00005, 0.0001, 0.0005, 0.001, 0.005, 0.01},
	},
	[]string{"outcome"},
)

// HashdbVerifyRunsTotal counts hashdb verify sweeps by outcome: ok (no drift, at least one ledger compared), drift (see HashdbDriftTotal) or error ("don't know", not a mismatch).
var HashdbVerifyRunsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_hashdb_verify_runs_total",
		Help: "Indexer periodic hashdb verify-sweep outcomes, labelled by outcome (ok|drift|error) and window (recent).",
	},
	[]string{"outcome", "window"},
)

// HashdbVerifyRunDurationSeconds is per-sweep latency by outcome; buckets 1s-30min since an S3 re-read of thousands of ledgers is not cheap.
var HashdbVerifyRunDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_hashdb_verify_run_duration_seconds",
		Help:    "Per-sweep latency of the hashdb periodic verify pass, labelled by outcome (ok|drift|error).",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200, 1800},
	},
	[]string{"outcome"},
)

// HashdbDriftTotal counts drifted LEDGERS found by verify since start; unlabelled because ledger sequence is unbounded cardinality (the log line names them).
var HashdbDriftTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_hashdb_drift_total",
		Help: "Cumulative count of ledgers hashdb's periodic verify sweep found drifted (recorded hash != freshly-observed hash). Any nonzero value means upstream history was rewritten or our lake object is corrupted — see the hashdb runbook.",
	},
)

// DEXTVLRefreshTotal counts internal/api/v1.DEXTVLCache refreshes (ok, error); sustained error with no ok means /v1/protocols TVL serves an ever-older snapshot while looking healthy.
var DEXTVLRefreshTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_dex_tvl_refresh_total",
		Help: "DEX TVL snapshot-cache refresh outcomes (ok|error). `error` means >=1 protocol carried its previous entry forward.",
	},
	[]string{"outcome"},
)

// DEXTVLRefreshDurationSeconds is per-refresh latency by outcome; buckets 50ms-180s, the top being the worker's 3-min hard stop.
var DEXTVLRefreshDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_dex_tvl_refresh_duration_seconds",
		Help:    "Per-refresh latency of the DEX TVL snapshot cache, labelled by outcome (ok|error).",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 180},
	},
	[]string{"outcome"},
)

// DEXTVLReconcileTotal counts the headline DEX TVL admission check: ok (exact sum of every part) or divergent (a protocol refused and named in `excluded`: a narrowed total, never a wrong one).
var DEXTVLReconcileTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_dex_tvl_reconcile_total",
		Help: "DEX TVL headline-total admission outcomes (ok|divergent). `divergent` means >=1 protocol was refused admission and the total excludes it.",
	},
	[]string{"outcome"},
)

// SDEXOrderBookMaintainTotal counts /v1/sdex/orderbook book maintenance by outcome (load_*, advance_*, verify_*); advance_held is a clean tick with zero cursor progress, alerted separately.
var SDEXOrderBookMaintainTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_sdex_orderbook_maintain_total",
		Help: "SDEX live order-book maintenance outcomes (load_ok|load_error|advance_ok|advance_error|advance_held|verify_ok|verify_error).",
	},
	[]string{"outcome"},
)

// SDEXOrderBookMaintainDurationSeconds is book-maintenance latency by outcome; buckets 50ms-30min (advance_* low, load_* high).
var SDEXOrderBookMaintainDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_sdex_orderbook_maintain_duration_seconds",
		Help:    "SDEX order-book maintenance latency, labelled by outcome (load_ok|load_error|advance_ok|advance_error|advance_held|verify_ok|verify_error).",
		Buckets: []float64{0.05, 0.25, 1, 5, 15, 60, 300, 900, 1800},
	},
	[]string{"outcome"},
)

// SDEXOrderBookCrossedPairs counts crossed pairs (best bid > best ask) in the served book; a resting classic book cannot cross, so each is a phantom offer and sustained non-zero is a lake coverage gap.
var SDEXOrderBookCrossedPairs = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_sdex_orderbook_crossed_pairs",
		Help: "Asset pairs whose served SDEX order book is crossed (best bid > best ask); any sustained non-zero value is phantom-offer data corruption.",
	},
)

// SDEXOrderBookPendingOffers is offers quarantined (version-tie suspects) until a lake removal probe proves them live; stuck high means served depth is thinner than real.
var SDEXOrderBookPendingOffers = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "stellarindex_sdex_orderbook_pending_offers",
		Help: "SDEX offers quarantined from the served book awaiting lake removal-verification.",
	},
)

// SDEXOrderBookUndecodableOffersTotal counts offer rows whose entry_xdr would not decode; the row is skipped, freezing that key's applied state. Should be 0.
var SDEXOrderBookUndecodableOffersTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_sdex_orderbook_undecodable_offers_total",
		Help: "Offer-entry rows skipped by the SDEX order-book reader because their entry XDR failed to decode; each skip freezes that offer key's previously-applied state.",
	},
)

// WorkerPanicsTotal counts panics recovered by worker.Recover, per worker; each increment is one dead worker until restart while the process stays up.
var WorkerPanicsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_worker_panics_total",
		Help: "Panics recovered by worker.Recover, per background worker name. Each one is a worker that is now STOPPED until the process restarts.",
	},
	[]string{"worker"},
)

// Auth-reaper liveness: the reapers bounding attacker-fillable and PII auth tables report what
// they did but not that they ran, so a dead one leaves every signal frozen at a healthy value.
// last_sweep_unix{reaper} is set after every sweep, failed ones included (only ctx-cancel skips
// it); interval_seconds{reaper} is the cadence. Alert: stellarindex_auth_reaper_stalled
// (time() - last_sweep > 3 x interval, 15m). A disabled reaper publishes no series.
const (
	AuthReaperLoginCode = "login_code"
	AuthReaperMagicLink = "magic_link"
	AuthReaperSignup    = "signup"
	// internal/retentionreaper instances share these gauges and the stalled alert.
	AuthReaperSession         = "session"
	AuthReaperWebhookDelivery = "webhook_delivery"
	AuthReaperRegistration    = "registration" // unused /v1/register accounts
)

// AuthReaperLastSweepUnix — see the auth-reaper liveness note above.
var AuthReaperLastSweepUnix = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_auth_reaper_last_sweep_unix",
		Help: "Unix time of the most recent COMPLETED sweep of each platform-table reaper (login_code|magic_link|signup|session|webhook_delivery); failed sweeps count, cancelled ones do not.",
	},
	[]string{"reaper"},
)

// RetentionReaperRowsDeletedTotal counts rows removed by internal/retentionreaper sweeps (ended sessions, finished webhook deliveries), by `reaper`.
var RetentionReaperRowsDeletedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_retention_reaper_rows_deleted_total",
		Help: "Rows deleted by the platform retention sweeps, by reaper (session|webhook_delivery).",
	},
	[]string{"reaper"},
)

// RetentionReaperRows is the current row count of each table a retentionreaper instance bounds, refreshed every sweep; reapers without a count seam stay absent rather than read 0.
var RetentionReaperRows = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_retention_reaper_rows",
		Help: "Rows in the table a platform retention sweep bounds, by reaper (webhook_delivery). Customer-configured fan-out writes it, so sustained growth means the sweep is not keeping up.",
	},
	[]string{"reaper"},
)

// RetentionReaperErrorsTotal counts failed internal/retentionreaper sweeps; pre-seeded so a never-failed reaper reads 0, not absent.
var RetentionReaperErrorsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_retention_reaper_errors_total",
		Help: "Platform retention sweep failures, by reaper (session|webhook_delivery). Non-zero = the table is not being bounded.",
	},
	[]string{"reaper"},
)

// AuthReaperIntervalSeconds — see the auth-reaper liveness note above.
var AuthReaperIntervalSeconds = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_auth_reaper_interval_seconds",
		Help: "Configured sweep cadence of each platform-table reaper, in seconds (the stalled alert's threshold is 3x this).",
	},
	[]string{"reaper"},
)

// ExplorerSWRRefreshTotal counts the explorer's detached stale-while-revalidate refreshes per cache and outcome; SWR hides failures at the API, so a sustained error rate on one cache is the only early sign.
var ExplorerSWRRefreshTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_explorer_swr_refresh_total",
		Help: "Explorer stale-while-revalidate detached refresh outcomes per cache (accounts_wealth|asset_holders|contract_detail|contracts_dir|native_lp_listing|network_throughput|op_type_stats|ops_directory|protocol_bespoke|ttl_liveness × ok|error).",
	},
	[]string{"cache", "outcome"},
)

// ExplorerRefreshGateSaturatedTotal counts detached refreshes clickhouse.RefreshGate REFUSED, by class and bound; separates a key-churn burst from real capacity pressure.
var ExplorerRefreshGateSaturatedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_explorer_refresh_gate_saturated_total",
		Help: "Detached explorer cache refreshes skipped because the shared refresh gate was saturated, by refresh class and the bound that refused (class|global).",
	},
	[]string{"class", "bound"},
)

// ExplorerSWRRefreshDurationSeconds is per-refresh latency by cache and outcome; buckets 50ms-300s (the widest refresh timeout).
var ExplorerSWRRefreshDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_explorer_swr_refresh_duration_seconds",
		Help:    "Explorer stale-while-revalidate detached refresh latency, labelled by cache and outcome.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
	},
	[]string{"cache", "outcome"},
)

// CHSchemaProbePresent is the lake's last answer to each explorer schema probe (1 usable, 0 absent or empty); an absent answer latches until restart. Unset until the first answer.
var CHSchemaProbePresent = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "stellarindex_ch_schema_probe_present",
		Help: "Last ClickHouse answer to each explorer schema probe (1 = object usable, 0 = absent or empty; the explorer serves that surface from its fallback while 0).",
	},
	[]string{"probe"},
)

// ExplorerSchemaAbsentEmptyTotal counts explorer reads served as empty because their ClickHouse table or column is missing.
var ExplorerSchemaAbsentEmptyTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_explorer_schema_absent_empty_total",
		Help: "Explorer reads answered as empty because their ClickHouse table or column does not exist, by table.",
	},
	[]string{"table"},
)

// CHSchemaProbeUnansweredTotal counts explorer schema probes that got no answer (transport error, deadline); leaves [CHSchemaProbePresent] untouched.
var CHSchemaProbeUnansweredTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_ch_schema_probe_unanswered_total",
		Help: "Explorer ClickHouse schema probes that received no schema answer (transport error, deadline, or non-schema exception), by probe.",
	},
	[]string{"probe"},
)

// ProtocolDetailRefreshTotal counts detached /v1/protocols/{name} detail rebuilds by outcome (ok, stale, degraded, timeout); sustained degraded/timeout means pages serve without analytics.
var ProtocolDetailRefreshTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_protocol_detail_refresh_total",
		Help: "Detached protocol-detail cache rebuild outcomes (ok|stale|degraded|timeout), covering the prewarm sweep and request-kicked stale revalidations.",
	},
	[]string{"outcome"},
)

// ProtocolDetailRefreshDurationSeconds is per-rebuild latency by outcome; buckets 50ms-90s, the top being the hard budget (protocolDetailRefreshTimeout).
var ProtocolDetailRefreshDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "stellarindex_protocol_detail_refresh_duration_seconds",
		Help:    "Detached protocol-detail rebuild latency, labelled by outcome (ok|stale|degraded|timeout).",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 90},
	},
	[]string{"outcome"},
)

// ObserveExplorerSWRRefresh records one detached SWR refresh attempt against [ExplorerSWRRefreshTotal] and its duration histogram, shared so outcome derivation stays identical across refreshers.
func ObserveExplorerSWRRefresh(cache string, start time.Time, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	ExplorerSWRRefreshTotal.WithLabelValues(cache, outcome).Inc()
	ExplorerSWRRefreshDurationSeconds.WithLabelValues(cache, outcome).Observe(time.Since(start).Seconds())
}

// APISparkline7dRowsTotal: alert when the empty share exceeds 0.5 for 30 min, which means a lookup-key or pricing-pipeline regression, not a quiet market.
var APISparkline7dRowsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_api_sparkline7d_rows_total",
		Help: "Priced listing rows for which a 7d sparkline was requested, by result (served|empty).",
	},
	[]string{"result"},
)

// APICoverageFloorProbesTotal counts coverage-floor memo consultations by result (hit, found, absent, error, evicted); anonymously reachable, so non-hit rate is added DB load.
var APICoverageFloorProbesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_api_coverage_floor_probes_total",
		Help: "Coverage-floor lookups behind the API's outside-coverage signal, by outcome (hit|found|absent|error|evicted); only non-hit results reach the database.",
	},
	[]string{"result"},
)

// APILCMHomeDomainFallbackTotal counts failed home-domain observation reads, each served as "unobserved" so the next ADR-0021 layer answered.
var APILCMHomeDomainFallbackTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "stellarindex_api_lcm_home_domain_fallback_total",
		Help: "Home-domain observation reads that failed (storage error or timeout) and were served as unobserved, falling through to the next home-domain layer.",
	},
)
