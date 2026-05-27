# Metric Name Inventory

Generated 2026-05-26T21:43:29Z.

Metric names registered in `internal/obs/metrics.go`:

```
var HTTPRequestsTotal = prometheus.NewCounterVec(
var HTTPRequestDuration = prometheus.NewHistogramVec(
var APICacheOpsTotal = prometheus.NewCounterVec(
var SourceEventsTotal = prometheus.NewCounterVec(
var SourceLagLedgers = prometheus.NewGaugeVec(
var SourceLastEventUnix = prometheus.NewGaugeVec(
var SourceEnabled = prometheus.NewGaugeVec(
var SourceMatchedEventsTotal = prometheus.NewCounterVec(
var SourceDecodeErrorsTotal = prometheus.NewCounterVec(
var SourceUnknownSymbolsTotal = prometheus.NewCounterVec(
var ExternalPollerPollsTotal = prometheus.NewCounterVec(
var ExternalPollerLastSuccessUnix = prometheus.NewGaugeVec(
var SourceOrphanEventsTotal = prometheus.NewCounterVec(
var Sep1CacheOpsTotal = prometheus.NewCounterVec(
var SourceInsertErrorsTotal = prometheus.NewCounterVec(
var CursorLastLedger = prometheus.NewGaugeVec(
var DivergenceRefreshTotal = prometheus.NewCounterVec(
var DivergenceRefreshDurationSeconds = prometheus.NewHistogramVec(
var TradeInsertsTotal = prometheus.NewCounterVec(
var StreamPublishTotal = prometheus.NewCounterVec(
var PriceStalenessSeconds = prometheus.NewGaugeVec(
var OracleLastUpdateUnix = prometheus.NewGaugeVec(
var OracleResolutionSeconds = prometheus.NewGaugeVec(
var AggregatorTicksTotal = prometheus.NewCounterVec(
var AggregatorStreamPublishTotal = prometheus.NewCounterVec(
var APIStreamSubscribeTotal = prometheus.NewCounterVec(
var CustomerWebhookDeliveryAttemptsTotal = prometheus.NewCounterVec(
var CustomerWebhookDeliveryDurationSeconds = prometheus.NewHistogramVec(
var APICORSDecisionsTotal = prometheus.NewCounterVec(
var AggregatorDroppedTradesTotal = prometheus.NewCounterVec(
var AggregatorDroppedWindowsTotal = prometheus.NewCounterVec(
var SupplyCrossCheckDivergenceStroops = prometheus.NewGaugeVec(
var SupplyCrossCheckTotal = prometheus.NewCounterVec(
var AnomalyFreezeEngagedTotal = prometheus.NewCounterVec(
var AnomalyFreezeRecoverySweepsTotal = prometheus.NewCounterVec(
var AnomalyFreezeRecoverySweepDurationSeconds = prometheus.NewHistogramVec(
var AggregatorTriangulationsTotal = prometheus.NewCounterVec(
var AggregatorFXSnapFallbackTotal = prometheus.NewCounterVec(
var AggregatorBaselineRefreshTotal = prometheus.NewCounterVec(
var AggregatorSupplyRefreshTotal = prometheus.NewCounterVec(
var AggregatorSupplyRefreshDurationSeconds = prometheus.NewHistogramVec(
var AggregatorConfidenceComputeTotal = prometheus.NewCounterVec(
var VerifyArchiveLedgersVerified = prometheus.NewCounterVec(
var VerifyArchiveCurrentLedger = prometheus.NewGaugeVec(
var VerifyArchiveCheckpointsTotal = prometheus.NewCounterVec(
var VerifyArchiveMismatchesTotal = prometheus.NewCounterVec(
var StripePlatformSyncErrorsTotal = prometheus.NewCounterVec(
...
```

Auditor cross-references against:
- `docs/reference/metrics/README.md`
- `deploy/monitoring/rules/*.yml` rule expressions
- `configs/prometheus/rules.r1/*.yml` rule expressions
