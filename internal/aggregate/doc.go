// Package aggregate is the pure price math over in-memory trade slices: VWAP (exact big.Rat, never float), TWAP, OHLC,
// ratio-space MAD outlier filtering (ADR-0046 §1; docs/methodology/vwap-aggregation.md), stablecoin fiat proxying at
// compute time, never at decode so a depeg stays visible, and the cross-rate router. Callers pre-filter to the window.
package aggregate
