// Package anomaly is ADR-0019 Phase 1: per-asset-class threshold freezes,
// running beside Phase 2 (internal/aggregate/baseline and confidence).
// Either phase can start a freeze on its own and both share one freeze
// lifecycle; once frozen, Phase 1 stands down and Phase 2 decides release.
//
// Before publishing a VWAP the aggregator calls [Checker.Evaluate] with the
// previous non-empty closed minute's VWAP, the largest closed-minute move
// since the last decision, and the source count. Thresholds are calibrated
// on minute-on-minute moves at every window length; scoring the
// overlapping rolling window instead damps a move by bucket/window and
// leaves freeze_pct unreachable at 1h and 24h.
//
// Freeze needs BOTH deviation >= freeze_pct AND at most one source:
// multi-source agreement is its own corroboration, so a large
// multi-source move only warns. [ActionWarn] counts operator-side and sets
// no wire flag; [ActionFreeze] serves the caller's last-known-good with
// `flags.frozen`. Unclassified assets fall to [ClassDefault].
//
// [ADR-0019]: ../../../docs/adr/0019-anomaly-response-and-confidence-scoring.md
package anomaly
