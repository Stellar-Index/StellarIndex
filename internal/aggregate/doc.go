// Package aggregate computes VWAP / TWAP / OHLC, runs outlier
// filtering, and applies stablecoin → fiat proxy mapping over a
// window of [canonical.Trade] values.
//
// # Scope
//
// This package is pure functions over in-memory slices. Persistence
// (TimescaleDB continuous aggregates), scheduling (the aggregator
// binary's [internal/aggregate/orchestrator]), and multi-source
// divergence detection live elsewhere. Here we only do the math.
//
// See docs/architecture/aggregation-plan.md for how the orchestrator
// composes these primitives into the policy chain (stablecoin
// expansion → class filter → outlier filter → VWAP).
//
// # VWAP
//
// Volume-Weighted Average Price: the canonical summary of "what did
// this asset trade at" over a window. Standard definition weights
// each trade's price by the base-asset volume it moved:
//
//	VWAP = Σ(price_i × volume_i) / Σ(volume_i)
//
// With price_i = Qi/Bi and volume_i = Bi (base), that collapses to:
//
//	VWAP = Σ(Qi) / Σ(Bi)
//
// — total quote moved divided by total base moved. This is exact
// arithmetic on [*big.Rat], never float. Caller chooses
// display-decimals when they render it.
//
// # Outliers
//
// The filter drops any trade whose price deviates from the window
// MEDIAN by more than σ × 1.4826 × MAD (median absolute deviation),
// σ defaulting to 4. Exact rational arithmetic throughout.
//
// Corrected 2026-08-04: this block used to describe a sigma-threshold
// filter around the unweighted MEAN and said "the σ form is what the
// methodology specifies". The median/MAD form shipped with the M5 fix;
// the methodology page has now been corrected to match too. Note the
// band is ADDITIVE in price space, so its lower edge goes non-positive
// above 1/(σ×1.4826) relative MAD — 16.9% at σ=4 — and downward
// outliers stop being rejectable there. On-call guidance lives in
// docs/operations/runbooks/aggregator-outlier-storm.md.
//
// # Stablecoin fiat proxy
//
// Quote-side stablecoin tickers map to their pegged fiat at
// VWAP-compute time, never at decode time — see
// [FiatProxy] / [ProxyPair] / [ProxyTrade] /
// [ExpandTargetPair]. Decoders preserve the raw pair so a depeg
// stays visible in the trade feed; the aggregator applies the
// rewrite when an operator opts in via
// orchestrator.Config.EnableStablecoinFiatProxy.
//
// # Triangulation
//
// Cross-pair chains (XLM/USD × USD/EUR = XLM/EUR) are priced by the
// graph router — [BuildEdges] builds the edge set from the
// freshly-cached leg VWAPs and [CombineRoutes]/[CompositeRate]
// enumerates and composites the best route — with the X2.5 forex-snap
// rule for chained-fiat pairs (per F-0014). The orchestrator's
// [Triangulations] field drives a per-tick pass after direct-pair
// refreshes have populated the leg cache. ([Triangulate] /
// [TriangulateChain] are the older direct-multiply helpers, kept for
// reference/tests but NOT on the serving path — the router is.)
//
// # What this package deliberately doesn't do
//
//   - No time-windowing. Callers pre-filter trades to the window
//     they want before passing in.
//   - No multi-venue weighting. Per-source weight overrides are
//     deferred per docs/architecture/aggregation-plan.md
//     §Deferred — every contributing source weights at 100 today.
package aggregate
