// Package redispub carries the aggregator's closed-bucket events to the
// API binary's in-process [streaming.Hub] over Redis pub/sub: [Publisher]
// implements orchestrator.StreamPublisher and [Subscriber] republishes
// each [ClosedBucketEvent] (one per pair and window, JSON, on
// [DefaultChannel] by default) on the local Hub.
//
// Pub/sub is fire-and-forget; an event published while a subscriber is
// down is lost. That is acceptable because the VWAP cache and the trades
// table are the durable record; SSE clients replay through the Hub's
// per-topic ring with Last-Event-ID.
package redispub
