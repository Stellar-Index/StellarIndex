// Package baseline is the pure robust statistics behind the per-asset
// volatility baselines of ADR-0019 Phase 2: [Median], [MAD], [Baseline]
// with [Baseline.ZScore], and [ReturnsFromVWAPs], which turns timed
// bucket VWAPs into returns scaled to one minute. Storage and
// orchestration live elsewhere so this stays fuzzable without a
// database.
//
// MAD replaces standard deviation because one attack in the training
// window inflates σ and hides the next attack; medians resist that. MAD
// is scaled by 1.4826 so it equals σ for normal data and "5σ" keeps its
// usual meaning.
package baseline
