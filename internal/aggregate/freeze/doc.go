// Package freeze writes + reads anomaly-freeze markers per ADR-0019.
//
// When the anomaly checker (internal/aggregate/anomaly) returns
// ActionFreeze for a (asset, quote) bucket close, the aggregator
// calls [Writer.Mark] to record the freeze in Redis at the
// `freeze:<asset>:<quote>` key. The API's /v1/price handler reads
// the same key via [Looker] (which implements
// internal/api/v1.FrozenLooker) and surfaces flags.frozen=true on
// responses for the affected pair.
//
// Why a separate Redis key (vs embedding the freeze state in the
// price-cache value): the freeze marker has different TTL semantics
// than the price itself, and the API hot path reads the marker on
// every /v1/price response — separating the keys lets the price
// reader stay agnostic of anomaly state.
//
// # Freeze duration is state, not a TTL
//
// [Policy] implements ADR-0019 §"Freeze duration": a 30-minute
// minimum hold (10 minutes for a pair with no corroborating lens —
// see [DefaultUncorroboratedInitialHold]), re-evaluated at expiry,
// extended by 30 minutes up to 4 times, then escalated to operator
// review and held until a manual unfreeze. Release requires the ADR's
// auto-unfreeze condition — confidence above 0.30 AND z below 3.0 for
// two CONSECUTIVE buckets — not merely the absence of the condition
// that fired the freeze.
//
// The Redis marker's TTL is the remaining hold plus a silence grace,
// so it is a liveness backstop ("how long a freeze survives an
// aggregator outage"), never the duration policy itself. Before the
// lifecycle landed, the 5-minute TTL WAS the duration, and a freeze
// therefore ended one bucket after the 3-signal AND stopped firing —
// so a single bucket with a second source, or with z nudged from 5.1
// to 4.9, published the price the freeze had just refused.
//
// [Policy.Evaluate] is a pure function of (previous state, this
// bucket's signal); the orchestrator owns persistence, marker IO,
// metrics and logs. Writer and Looker remain thin adapters around a
// Redis client + the cache-key builders in internal/cachekeys.
package freeze
