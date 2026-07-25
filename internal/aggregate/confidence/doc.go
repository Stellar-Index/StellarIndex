// Package confidence implements the multi-factor confidence score
// per ADR-0019 §"Multi-factor confidence score".
//
// Each price published by the aggregator carries a `confidence ∈ [0, 1]`
// value computed by combining the factors below via weighted
// geometric mean:
//
//   - [ZScoreFactor]: how anomalous the bucket's return is vs the
//     per-asset statistical baseline (ADR-0019 Phase 2; see
//     [internal/aggregate/baseline]).
//   - [SourceCountFactor]: how many distinct sources contributed.
//   - [DiversityFactor]: did multiple source CLASSES contribute (CEX
//     vs DEX vs oracle) — orthogonal to count.
//   - [LiquidityFactor]: USD volume in the bucket.
//   - [CrossOracleFactor]: agreement with external reference oracles
//     (Reflector, Chainlink, etc.).
//   - [TriangulationAgreementFactor]: agreement between the pair's
//     direct price and the composite implied by a configured
//     triangulation chain. Half-weight by default and weightless when
//     no composite exists, so it is inert for pairs without a chain —
//     ADR-0019 predates it; see that ADR's 2026-07-25 amendment.
//   - [BaselineQualityFactor]: how mature the per-asset baseline is.
//
// The combiner is the NORMALISED weighted geometric mean —
// `prod(factor_i ^ weight_i) ^ (1 / sum(weights))`, implemented in
// [Compute]. The `^ (1 / sum(weights))` exponent is load-bearing and
// not optional decoration:
//
//   - It keeps weights RELATIVE. Without it, doubling every weight
//     squares the score, so an operator who raises one weight to
//     emphasise a factor silently craters every published score.
//   - It keeps the documented neutral values neutral. The
//     "no cross-oracle data" factor is 0.7 by design (ADR-0019); as a
//     bare product that single term would cap every score without an
//     external reference at 0.7, which is a penalty, not neutrality.
//   - It keeps the served [0, 1] value interpretable for consumers
//     gating on `confidence`, and keeps the ADR's bootstrap cap of
//     0.5 a real ceiling rather than a number ordinary buckets never
//     reach anyway.
//
// ADR-0019's formula block writes the product WITHOUT the exponent
// while its prose says "weighted geometric mean" (which is the
// normalised form by definition). The code is authoritative; see the
// R-003 amendment at the top of
// docs/adr/0019-anomaly-response-and-confidence-scoring.md for the
// full reasoning and for what the 0.10 freeze threshold means on this
// scale.
//
// The shape is right because it gives DOMINATING-FACTOR behaviour:
// any one near-zero factor pulls the whole score toward zero (and an
// exact zero takes it to zero outright). That matches the operational
// intent — a single missing signal is enough to down-rank the score.
//
// Each factor returns a value strictly in [0, 1]; this package
// guards inputs against NaN/Inf and clamps outputs at the bounds so
// the geometric mean never produces NaN.
//
// The wire response carries `confidence` plus its raw [Factors]
// decomposition so customers and operators can see WHY confidence
// dropped (per the ADR's worked example). [Factors] is the
// debug-friendly view; [Score] is the combined float.
package confidence
