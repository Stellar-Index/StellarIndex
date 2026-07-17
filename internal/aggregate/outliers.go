package aggregate

import (
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// FilterOutliers returns a copy of trades with prices further than
// `sigma` σ-equivalents from the robust centre removed, using a
// median + MAD (median absolute deviation) guard rather than a
// single-pass mean/standard-deviation.
//
// Why MAD, not mean/σ (finding M5): the published-VWAP path fed the
// orchestrator through a single-pass mean/σ filter, which is
// MASKING-vulnerable — a few extreme prints inflate σ enough that the
// outliers escape their own rejection — and for windows below ~18
// trades σ is so large relative to the data that it provably rejects
// nothing. Median + MAD is masking-resistant (neither the median nor
// the MAD is dragged by a minority of outliers) and discriminates on
// small windows. This aligns the published-VWAP path with the
// serve-time guard ([GuardServedVWAP]), which already uses the same
// median/MAD machinery (see robust.go).
//
// `sigma` keeps its meaning as a σ-equivalent multiplier: a price is
// dropped when |price − median| > sigma · (1.4826 · MAD). The 1.4826
// factor ([madToStd]) rescales MAD to a standard-deviation equivalent
// for normal data, so an existing config default of 4.0 still reads as
// "≈4σ" and callers need not change.
//
// Everything on the value path is exact *big.Rat (ADR-0003): prices
// are quote/base rationals, the median and MAD are exact, and the only
// float64 (`sigma`, a config knob — never a served value) is converted
// to an exact rational before it touches a price.
//
// Edge cases (behaviour preserved from the prior mean/σ version):
//   - sigma <= 0 is a no-op (returns a shallow copy). A σ of 0 would
//     reject every trade, which is never what callers want.
//   - Fewer than 3 usable prices → the filter can't form a robust
//     centre, so it returns the usable trades unchanged.
//   - Zero-base / zero-quote trades have no defined price and are
//     dropped before the statistics.
//   - MAD == 0 (a strict majority of prices identical) makes the scale
//     zero: any price differing from that majority centre is an
//     outlier and is dropped. This is exactly the masking case the
//     finding cites (e.g. [100,100,100,100,200]) and is consistent
//     with the codebase's MAD convention.
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

	out := make([]canonical.Trade, 0, len(validIdx))
	for k, p := range prices {
		dev := new(big.Rat).Sub(p, centre)
		dev.Abs(dev)
		if dev.Cmp(threshold) > 0 {
			continue // outlier — drop
		}
		out = append(out, trades[validIdx[k]])
	}
	return out
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
