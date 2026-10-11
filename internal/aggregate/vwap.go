package aggregate

import (
	"errors"
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ErrNoTrades is returned from [VWAP] when the input is empty or every trade has zero base volume; callers treat it as price unavailable.
var ErrNoTrades = errors.New("aggregate: no trades in window")

// VWAP returns Σquote/Σbase over a [ScaledWindow] as an exact big.Rat, skipping trades with a non-positive leg (load-bearing: Validate admits one zero leg); [ErrNoTrades] if nothing is priceable, so the result is strictly positive.
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

// TotalBaseVolume returns Σ(BaseAmount) over the trades [VWAP] prices from, the exact denominator of the served price.
func TotalBaseVolume(trades []canonical.Trade) canonical.Amount {
	sum := new(big.Int)
	for i := range trades {
		if priceable(&trades[i]) {
			sum.Add(sum, trades[i].BaseAmount.BigInt())
		}
	}
	return canonical.NewAmount(sum)
}

// TotalQuoteVolume returns Σ(QuoteAmount) over the trades [VWAP] prices from, the exact numerator of the served price.
func TotalQuoteVolume(trades []canonical.Trade) canonical.Amount {
	sum := new(big.Int)
	for i := range trades {
		if priceable(&trades[i]) {
			sum.Add(sum, trades[i].QuoteAmount.BigInt())
		}
	}
	return canonical.NewAmount(sum)
}

// SourceContribution is one source's exact quote-volume share (Weight in [0, 1], summing to exactly 1 per slice) of a windowed VWAP.
type SourceContribution struct {
	Source      string
	Weight      *big.Rat
	BaseVolume  *big.Int
	QuoteVolume *big.Int
	TradeCount  int
}

// SourceContributions returns one [SourceContribution] per distinct Source, skipping the same trades as [VWAP]; nil for empty or all-skipped input.
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
