package aggregate

import (
	"math/big"
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
	// guardMinSamples is the number of trailing closed buckets with a
	// usable VWAP at or above which the guard applies its FULL band
	// (tight ratio ∪ MAD). Below it there is no stable MAD, so the guard
	// falls back to the wider thin-history band ([guardThinRatioBound])
	// rather than failing fully open — finding M11(b). Only a TRULY
	// empty baseline (zero usable buckets) fails open, because then
	// there is no centre to judge a candidate against at all.
	guardMinSamples = 5
)

var (
	// guardRatioBound: on a tight/flat history a candidate is accepted
	// only if it lies within [centre/R, centre*R] of the robust centre.
	//
	// R = 3 (finding M11(a); was 10). The previous 10× admitted the
	// entire 3×–9× manipulation band a served-price sanity guard exists
	// to stop; 3× catches a 5× pump (the finding's proof) and every
	// larger deviation. A swing beyond 3× in a single 1-minute bucket is
	// not credible as organic price discovery on a pair whose recent
	// history is tight, and when a genuine >3× move does occur we serve
	// the last clean bucket (a real recent price), never a fabricated
	// number — a one-bucket hold is the conservative, honest trade-off.
	//
	// The band is inclusive, so this rejects deviations STRICTLY GREATER
	// than 3×; a move of up to 3× (covering stablecoin depegs/halvings
	// and extreme-but-real volatility) is still served. NOTE
	// (business-value knob): 3 is the served-price manipulation
	// tolerance — surfaced for the orchestrator to tighten (e.g. to 2×)
	// per risk appetite. A genuinely volatile pair is not held to this
	// number: the MAD band below widens acceptance from the pair's own
	// history.
	guardRatioBound = big.NewRat(3, 1)

	// guardThinRatioBound is the WIDER but FINITE ratio band applied
	// when the baseline is thin (1..guardMinSamples-1 usable buckets):
	// [centre/10, centre*10]. Finding M11(b): a short history used to
	// fail fully open, admitting any manipulation. A scarce baseline is
	// widened (we only catch order-of-magnitude, decimal-shift
	// fat-fingers) rather than tightened, so a real price off a thin
	// history is never over-filtered — but a 10×/100× print is still
	// caught instead of served unguarded.
	guardThinRatioBound = big.NewRat(10, 1)

	// guardMADFactor widens acceptance for genuinely volatile pairs: a
	// candidate within centre ± K·(1.4826·MAD) also passes. Because the
	// acceptance region is the UNION of the ratio band and this MAD band,
	// a volatile pair whose recent buckets are widely spread is never
	// over-filtered — its own history earns it the wider band. K = 10 is
	// ~6.7σ-equivalent, well beyond ordinary volatility.
	guardMADFactor = big.NewRat(10, 1)
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

// robustBand returns the acceptance interval [lo, hi] for a candidate,
// where centre is the median of the (positive, non-nil) trailing
// values. ok is false ONLY when there is no usable value at all (an
// empty baseline — nothing to judge against, so the guard fails open).
//
// Regimes:
//   - >= [guardMinSamples] usable values: the FULL band = union of the
//     tight ratio band [centre/R, centre*R] ([guardRatioBound]) and the
//     MAD band centre ± K·1.4826·MAD ([guardMADFactor]). A volatile
//     pair earns the wider band from its own spread.
//   - 1..guardMinSamples-1 usable values (thin history, M11(b)): the
//     WIDER but finite ratio-only band [centre/thinR, centre*thinR]
//     ([guardThinRatioBound]). No stable MAD on so few points, so we
//     widen rather than fail fully open.
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

// medianRat and madRat (the exact median / MAD primitives) live in
// robust.go — shared with the published-VWAP outlier filter (M5) and
// the global aggregator-tier filter (M8) so all three guards use one
// exact-rational definition of robust centre and spread.
