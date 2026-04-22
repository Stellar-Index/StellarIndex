// Package aggregate computes VWAP / TWAP / OHLC and runs outlier
// filtering over a window of [canonical.Trade] values.
//
// # Scope
//
// This package is pure functions over in-memory slices. Persistence
// (TimescaleDB continuous aggregates), scheduling (the aggregator
// binary), and multi-source reconciliation (the divergence layer)
// live elsewhere. Here we only do the math.
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
// Phase-1 design settled on a sigma-threshold filter (default 4σ) —
// drop any trade whose price is more than N standard deviations
// from the unweighted mean. Over small windows with fat tails this
// is less defensible than a MAD-based filter, but it's what ADR /
// RFP cite. See [ADR TBD] for the filter's full rationale.
//
// # What this package deliberately doesn't do
//
//   - No time-windowing. Callers pre-filter trades to the window
//     they want before passing in.
//   - No multi-venue weighting / triangulation. Those happen
//     upstream in [internal/divergence].
package aggregate
