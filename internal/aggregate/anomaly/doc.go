// Package anomaly implements the Phase-1 component of [ADR-0019] —
// per-asset-class threshold-based anomaly detection. It runs alongside
// Phase 2 (per-asset MAD baselines + multi-factor confidence) rather
// than being superseded by it: either layer can start a freeze on its own
// (Phase 1 via the class-threshold AND below, Phase 2 via the
// [Phase2FreezeConfig] AND-of-three-signals rule), and both share one
// freeze lifecycle.
//
// # Scope
//
// Phase 1 is an operator-set TOML threshold table per asset class.
// It is deliberately crude:
//
//   - One pair of (warn_pct, freeze_pct) thresholds per asset class
//   - Decision is a function of (asset class, prev VWAP, curr VWAP, source count)
//   - No statistical baseline; no z-score; no confidence score
//
// Phase 2, shipped, lives at [internal/aggregate/baseline]
// (per-asset MAD baselines + z-score) and
// [internal/aggregate/confidence] (seven-factor weighted-geomean
// confidence). The aggregator orchestrator wires both — Phase 1 here
// freezes on "this movement is large for this asset class AND has at
// most one source" while Phase 2 freezes on "this movement is
// statistically anomalous AND under-confident AND under-corroborated".
// The two are independent triggers, not a joint vote: a Phase 1 fire
// freezes without a Phase 2 signal. Once a window is frozen, Phase 1
// stands down and the Phase 2 lifecycle alone decides release.
//
// # The decision algorithm
//
// Before publishing a VWAP the aggregator calls [Checker.Evaluate]
// with:
//
//   - the comparand: the previous non-empty closed one-minute bucket's
//     VWAP (the last published VWAP only when no earlier minute is known)
//   - a closed one-minute bucket's VWAP — the largest move among the
//     minutes closed since the previous decision
//   - how many sources contributed
//
// The thresholds are calibrated per closed one-minute bucket: freeze_pct
// and warn_pct are the size of a move between two consecutive,
// non-overlapping minute buckets. [internal/aggregate/orchestrator]
// scores that minute-on-minute move at every window length (5m, 1h,
// 24h), never the tick-to-tick change of the overlapping rolling window,
// which would damp a move by bucket/window and leave freeze_pct
// unreachable at 1h and 24h. Phase 2's z-score scores the same returns,
// the grain its MAD baseline is trained on ([internal/aggregate/baseline]).
//
// Evaluate returns a [Decision] with one of three actions:
//
//   - [ActionAllow]  — publish normally
//   - [ActionWarn]   — publish, and count it in obs.AnomalyWarnTotal
//     (operator-side only; it does NOT set flags.divergence_warning —
//     see [ActionWarn])
//   - [ActionFreeze] — DO NOT publish; serve the previous bucket's
//     LKG with `flags.frozen: true` (caller's
//     responsibility to maintain the LKG slot)
//
// The Phase-1 freeze condition is the AND of two signals:
//
//	deviation_pct >= thresholds[class].freeze_pct
//	source_count <= 1
//
// Both must trip. A 100x movement on a multi-source asset (real
// flash crash, news event) gets WARN, not FREEZE — because
// multi-source agreement provides its own corroboration.
//
// # Asset classification
//
// Operator config maps each asset to a class via
// `[anomaly.classifications]`. Anything not explicitly
// classified falls through to [ClassDefault] with conservative
// thresholds.
//
// Per-asset behaviour layers on top via Phase 2's
// [internal/aggregate/baseline] (volatility profile observed from
// the `volatility_baseline_1m` CAGG); the per-class table here
// remains operator-curated as a coarse safety net for assets
// without enough trades to build a baseline.
//
// # Why this lives separate from internal/aggregate
//
// The aggregate package computes VWAP/TWAP from raw trade slices;
// it doesn't know about wire policy. The anomaly package consumes
// that output and decides whether to publish it. Keeping them
// separate lets Phase 1's class thresholds and Phase 2's
// per-asset baselines + confidence ([internal/aggregate/baseline],
// [internal/aggregate/confidence]) layer cleanly on top of an
// untouched math layer.
//
// [ADR-0019]: ../../../docs/adr/0019-anomaly-response-and-confidence-scoring.md
package anomaly
