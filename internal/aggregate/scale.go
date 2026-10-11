package aggregate

import (
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ScaledWindow is a trade window whose amounts all sit at one
// smallest-unit scale. [NormalizeAmountScale] is its only constructor and
// [VWAP] accepts nothing else, so a mixed-scale slice cannot be priced.
type ScaledWindow struct {
	trades   []canonical.Trade
	decimals int
}

// Trades returns the lifted trades. Never hand them back to
// [NormalizeAmountScale]: it keys off Source, not the amounts, so a second
// pass multiplies the lower-scale trades again.
func (w ScaledWindow) Trades() []canonical.Trade { return w.trades }

// Decimals is the common scale every trade was lifted to: the largest
// per-source scale in the window as built, 0 for an empty window.
func (w ScaledWindow) Decimals() int { return w.decimals }

// Len is the number of trades in the window.
func (w ScaledWindow) Len() int { return len(w.trades) }

// FilterOutliers applies [FilterOutliers] and keeps the window's scale:
// dropping the trades that set it does not un-lift the survivors.
func (w ScaledWindow) FilterOutliers(sigma float64) ScaledWindow {
	return ScaledWindow{trades: FilterOutliers(w.trades, sigma), decimals: w.decimals}
}

// NormalizeAmountScale returns a [ScaledWindow] over a copy of `trades` with
// every trade's BaseAmount and QuoteAmount lifted to the LARGEST per-source
// scale present, so volume-weighted aggregation ([VWAP], [ComputeOHLC]
// volume) reflects real volume.
//
// Amounts are stamped at a per-SOURCE scale (AGENTS.md: on-chain 7dp, CEX 8,
// FX 6). Price is scale-invariant but a trade's weight in Σquote/Σbase is its
// raw BaseAmount, so a mixed window over-weights the finer-scaled source; the
// API fiat-combine path ([Server.fiatCombinedTrades]) mixes 7dp and 8dp.
// Unlike [AdjustPrice] (one constant per pair, applicable post hoc), the
// factor varies per trade, so it must be applied before the weighted sum.
//
// decimalsFor resolves a trade's scale from its Source (production:
// external.Lookup(source).AmountScaleDecimals()). It is a parameter because
// internal/sources/external imports aggregate.
//
// Rescaling is an exact integer multiply by 10^(max−scale), so there is no
// precision loss (ADR-0003) and both legs keep their ratio. Uniform-scale
// windows come back byte-identical. Nil or empty input is wrapped at scale 0.
// Input trades may alias a shared read cache and are never mutated: amounts
// are copied on read and write.
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
