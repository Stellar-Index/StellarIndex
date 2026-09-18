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
// median would serve an unproduced midpoint (H1). When the central values
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
// scale when the measured MAD is 0 (MNY-22).
//
// MAD is 0 whenever a strict MAJORITY of vals sit at one exact price —
// a routine shape for a bucket of trades filling against the same
// resting order, or a pegged pair. Left at 0 the σ-equivalent band
// collapses to the single point `centre`, so the filter dropped EVERY
// honestly-differing print in the window (a 100.01 next to four 100s)
// and handed VWAP the majority price alone. Substituting a relative
// floor keeps the outlier rejection that the zero-MAD case exists to
// provide while letting genuine price discovery through.
//
// This mirrors [robustBand] in served_guard.go, which already solved
// the same degeneracy by UNIONing the MAD band with a ratio band so a
// collapsed MAD can never shrink the acceptance interval to a point.
//
// 1/200 = 0.5% of the centre. At the shipped default sigma of 4
// (config `aggregate.outlier_sigma_threshold`) that is a ±2%
// acceptance band around the majority price: comfortably wider than
// the spread honest fills sit inside, and far tighter than the
// fat-finger / wash prints the filter exists to remove (the M5 proof
// case, a 2× print, is 100% away and is still dropped).
var zeroScaleRelFloor = big.NewRat(1, 200)

// robustCentreScale returns the robust centre (median) and the
// σ-equivalent scale (madToStd · MAD) of vals, all exact. vals must be
// non-empty.
//
// MAD is 0 exactly when a strict majority of vals are equal. Rather
// than return a zero scale — which collapses every caller's band to
// the centre point and rejects all honest dispersion — the scale
// falls back to [zeroScaleRelFloor]·|centre|. A zero centre has no
// relative floor to compute, so it keeps the zero scale.
func robustCentreScale(vals []*big.Rat) (centre, scale *big.Rat) {
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

// symmetricDev returns the deviation of p from centre measured
// symmetrically in RATIO (log) space, expressed in the same price units
// as the σ-equivalent scale the callers compare it against.
//
// Why (MNY-22, findings F037/F039/K004/RLT-391): every robust band here
// used to be ADDITIVE in price space — `|p − centre| > K·scale` — which
// is one-sided-blind by construction. `p` can only ever be `centre`
// below the centre, so once `K·scale >= centre` NO downward print can
// exceed the threshold: a crash print, a decimal-shift fat finger, even
// an exact 0, all score inside the band, while the mirror-image up-move
// is still rejected. The additive band goes blind below at a relative
// scale of 1/K — 16.9 % for [FilterOutliers] at the default σ=4, 6.75 %
// for [robustBand]'s MAD arm at K=10 — which ordinary long-tail
// volatility reaches routinely.
//
// Price noise is MULTIPLICATIVE (ADR-0046 §1: "a 2× and a ½× deviation
// should be equally outlying"), so the deviation is measured on the
// ratio: a price below the centre is first mirrored to the up-move that
// is the same distance away in log space (centre²/p — the reflection of
// p about centre under multiplication) and then measured from the
// centre. The resulting band is
//
//	[ centre² / (centre + K·scale) , centre + K·scale ]
//
// — geometrically symmetric (lo·hi = centre²), always strictly
// positive, and IDENTICAL to the old band above the centre. Below it
// the new edge is never lower than the old one (1/(1+r) >= 1 − r), so
// this only ever tightens the downward side: nothing a caller used to
// reject is newly accepted. Exact *big.Rat throughout (ADR-0003) — the
// mirror is one multiply and one divide, so no logarithm (and no
// float64) enters the value path.
//
// Returns nil for a non-positive p against a positive centre: such a
// print has no finite ratio deviation at all, and callers treat nil as
// "rejected" / "no finite score". A non-positive centre has nothing to
// mirror around, so the plain additive deviation is returned unchanged
// (defensive — every caller's centre is the median of positive prices).
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
