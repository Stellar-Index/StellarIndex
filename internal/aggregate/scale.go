package aggregate

import (
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ScaledWindow is a trade window with all amounts at one smallest-unit scale; [NormalizeAmountScale] is its only constructor and [VWAP] accepts nothing else.
type ScaledWindow struct {
	trades   []canonical.Trade
	decimals int
}

// Trades returns the lifted trades; never pass them back to [NormalizeAmountScale], which keys off Source and would lift them again.
func (w ScaledWindow) Trades() []canonical.Trade { return w.trades }

// Decimals is the common scale every trade was lifted to (0 for an empty window).
func (w ScaledWindow) Decimals() int { return w.decimals }

// Len is the number of trades in the window.
func (w ScaledWindow) Len() int { return len(w.trades) }

// FilterOutliers applies [FilterOutliers] and keeps the window's scale, since dropping trades does not un-lift survivors.
func (w ScaledWindow) FilterOutliers(sigma float64) ScaledWindow {
	return ScaledWindow{trades: FilterOutliers(w.trades, sigma), decimals: w.decimals}
}

// NormalizeAmountScale lifts every trade's amounts to the LARGEST per-source scale by exact integer multiply (ADR-0003) so VWAP weights reflect real volume; input is never mutated and decimalsFor resolves scale from Source.
func NormalizeAmountScale(trades []canonical.Trade, decimalsFor func(source string) int) ScaledWindow {
	if len(trades) == 0 {
		return ScaledWindow{trades: trades}
	}

	// Memoize the per-source scale — a window carries hundreds of trades
	// across at most a handful of sources, and decimalsFor may be a
	// registry lookup.
	decCache := make(map[string]int, 4)
	dec := func(source string) int {
		if d, ok := decCache[source]; ok {
			return d
		}
		d := decimalsFor(source)
		decCache[source] = d
		return d
	}

	maxDec := 0
	for i := range trades {
		if d := dec(trades[i].Source); d > maxDec {
			maxDec = d
		}
	}

	// factorFor caches 10^exp per distinct exponent (at most a handful).
	factorFor := make(map[int]*big.Int, 3)

	out := make([]canonical.Trade, len(trades))
	copy(out, trades)
	for i := range out {
		exp := maxDec - dec(out[i].Source)
		if exp <= 0 {
			// Already at the common scale — leave the trade untouched
			// (this is every trade in a uniform window).
			continue
		}
		factor, ok := factorFor[exp]
		if !ok {
			factor = new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exp)), nil)
			factorFor[exp] = factor
		}
		base := out[i].BaseAmount.BigInt() // BigInt copies — no aliasing.
		base.Mul(base, factor)
		out[i].BaseAmount = canonical.NewAmount(base)
		quote := out[i].QuoteAmount.BigInt()
		quote.Mul(quote, factor)
		out[i].QuoteAmount = canonical.NewAmount(quote)
	}
	return ScaledWindow{trades: out, decimals: maxDec}
}
