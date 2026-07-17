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
func medianRat(vals []*big.Rat) *big.Rat {
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

// robustCentreScale returns the robust centre (median) and the
// σ-equivalent scale (madToStd · MAD) of vals, all exact. vals must be
// non-empty.
//
// The scale is 0 exactly when a strict majority of vals are equal
// (MAD == 0); callers decide how to treat a zero scale — the guards
// here treat any deviation from that majority centre as an outlier,
// matching the codebase's MAD convention (baseline.ZScore reports any
// deviation from a zero-spread baseline as anomalous).
func robustCentreScale(vals []*big.Rat) (centre, scale *big.Rat) {
	centre = medianRat(vals)
	scale = new(big.Rat).Mul(madToStd, madRat(vals, centre))
	return centre, scale
}
