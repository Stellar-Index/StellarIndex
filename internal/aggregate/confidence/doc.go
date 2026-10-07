// Package confidence computes the [0, 1] confidence carried by every
// published price (ADR-0019) from its factors: [ZScoreFactor],
// [SourceCountFactor], [DiversityFactor], [LiquidityFactor],
// [CrossOracleFactor], [TriangulationAgreementFactor] (half weight, inert
// without a composite) and [BaselineQualityFactor].
//
// [Compute] combines them as the NORMALISED weighted geometric mean,
// `prod(factor_i ^ weight_i) ^ (1 / sum(weights))`. The outer exponent
// keeps weights relative (doubling them all must not square the score)
// and keeps neutral values neutral (a bare product would cap every
// score with no cross-oracle data at its neutral 0.7). Any factor near
// zero still drags the score toward zero, which is intended.
//
// Factors are NaN/Inf-guarded and clamped to [0, 1]. The response
// carries [Factors] beside the [Score] so a drop can be explained;
// docs/architecture/oracle-manipulation-defense.md has the constants.
package confidence
