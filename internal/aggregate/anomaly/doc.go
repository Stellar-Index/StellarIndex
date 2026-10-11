// Package anomaly is [ADR-0019] Phase 1: per-asset-class threshold freezes, evaluated by [Checker.Evaluate] before a VWAP is published.
// Thresholds are calibrated on minute-on-minute moves (an overlapping rolling window damps a move by bucket/window); freeze needs BOTH deviation >= freeze_pct AND at most one source.
// Unclassified assets fall to [ClassDefault].
// [ADR-0019]: ../../../docs/adr/0019-anomaly-response-and-confidence-scoring.md
package anomaly
