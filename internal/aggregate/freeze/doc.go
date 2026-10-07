// Package freeze records anomaly freezes (ADR-0019). When the anomaly
// checker returns ActionFreeze, the aggregator calls [Writer.Mark] to set
// `freeze:<asset>:<quote>` in Redis; [Looker] reads it so /v1/price can
// set flags.frozen. The marker is separate from the price cache so the
// price reader stays agnostic of anomaly state.
//
// Freeze duration is state, not a TTL. [Policy] holds a freeze for 30
// minutes ([DefaultUncorroboratedInitialHold] for a pair with no
// corroborating lens), extends it by 30 minutes up to 4 times, then holds
// it for operator review. Release needs confidence > 0.30 AND z < 3.0 for
// two consecutive buckets, not merely the trigger going quiet; one
// lucky bucket must not publish the price the freeze refused.
//
// The marker's TTL is the remaining hold plus a grace: a backstop for an
// aggregator outage, never the policy. [Policy.Evaluate] is pure; the
// orchestrator owns persistence, marker IO, metrics and logs.
package freeze
