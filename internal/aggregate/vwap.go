package aggregate

import (
	"errors"
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ErrNoTrades is returned from [VWAP] when the input slice is empty
// or every trade has zero base volume. Callers treat this as "price
// unavailable for this window" — not a programming error.
var ErrNoTrades = errors.New("aggregate: no trades in window")

// VWAP returns the volume-weighted average price of a window of
// trades as an exact-precision big.Rat (quote-per-base). It takes a
// [ScaledWindow] because a raw sum over trades at different source scales
// weights each by its smallest-unit magnitude, not its volume.
//
// Definition: VWAP = Σ(QuoteAmount_i) / Σ(BaseAmount_i). Trades
// whose base OR quote is non-positive are skipped — they can't
// contribute to a meaningful weighted price. canonical.Trade.Validate
// admits one zero leg (a stored SDEX rounding fill), so this skip is
// load-bearing, not merely defensive.
//
// Returns [ErrNoTrades] when the sum of base volumes is zero (either
// an empty input or every trade skipped). The returned *big.Rat is
// always strictly positive: only priceable trades (base > 0 AND
// quote > 0) contribute.
func VWAP(w ScaledWindow) (*big.Rat, error) {
	trades := w.trades
	if len(trades) == 0 {
		return nil, ErrNoTrades
	}

	sumQuote := new(big.Int)
	sumBase := new(big.Int)
	for i := range trades {
		t := &trades[i]
		if !priceable(t) {
			continue
		}
		sumBase.Add(sumBase, t.BaseAmount.BigInt())
		sumQuote.Add(sumQuote, t.QuoteAmount.BigInt())
	}
	if sumBase.Sign() == 0 {
		return nil, ErrNoTrades
	}
	return new(big.Rat).SetFrac(sumQuote, sumBase), nil
}

// priceable reports whether a trade belongs to the priced population:
// both legs strictly positive. [VWAP], [TWAP], [SourceContributions] and
// the Total*Volume sums share it so the price and the volumes served
// beside it are computed over the same trades.
func priceable(t *canonical.Trade) bool {
	return t.BaseAmount.BigInt().Sign() > 0 && t.QuoteAmount.BigInt().Sign() > 0
}

// TotalBaseVolume returns Σ(BaseAmount_i) over the trades [VWAP] prices
// from, so it is the exact denominator of the price served beside it.
func TotalBaseVolume(trades []canonical.Trade) canonical.Amount {
	sum := new(big.Int)
	for i := range trades {
		if priceable(&trades[i]) {
			sum.Add(sum, trades[i].BaseAmount.BigInt())
		}
	}
	return canonical.NewAmount(sum)
}

// TotalQuoteVolume returns Σ(QuoteAmount_i) over the trades [VWAP] prices
// from, so it is the exact numerator of the price served beside it.
func TotalQuoteVolume(trades []canonical.Trade) canonical.Amount {
	sum := new(big.Int)
	for i := range trades {
		if priceable(&trades[i]) {
			sum.Add(sum, trades[i].QuoteAmount.BigInt())
		}
	}
	return canonical.NewAmount(sum)
}

// SourceContribution captures one source's share of a windowed VWAP
// — the building block the explorer source-contribution donut renders.
//
// Weight is the exact quote-volume share in [0, 1]; the weights of one
// trade slice sum to exactly 1.
type SourceContribution struct {
	Source      string
	Weight      *big.Rat
	BaseVolume  *big.Int
	QuoteVolume *big.Int
	TradeCount  int
}

// SourceContributions returns one [SourceContribution] per distinct
// trade.Source in `trades`. Each entry's Weight is its
// quote-volume share (i.e. how much that source contributed to the
// VWAP's numerator).
//
// Skips the same edge cases as [VWAP] — trades with non-positive
// base or quote volumes don't contribute. Returns nil for an empty
// or all-skipped input.
func SourceContributions(trades []canonical.Trade) []SourceContribution {
	if len(trades) == 0 {
		return nil
	}
	type accum struct {
		base  *big.Int
		quote *big.Int
		count int
	}
	bySource := make(map[string]*accum)
	totalQuote := new(big.Int)
	for i := range trades {
		t := &trades[i]
		if !priceable(t) {
			continue
		}
		b, q := t.BaseAmount.BigInt(), t.QuoteAmount.BigInt()
		a, ok := bySource[t.Source]
		if !ok {
			a = &accum{base: new(big.Int), quote: new(big.Int)}
			bySource[t.Source] = a
		}
		a.base.Add(a.base, b)
		a.quote.Add(a.quote, q)
		a.count++
		totalQuote.Add(totalQuote, q)
	}
	if totalQuote.Sign() == 0 {
		return nil
	}
	out := make([]SourceContribution, 0, len(bySource))
	for source, a := range bySource {
		out = append(out, SourceContribution{
			Source:      source,
			Weight:      new(big.Rat).SetFrac(a.quote, totalQuote),
			BaseVolume:  a.base,
			QuoteVolume: a.quote,
			TradeCount:  a.count,
		})
	}
	return out
}
