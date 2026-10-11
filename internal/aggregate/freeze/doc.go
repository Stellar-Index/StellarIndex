// Package freeze records anomaly freezes (ADR-0019) as `freeze:<asset>:<quote>` Redis markers that [Looker] reads for flags.frozen.
// Duration is lifecycle state, not a TTL: [Policy] holds, extends up to 4 times, escalates, and releases only on confidence > 0.30 AND z < 3.0
// for two consecutive buckets. The marker TTL is remaining hold plus grace, a backstop only; [Policy.Evaluate] is pure.
package freeze
