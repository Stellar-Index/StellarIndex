// Package aggregate is the pure price math over in-memory
// [canonical.Trade] slices: VWAP, TWAP, OHLC, outlier filtering,
// stablecoin fiat proxying and route compositing. Callers pre-filter to
// the window; persistence and scheduling live elsewhere
// (docs/architecture/aggregation-plan.md has the policy chain).
//
// VWAP is Σquote/Σbase in exact [*big.Rat]; never float.
//
// The outlier filter rejects prices beyond σ × 1.4826 × MAD of the window
// median, measured in RATIO space so a ½× print is as outlying as a 2×
// one (ADR-0046 §1). An additive band's lower edge goes below zero above
// 1/(σ×1.4826) relative MAD (16.9% at σ=4), after which downward prints
// cannot be rejected; docs/methodology/vwap-aggregation.md has the band.
//
// Stablecoin tickers map to their pegged fiat at compute time, never at
// decode, so a depeg stays visible in the trade feed ([FiatProxy]).
// Cross-pair prices come from the graph router ([BuildEdges],
// [CombineRoutes], [CompositeRate]); [Triangulate] and [TriangulateChain]
// are not on the serving path.
package aggregate
