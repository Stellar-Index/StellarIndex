package aggregate

import (
	"math/big"
	"sort"
)

// Serving-sanity guard for the /v1/price closed-bucket path.
//
// Context (adversarial-review HIGH): /v1/price serves the most-recent
// CLOSED prices_1m continuous-aggregate bucket for a directly-quoted
// pair. That CAGG is a bare Σ(quote)/Σ(base) per bucket — it is NOT run
// through the orchestrator's σ-outlier filter, min-USD-volume gate, or
// freeze value-protection (those guard the ORCHESTRATOR path that writes
// the filtered VWAP to Redis, which the CAGG bypasses). A pure-synthetic
// fiat pair like native/fiat:USD has no prices_1m rows at all (SDEX
// native trades are quoted in issuer-stablecoins, never fiat:USD), so it
// misses the CAGG read and falls through to the filtered Redis value. But
// any pair with real prices_1m rows serves its raw closed-bucket VWAP
// unfiltered: directly-quoted DEX/CEX pairs (a Soroban token priced in
// USDC-GA5Z…, crypto:BTC/crypto:USDT, …) AND headline pairs with a real
// fiat CEX market (crypto:XLM/fiat:USD via Kraken/Coinbase). For those, a
// single fat-finger / manipulation trade in the served minute would
// corrupt the price with stale=false and no volume floor.
//
// [GuardServedVWAP] is a robust sanity bound over the pair's recent
// trailing closed buckets: it rejects a candidate whose VWAP is grossly
// off the robust centre and signals the caller to serve last-known-good
// instead. It is tuned CONSERVATIVELY — the acceptance region is the
// UNION of a wide ratio band and a MAD band, so it only ever catches
// gross (order-of-magnitude-ish) deviation and never a legitimately
// volatile-but-real move. On a healthy bucket it is a pure pass-through
// (a liquid pair sits tightly clustered and always passes), so it changes
// the served value ONLY for a manipulated bucket. Everything is exact
// *big.Rat (ADR-0003); no float64 enters the value path.

const (
	// guardMinSamples is the minimum number of trailing closed buckets
	// with a usable VWAP required before the guard will judge a
	// candidate. With fewer there is no robust baseline, so the guard
	// fails OPEN (serves the candidate) rather than risk dropping a real
	// price off a thin history.
	guardMinSamples = 5
)

var (
	// guardRatioBound: a candidate is accepted if it lies within
	// [centre/R, centre*R] of the robust centre. R = 10 catches
	// decimal-shift fat-fingers (10×, 100×, 0.1×, 0.01× — the classic
	// "extra zero" / misplaced-decimal manipulation) while passing
	// anything inside an order of magnitude. A real move that large in a
	// single 1-minute bucket essentially never happens outside
	// manipulation, and even when it does we serve the last clean bucket
	// (a real recent price), never a fabricated number — so a one-bucket
	// hold on a genuine 10× move is the conservative, honest trade-off.
	guardRatioBound = big.NewRat(10, 1)

	// guardMADFactor widens acceptance for genuinely volatile pairs: a
	// candidate within centre ± K·(1.4826·MAD) also passes. Because the
	// acceptance region is the UNION of the ratio band and this MAD band,
	// a volatile pair whose recent buckets are widely spread is never
	// over-filtered — its own history earns it the wider band. K = 10 is
	// ~6.7σ-equivalent, well beyond ordinary volatility.
	guardMADFactor = big.NewRat(10, 1)

	// madToStd is 1.4826, the constant that scales a median-absolute-
	// deviation to a standard-deviation-equivalent for a normal
	// distribution (MAD is more robust to outliers than raw stdev, which
	// is the whole point here — a prior manipulated bucket in the
	// trailing set must not inflate the scale).
	madToStd = big.NewRat(7413, 5000)
)

// GuardServedVWAP decides, from a candidate VWAP and the pair's recent
// trailing closed-bucket VWAPs (newest-first, index-aligned with the
// caller's rows — nil entries are tolerated for unparseable values),
// whether the candidate is a robust-sane value to serve.
//
// Returns:
//   - accept=true, lkgIdx=-1 → serve the candidate. Either it passed the
//     robust band, or there was no usable baseline to judge it against
//     (fail-open: favour serving a real price over over-filtering).
//   - accept=false, lkgIdx>=0 → the candidate is grossly off the robust
//     centre; serve trailing[lkgIdx] instead — the newest trailing value
//     that IS within the band (last-known-good). Because the robust
//     centre is the median of the baseline, at least half the baseline
//     lies within the band, so a clean member is guaranteed to exist
//     whenever the guard fires.
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

// robustBand returns the acceptance interval [lo, hi] = union of the
// ratio band [centre/R, centre*R] and the MAD band centre ± K·1.4826·MAD,
// where centre is the median of the (positive, non-nil) trailing values.
// ok is false when fewer than [guardMinSamples] usable values exist.
func robustBand(trailing []*big.Rat) (lo, hi *big.Rat, ok bool) {
	vals := make([]*big.Rat, 0, len(trailing))
	for _, v := range trailing {
		if v != nil && v.Sign() > 0 {
			vals = append(vals, v)
		}
	}
	if len(vals) < guardMinSamples {
		return nil, nil, false
	}
	centre := medianRat(vals)
	if centre.Sign() <= 0 {
		return nil, nil, false
	}

	// Ratio band: [centre/R, centre*R].
	lo = new(big.Rat).Quo(centre, guardRatioBound)
	hi = new(big.Rat).Mul(centre, guardRatioBound)

	// MAD band: centre ± K·(1.4826·MAD). Union with the ratio band; both
	// intervals contain centre, so their union is a single interval.
	scale := new(big.Rat).Mul(madToStd, madRat(vals, centre)) // σ-equivalent
	half := new(big.Rat).Mul(guardMADFactor, scale)           // K·scale
	if madLo := new(big.Rat).Sub(centre, half); madLo.Cmp(lo) < 0 {
		lo = madLo
	}
	if madHi := new(big.Rat).Add(centre, half); madHi.Cmp(hi) > 0 {
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

// medianRat returns the exact median of vals (which must be non-empty)
// as a fresh *big.Rat. Does not mutate its input.
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

// madRat returns the median absolute deviation of vals about centre.
func madRat(vals []*big.Rat, centre *big.Rat) *big.Rat {
	devs := make([]*big.Rat, len(vals))
	for i, v := range vals {
		d := new(big.Rat).Sub(v, centre)
		devs[i] = d.Abs(d)
	}
	return medianRat(devs)
}
