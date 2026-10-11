package aggregate

import (
	"math/big"
	"sort"
)

// Shared robust-statistics primitives for the served-price guards (FilterOutliers, rejectAggregatorOutliers, GuardServedVWAP): the median
// and the 1.4826-scaled MAD, preferred over mean/sigma, which is masking-vulnerable. Exact *big.Rat (ADR-0003): no float64 on the value path.

// madToStd is 1.4826 = 1/Phi^-1(0.75), rescaling a MAD to a sigma-equivalent for normal data, held as the exact rational 7413/5000.
var madToStd = big.NewRat(7413, 5000)

// medianRat returns the exact median of non-empty vals as a fresh *big.Rat without mutating input; even length gives the mean of the
// middle two. Empty input returns nil rather than panicking on the served-price path.
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

// medianMemberRat is a median that is ALWAYS one of the inputs: for even counts the LOWER central value (fail-low), never an averaged
// midpoint of disagreeing routes. Used for the served cross-rate composite ([highestConfidencePrice]); equals medianRat when the
// central values agree. Empty input returns nil.
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

// zeroScaleRelFloor (1/200 = 0.5% of the centre, a +-2% band at sigma 4) is the scale used when MAD is 0, i.e. a strict majority sit at
// one price; without it the band collapses to a point and drops every honestly-differing print. Mirrors [robustBand] in served_guard.go.
var zeroScaleRelFloor = big.NewRat(1, 200)

// robustCentreScale returns the median and the sigma-equivalent scale (madToStd*MAD) of vals, all exact; empty vals returns nil, nil.
// When MAD is 0 the scale falls back to [zeroScaleRelFloor]*|centre| (a zero centre keeps the zero scale).
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

// symmetricDev returns p's deviation from centre in RATIO space (ADR-0046 §1): above centre p-centre; below it p is mirrored to centre^2/p first,
// so the band is [centre^2/(centre+K*scale), centre+K*scale]; an additive band goes blind below once K*scale >= centre. Exact *big.Rat.
// Returns nil for a non-positive p against a positive centre (callers treat nil as rejected); a non-positive centre gets the additive deviation.
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
