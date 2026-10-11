// Package confidence computes the [0, 1] confidence of every published price (ADR-0019) from [ZScoreFactor], [SourceCountFactor], [DiversityFactor], [LiquidityFactor], [CrossOracleFactor], [TriangulationAgreementFactor] and [BaselineQualityFactor].
// [Compute] is the NORMALISED weighted geometric mean, `prod(factor_i ^ weight_i) ^ (1 / sum(weights))`: the outer exponent keeps weights relative and neutral values neutral. A bare product would cap no-cross-oracle scores at 0.7.
// Factors are NaN/Inf-guarded and clamped to [0, 1]; constants in docs/architecture/oracle-manipulation-defense.md.
package confidence
