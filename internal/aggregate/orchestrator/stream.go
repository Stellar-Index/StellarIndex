package orchestrator

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// streamBucketOnce publishes a served closed bucket to the stream the
// first time any writer serves it. The SSE contract (ADR-0015, openapi
// price/stream) promises byte-identical payloads across subscribers and
// regions on the same (asset, quote, window), so the event is stamped
// with the bucket it covers, not the tick's jittered clock, and a
// replaying tick does not re-emit it.
func (o *Orchestrator) streamBucketOnce(
	ctx context.Context, pair canonical.Pair, window time.Duration, value string, bucketEnd time.Time,
	coverage *cachekeys.WindowCoverage,
) {
	if !o.claimStreamBucket(pair, window, bucketEnd) {
		return
	}
	o.publishToStream(ctx, pair, window, value, bucketEnd, coverage)
}

// streamFrozenOnce puts a refused bucket on the stream in place of its
// value, once per bucket like [Orchestrator.streamBucketOnce]: without it
// a frozen series is indistinguishable from a quiet market. firedAt is the
// freeze's fire time; the bucket it truncates to is the first one refused.
func (o *Orchestrator) streamFrozenOnce(
	ctx context.Context, pair canonical.Pair, window time.Duration, bucketEnd, firedAt time.Time,
) {
	if o.cfg.StreamPublisher == nil || !o.claimStreamBucket(pair, window, bucketEnd) {
		return
	}
	var frozenSince time.Time
	if !firedAt.IsZero() {
		frozenSince = firedAt.Truncate(closedBucket)
	}
	if err := o.cfg.StreamPublisher.PublishFrozenBucket(ctx, pair, window, bucketEnd, frozenSince); err != nil {
		obs.AggregatorStreamPublishTotal.WithLabelValues("error").Inc()
		o.logger.Warn("stream publish of frozen bucket failed",
			"pair", pair.String(), "window", window, "err", err)
		return
	}
	obs.AggregatorStreamPublishTotal.WithLabelValues("ok").Inc()
}

// claimStreamBucket records bucketEnd as streamed for (pair, window),
// reporting false when it already was: one stream event per bucket.
func (o *Orchestrator) claimStreamBucket(pair canonical.Pair, window time.Duration, bucketEnd time.Time) bool {
	k := pair.String() + ":" + window.String()
	if last, ok := o.streamedBuckets[k]; ok && last.Equal(bucketEnd) {
		return false
	}
	if o.streamedBuckets == nil {
		o.streamedBuckets = make(map[string]time.Time)
	}
	o.streamedBuckets[k] = bucketEnd
	return true
}

// publishToStream fans the closed-bucket event out to the
// configured StreamPublisher (Redis pub/sub in production). Pure
// best-effort: never returns an error — failures log + increment
// the per-outcome counter. The VWAP cache write upstream is the
// source of truth; the stream is enrichment for SSE subscribers.
func (o *Orchestrator) publishToStream(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	value string,
	observedAt time.Time,
	coverage *cachekeys.WindowCoverage,
) {
	if o.cfg.StreamPublisher == nil {
		return
	}
	if err := o.cfg.StreamPublisher.PublishClosedBucket(ctx, pair, window, value, observedAt, coverage); err != nil {
		obs.AggregatorStreamPublishTotal.WithLabelValues("error").Inc()
		o.logger.Warn("stream publish failed",
			"pair", pair.String(), "window", window, "err", err)
		return
	}
	obs.AggregatorStreamPublishTotal.WithLabelValues("ok").Inc()
}
