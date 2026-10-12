package aggregate

import (
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// FilterOutliers drops prices beyond sigma*(1.4826*MAD) of the median in ratio space (ADR-0046 §1), exact big.Rat; sigma <= 0 or fewer than 3 usable prices is a no-op, and a trim dropping the base-volume majority returns an empty slice.
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

// keepIfVolumeMajority returns kept unless it carries less base volume than the usable trades dropped, then empty: a count majority of
// dust prints must not delete the money traded (ADR-0046 §5). scaleOf lifts volumes to one scale first; nil compares raw BaseAmount.
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

// priceRat projects a trade's price (quote-per-base) to an exact *big.Rat; ok=false for zero-base or zero-quote trades so they are
// dropped before the statistics rather than dragged in as a spurious price.
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
