package aggregate

import (
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// NormalizeAmountScale returns a copy of `trades` in which every trade's
// BaseAmount and QuoteAmount have been lifted to a single common
// smallest-unit scale — the LARGEST per-source scale present in the slice
// — so that a volume-weighted aggregation ([VWAP], [ComputeOHLC]'s volume
// totals) reflects each trade's REAL volume rather than its raw
// smallest-unit magnitude.
//
// Why this exists: canonical.Trade amounts are stamped at a per-SOURCE
// fixed-point scale (CLAUDE.md / CS-040 — on-chain DEX legs are 7-decimal
// stroops, CEX 8, the FX pollers 6). A trade's implied PRICE
// (QuoteAmount/BaseAmount) is scale-invariant, but its WEIGHT in a
// Σquote/Σbase mean is its raw BaseAmount = real_volume × 10^source_decimals.
// Summing raw amounts across a window that MIXES sources of different scale
// therefore over-weights the finer-scaled source — a CEX 1e8 trade counts
// 10× an equal-sized 1e7 on-chain trade — skewing the served weighted price
// toward that source. The API fiat-combine path
// ([Server.fiatCombinedTrades] in internal/api/v1) is exactly such a mix:
// it merges on-chain native/USDC-classic (7dp) with CEX
// crypto:XLM/crypto:USDT (8dp) into one slice.
//
// This is a distinct axis from [AdjustPrice]. AdjustPrice corrects a
// per-PAIR base-vs-quote asset-decimals mismatch, which is a single
// constant for a whole aggregation call (every trade shares one pair), so it
// can be a post-hoc scalar on the finished ratio. Here the factor varies
// per-trade by SOURCE, so it must be applied to each trade BEFORE the
// weighted sum — a post-hoc scalar cannot express it.
//
// decimalsFor resolves a trade's smallest-unit scale from its Source (the
// production resolver is external.Lookup(source).AmountScaleDecimals()).
// aggregate takes it as a parameter rather than importing
// internal/sources/external — that package imports aggregate, so the reverse
// edge would be an import cycle.
//
// Correctness properties:
//
//   - The common scale is the MAXIMUM scale in the window, so every rescale
//     is an exact integer multiply by 10^(max−scale) ≥ 1 — no division, no
//     precision loss (ADR-0003).
//   - Each trade's price ratio is preserved exactly: base and quote are both
//     multiplied by the same factor (a source stamps both legs at one scale).
//   - UNIFORM-scale windows — every trade at one scale, the common all-CEX or
//     all-on-chain case — are returned BYTE-IDENTICAL: the factor is
//     10^0 = 1 for every trade, so no amount is touched. Only genuinely
//     mixed windows move, toward the true real-volume-weighted price.
//
// A nil or empty input is returned unchanged. Input trades (which may alias a
// shared read cache) are never mutated: amounts are only ever read via
// Amount.BigInt (which copies) and written back via canonical.NewAmount
// (which copies), so the returned slice shares no *big.Int with the input.
func NormalizeAmountScale(trades []canonical.Trade, decimalsFor func(source string) int) []canonical.Trade {
	if len(trades) == 0 {
		return trades
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
	return out
}
