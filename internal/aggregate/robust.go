package aggregate

import (
	"math/big"
	"sort"
)

// Shared robust-statistics primitives for the served-price guards.
//
// All three served-price robustness guards — the published-VWAP
// outlier filter ([FilterOutliers], M5), the global aggregator-tier
// divergence filter ([rejectAggregatorOutliers], M8), and the
// serve-time sanity band ([GuardServedVWAP], M11) — share ONE
// masking-resistant, exact-rational definition of "robust centre and
// spread": the median and the 1.4826-scaled median-absolute-deviation.
//
// Median + MAD is preferred over mean + standard-deviation because
// mean/σ is masking-vulnerable (a few extreme values inflate σ so the
// outliers escape their own rejection) and degenerate on small
// windows. Everything here is exact *big.Rat (ADR-0003): no float64
// rounding ever enters the value path.

// madToStd is 1.4826 = 1/Φ⁻¹(0.75), the constant that rescales a
// median-absolute-deviation to a standard-deviation equivalent for
// normally-distributed data (so a "K·scale" band reads as "K·σ"). Held
// as the exact rational 7413/5000 to keep the guards on the exact
// *big.Rat value path.
var madToStd = big.NewRat(7413, 5000)

// medianRat returns the exact median of vals (which must be non-empty)
// as a fresh *big.Rat. Does not mutate its input. Even-length inputs
// return the exact arithmetic mean of the two middle values.
//
// Empty input returns nil (defensive, L5): every caller here guards
// non-empty, so this only ever fires if that invariant is violated —
// returning nil rather than panicking with an index-out-of-range on the
// served-price path (s[n/2] on a zero-length slice).
func medianRat(vals []*big.Rat) *big.Rat {
	if len(vals) == 0 {
		return nil
	}
	s := make([]*big.Rat, len(vals))
	copy(s, vals)
	sort.Slice(s, func(i, j int) bool { return s[i].Cmp(s[j]) < 0 })
	n := len(s)
	if n%2 == 1 {
		return new(big.Rat).Set(s[n/2])
	}
	sum := new(big.Rat).Add(s[n/2-1], s[n/2])
	return sum.Quo(sum, big.NewRat(2, 1))
}

// medianMemberRat returns a median-like centre that is ALWAYS one of the
// input values — never an averaged midpoint of two disagreeing values. For
// an odd count it is the exact median; for an even count it is the LOWER of
// the two central values (the conservative, fail-low choice for a served
// price). Does not mutate its input.
//
// Used for the SERVED cross-rate composite (see [highestConfidencePrice]) so
// the published price is one a route ACTUALLY produced, not a blend of two
// disagreeing co-equal routes — the bimodal/co-equal case where an averaging
// median would serve an unproduced midpoint. When the central values
// are equal (agreeing routes) it coincides with medianRat exactly, so the
// only observable difference is precisely the disagreement case it exists to
// fix. Empty input returns nil (defensive; callers guard non-empty — L5).
func medianMemberRat(vals []*big.Rat) *big.Rat {
	if len(vals) == 0 {
		return nil
	}
	s := make([]*big.Rat, len(vals))
	copy(s, vals)
	sort.Slice(s, func(i, j int) bool { return s[i].Cmp(s[j]) < 0 })
	return new(big.Rat).Set(s[(len(s)-1)/2])
}

// madRat returns the exact (unscaled) median absolute deviation of
// vals about centre.
func madRat(vals []*big.Rat, centre *big.Rat) *big.Rat {
	devs := make([]*big.Rat, len(vals))
	for i, v := range vals {
		d := new(big.Rat).Sub(v, centre)
		devs[i] = d.Abs(d)
	}
	return medianRat(devs)
}

// zeroScaleRelFloor is the fraction of the centre used as the robust
// scale when the measured MAD is 0.
//
// MAD is 0 whenever a strict MAJORITY of vals sit at one exact price (fills
// against one resting order, a pegged pair). The sigma band then collapses to
// the point `centre` and the filter dropped EVERY honestly-differing print. A
// relative floor keeps the outlier rejection while letting real price
// discovery through. Mirrors [robustBand] in served_guard.go.
//
// 1/200 = 0.5% of the centre. At the default sigma of 4 that is a +-2% band:
// wider than honest fill spread, far tighter than fat-finger / wash prints (a
// 2x print is still dropped).
var zeroScaleRelFloor = big.NewRat(1, 200)

// robustCentreScale returns the robust centre (median) and the
// σ-equivalent scale (madToStd · MAD) of vals, all exact. vals is
// expected non-empty (every caller today guards it); an empty vals
// returns nil, nil rather than dereferencing the nil [medianRat]
// result, the same defensive posture medianRat itself takes (L5).
//
// MAD is 0 exactly when a strict majority of vals are equal. Rather
// than return a zero scale — which collapses every caller's band to
// the centre point and rejects all honest dispersion — the scale
// falls back to [zeroScaleRelFloor]·|centre|. A zero centre has no
// relative floor to compute, so it keeps the zero scale.
func robustCentreScale(vals []*big.Rat) (centre, scale *big.Rat) {
	if len(vals) == 0 {
		return nil, nil
	}
	centre = medianRat(vals)
	scale = new(big.Rat).Mul(madToStd, madRat(vals, centre))
	if scale.Sign() == 0 {
		floor := new(big.Rat).Mul(zeroScaleRelFloor, centre)
		floor.Abs(floor)
		if floor.Sign() > 0 {
			scale = floor
		}
	}
	return centre, scale
}

// symmetricDev returns the deviation of p from centre measured symmetrically in
// RATIO (log) space, in the same price units as the σ-equivalent scale.
//
// An ADDITIVE band `|p - centre| > K·scale` is blind below the centre: once
// `K·scale >= centre` no downward print can exceed it, so a crash or
// decimal-shift print (even an exact 0) passes while the mirror up-move is
// rejected.
//
// Price noise is MULTIPLICATIVE (ADR-0046 §1), so a price below the centre is
// mirrored to the up-move at the same log distance (centre²/p) and measured
// from the centre. The band is
//
//	[ centre² / (centre + K·scale) , centre + K·scale ]
//
// identical to the additive band above the centre, tighter below it. Exact
// *big.Rat (ADR-0003): no logarithm, no float64.
//
// Returns nil for a non-positive p against a positive centre (callers treat nil
// as "rejected"); a non-positive centre returns the plain additive deviation.
func symmetricDev(p, centre *big.Rat) *big.Rat {
	if p == nil || centre == nil {
		return nil
	}
	if centre.Sign() <= 0 || p.Cmp(centre) >= 0 {
		dev := new(big.Rat).Sub(p, centre)
		return dev.Abs(dev)
	}
	if p.Sign() <= 0 {
		return nil
	}
	// centre²/p − centre = centre·(centre − p)/p, exact.
	dev := new(big.Rat).Sub(centre, p)
	dev.Mul(dev, centre)
	return dev.Quo(dev, p)
}
