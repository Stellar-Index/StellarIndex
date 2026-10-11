package aggregate

import (
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// OHLC is an open/high/low/close bar with total base and quote volumes; prices are exact rationals.
type OHLC struct {
	Open        *big.Rat
	High        *big.Rat
	Low         *big.Rat
	Close       *big.Rat
	BaseVolume  canonical.Amount
	QuoteVolume canonical.Amount
	TradeCount  int
}

// ComputeOHLC builds an [OHLC] bar from trades that MUST be sorted ascending by Timestamp (not sorted here, like [TWAP]); [ErrNoTrades] if none has a valid price.
func ComputeOHLC(trades []canonical.Trade) (*OHLC, error) {
	if len(trades) == 0 {
		return nil, ErrNoTrades
	}

	bar := &OHLC{}
	totalBase := new(big.Int)
	totalQuote := new(big.Int)

	for i := range trades {
		t := &trades[i]
		b := t.BaseAmount.BigInt()
		if b.Sign() <= 0 {
			continue
		}
		q := t.QuoteAmount.BigInt()
		if q.Sign() <= 0 {
			continue
		}

		price := new(big.Rat).SetFrac(q, b)

		if bar.TradeCount == 0 {
			bar.Open = new(big.Rat).Set(price)
			bar.High = new(big.Rat).Set(price)
			bar.Low = new(big.Rat).Set(price)
		} else {
			if price.Cmp(bar.High) > 0 {
				bar.High.Set(price)
			}
			if price.Cmp(bar.Low) < 0 {
				bar.Low.Set(price)
			}
		}
		bar.Close = new(big.Rat).Set(price)
		bar.TradeCount++

		totalBase.Add(totalBase, b)
		totalQuote.Add(totalQuote, q)
	}

	if bar.TradeCount == 0 {
		return nil, ErrNoTrades
	}
	bar.BaseVolume = canonical.NewAmount(totalBase)
	bar.QuoteVolume = canonical.NewAmount(totalQuote)
	return bar, nil
}
