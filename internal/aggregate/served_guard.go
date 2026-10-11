package aggregate

import (
	"math/big"
)

// Serving-sanity guard for the /v1/price closed-bucket path: the prices_1m CAGG is a bare sum(quote)/sum(base) that bypasses the outlier filter, USD-volume gate
// and freeze protection, so one fat-finger trade would be served as stale=false. [GuardServedVWAP] rejects a candidate grossly off the trailing robust centre
// and signals serving last-known-good; the acceptance region is the UNION of a ratio band and a MAD band, so volatile moves pass. Exact *big.Rat (ADR-0003).

const (
	// guardMinSamples is the usable trailing buckets at or above which the FULL band applies; below it the wider [guardThinRatioBound] applies, and only a truly empty baseline fails open.
	guardMinSamples = 5
)

var (
	// guardRatioBound R=3: on a tight history a candidate is accepted only within [centre/R, centre*R] (inclusive). 10x would admit the 3x-9x manipulation
	// band; a genuine >3x move serves the last clean bucket. A business-value knob; the MAD band widens acceptance for volatile pairs.
	guardRatioBound = big.NewRat(3, 1)

	// guardThinRatioBound is the WIDER but FINITE band [centre/10, centre*10] for a thin baseline (1..guardMinSamples-1 buckets): failing open would admit any manipulation.
	guardThinRatioBound = big.NewRat(10, 1)

	// guardMADFactor K=10 (~6.7 sigma) widens acceptance for volatile pairs: the ratio-symmetric band [centre^2/(centre+K*s), centre+K*s], s = 1.4826*MAD,
	// is UNIONED with the ratio band ([symmetricDev]), so a pair's own spread earns it latitude.
	guardMADFactor = big.NewRat(10, 1)
)

// GuardServedVWAP decides whether a candidate VWAP is sane against newest-first trailing VWAPs: accept=false gives lkgIdx of the newest in-band value; accept=true with lkgIdx=-1 may be an UNVALIDATED fail-open, so callers must check [ServedBaselineValidated].
func GuardServedVWAP(candidate *big.Rat, trailing []*big.Rat) (accept bool, lkgIdx int) {
	lo, hi, ok := robustBand(trailing)
	if !ok || candidate == nil {
		return true, -1
	}
	if withinBand(candidate, lo, hi) {
		return true, -1
	}
	// Candidate rejected — walk newest-first for the last clean bucket.
	for i := range trailing {
		if trailing[i] != nil && withinBand(trailing[i], lo, hi) {
			return false, i
		}
	}
	// No clean trailing member (unreachable given the median centre); the
	// safe fallback is to serve the candidate rather than 404 a pair that
	// demonstrably has data.
	return true, -1
}

// ServedBaselineValidated reports whether trailing gives a usable centre to validate against; when false, surface an accepted value as low-confidence/stale, never a blackout.
func ServedBaselineValidated(trailing []*big.Rat) bool {
	_, _, ok := robustBand(trailing)
	return ok
}

// robustBand returns the acceptance interval [lo, hi] around the median of positive trailing values; ok is false only for an empty baseline (guard fails open).
// With >= guardMinSamples values it is the union of the tight ratio band and the ratio-symmetric MAD band; with fewer, the finite [guardThinRatioBound] band.
func robustBand(trailing []*big.Rat) (lo, hi *big.Rat, ok bool) {
	vals := make([]*big.Rat, 0, len(trailing))
	for _, v := range trailing {
		if v != nil && v.Sign() > 0 {
			vals = append(vals, v)
		}
	}
	// Empty baseline → no centre → fail open (favour serving a real
	// price over rejecting against nothing).
	if len(vals) == 0 {
		return nil, nil, false
	}
	centre := medianRat(vals)
	if centre.Sign() <= 0 {
		return nil, nil, false
	}

	// Thin history: wider, finite ratio-only band — still catches
	// order-of-magnitude manipulation, never fails fully open.
	if len(vals) < guardMinSamples {
		lo = new(big.Rat).Quo(centre, guardThinRatioBound)
		hi = new(big.Rat).Mul(centre, guardThinRatioBound)
		return lo, hi, true
	}

	// Ratio band: [centre/R, centre*R].
	lo = new(big.Rat).Quo(centre, guardRatioBound)
	hi = new(big.Rat).Mul(centre, guardRatioBound)

	// MAD band: up to centre + K*scale, down to its ratio-symmetric mirror centre^2/(centre + K*scale) ([symmetricDev]), unioned with the ratio band.
	// An additive lower edge goes non-positive at relative MAD 1/(K*1.4826) = 6.75%, leaving no downside guard: a crafted bucket down to 0 would be served.
	scale := new(big.Rat).Mul(madToStd, madRat(vals, centre)) // σ-equivalent
	half := new(big.Rat).Mul(guardMADFactor, scale)           // K·scale
	madHi := new(big.Rat).Add(centre, half)
	madLo := new(big.Rat).Quo(new(big.Rat).Mul(centre, centre), madHi)
	if madLo.Cmp(lo) < 0 {
		lo = madLo
	}
	if madHi.Cmp(hi) > 0 {
		hi = madHi
	}
	return lo, hi, true
}

// withinBand reports whether lo <= v <= hi. A nil bound or value can't be
// judged, so it is treated as "in band" (never a reason to reject).
func withinBand(v, lo, hi *big.Rat) bool {
	if v == nil || lo == nil || hi == nil {
		return true
	}
	return v.Cmp(lo) >= 0 && v.Cmp(hi) <= 0
}

// medianRat and madRat (the exact median / MAD primitives) live in
// robust.go — shared with the published-VWAP outlier filter and
// the global aggregator-tier filter so all three guards use one
// exact-rational definition of robust centre and spread.
