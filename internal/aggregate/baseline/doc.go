// Package baseline is the pure robust statistics behind ADR-0019 Phase 2 volatility baselines ([Median], [MAD], [Baseline], [ReturnsFromVWAPs]); storage lives elsewhere so it stays fuzzable.
// MAD replaces standard deviation because one attack in the training window inflates σ and hides the next; it is scaled by 1.4826 so "5σ" keeps its usual meaning.
package baseline
