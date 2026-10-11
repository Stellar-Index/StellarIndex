package aggregate

import (
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// FilterOutliers returns a copy of trades with prices further than `sigma`
// σ-equivalents from the robust centre removed, using a median + MAD guard.
//
// Why MAD, not mean/σ: a mean/σ filter is MASKING-vulnerable (extreme prints
// inflate σ enough to escape rejection) and below ~18 trades rejects nothing.
//
// A price is dropped when its deviation from the median exceeds
// sigma · (1.4826 · MAD); [madToStd] rescales MAD to a σ-equivalent. The
// deviation is symmetric in RATIO space ([symmetricDev]; ADR-0046 §1). Exact
// *big.Rat on the value path (ADR-0003); `sigma` is converted to a rational first.
//
//   - sigma <= 0 is a no-op (shallow copy).
//   - Fewer than 3 usable prices → no robust centre; returned unchanged.
//   - Zero-base / zero-quote trades have no price and are dropped first.
//   - MAD == 0 falls back to [zeroScaleRelFloor]·centre (±2% at sigma=4).
//   - A trim whose survivors carry less base volume than the dropped prints
//     returns an EMPTY slice ([keepIfVolumeMajority]).
func FilterOutliers(trades []canonical.Trade, sigma float64) []canonical.Trade {
	if sigma <= 0 || len(trades) < 3 {
		out := make([]canonical.Trade, len(trades))
		copy(out, trades)
		return out
	}

	prices := make([]*big.Rat, 0, len(trades))
	validIdx := make([]int, 0, len(trades))
	for i := range trades {
		p, ok := priceRat(&trades[i])
		if !ok {
			continue
		}
		prices = append(prices, p)
		validIdx = append(validIdx, i)
	}
	if len(prices) < 3 {
		// Too few usable prices to form a robust centre; return the
		// valid trades unchanged (never the zero-price ones).
		return keepByIndex(trades, validIdx)
	}

	centre, scale := robustCentreScale(prices)
	// threshold = sigma · scale, exact. SetFloat64 is exact for any
	// finite float64; sigma > 0 here, so it never returns nil — but
	// guard defensively rather than dereference a nil.
	sigmaRat := new(big.Rat).SetFloat64(sigma)
	if sigmaRat == nil {
		return keepByIndex(trades, validIdx)
	}
	threshold := new(big.Rat).Mul(sigmaRat, scale)

	kept := make([]int, 0, len(validIdx))
	for k, p := range prices {
		dev := symmetricDev(p, centre)
		if dev == nil || dev.Cmp(threshold) > 0 {
			continue // outlier — drop
		}
		kept = append(kept, validIdx[k])
	}
	return keepIfVolumeMajority(trades, validIdx, kept, nil)
}

// keepIfVolumeMajority returns the kept trades, in order, unless they
// carry less base volume than the usable trades the filter dropped — in
// which case it returns an empty slice and the window is withheld.
//
// The robust centre is a per-print (count) median, so a count majority
// of dust prints can put the honest block outside the band: the trim
// then deletes most of the money traded and the dust becomes the whole
// Σquote/Σbase. A volume-weighted centre only moves the lever to one
// large print (ADR-0046 §5). Requiring the survivors to hold the volume
// majority as well means setting the published price costs both.
//
// scaleOf lifts every trade to one smallest-unit scale before volumes
// are compared ([NormalizeAmountScale]); nil compares raw BaseAmount,
// which is exact for a single-scale window.
func keepIfVolumeMajority(trades []canonical.Trade, validIdx, kept []int, scaleOf func(source string) int) []canonical.Trade {
	if len(kept) == len(validIdx) {
		return keepByIndex(trades, kept)
	}
	vol := trades
	if scaleOf != nil {
		vol = NormalizeAmountScale(trades, scaleOf).Trades()
	}
	total, keptVol := new(big.Int), new(big.Int)
	for _, i := range validIdx {
		total.Add(total, vol[i].BaseAmount.BigInt())
	}
	for _, i := range kept {
		keptVol.Add(keptVol, vol[i].BaseAmount.BigInt())
	}
	// kept < dropped  ⇔  2·kept < total, exact.
	if keptVol.Lsh(keptVol, 1).Cmp(total) < 0 {
		return []canonical.Trade{}
	}
	return keepByIndex(trades, kept)
}

// keepByIndex returns the trades at the given indices, in order.
func keepByIndex(trades []canonical.Trade, idx []int) []canonical.Trade {
	out := make([]canonical.Trade, 0, len(idx))
	for _, i := range idx {
		out = append(out, trades[i])
	}
	return out
}

// priceRat projects a trade's price (quote-per-base) to an exact
// *big.Rat. Reports ok=false for zero-base or zero-quote trades (no
// defined price) so they are dropped before the statistics rather than
// dragged in as a spurious price.
func priceRat(t *canonical.Trade) (*big.Rat, bool) {
	b := t.BaseAmount.BigInt()
	if b.Sign() <= 0 {
		return nil, false
	}
	q := t.QuoteAmount.BigInt()
	if q.Sign() <= 0 {
		return nil, false
	}
	return new(big.Rat).SetFrac(q, b), true
}
