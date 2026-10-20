package aggregate

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// twapScale is the 10^40 fixed-point scale of the weighted-price accumulator, bounding relative error at 10^-40 (see [TWAP]) while keeping intermediates ~320 bits.
var twapScale = new(big.Int).Exp(big.NewInt(10), big.NewInt(40), nil)

// ErrUnsortedTrades is returned from [TWAP] when a timestamp precedes its predecessor's, which would give the oldest trade the whole window.
var ErrUnsortedTrades = errors.New("aggregate: trades not sorted by timestamp ascending")

// TWAP returns the time-weighted average price where each INSTANT (same-Timestamp trades priced at their volume-weighted Σquote/Σbase) is active until the next priced instant or windowEnd; trades MUST be sorted ([ErrUnsortedTrades]), and [ErrNoTrades] covers empty input or zero duration.
func TWAP(trades []canonical.Trade, windowEnd time.Time) (*big.Rat, error) {
	price, _, err := TWAPWithCount(trades, windowEnd)
	return price, err
}

// TWAPWithCount is [TWAP] that also returns how many trades carried weight (priced trades in an instant with a positive slot).
func TWAPWithCount(trades []canonical.Trade, windowEnd time.Time) (*big.Rat, int, error) {
	if len(trades) == 0 {
		return nil, 0, ErrNoTrades
	}

	// weightedSum accumulates Σ(price×Δt) in FIXED POINT (twapScale), not big.Rat: Rat.Add's LCM growth made n=10000 take ~60s with no ctx to cancel it;
	// truncation is bounded by 10^-40 relative. totalNanos is a *big.Int: time.Time.Sub saturates at MaxInt64, so a far-future windowEnd would wrap an int64 negative.
	weightedSum := new(big.Int)
	totalNanos := new(big.Int)
	scratch := new(big.Int)

	// pending is the last PRICED instant seen: its price is current until the next priced instant's timestamp (or windowEnd).
	// An unpriced instant in between contributes nothing and does NOT close pending's slot (see the doc comment above).
	var pendingBase, pendingQuote *big.Int
	var pendingStart time.Time
	pendingPriced := 0
	havePending := false

	weighted := 0
	for i := 0; i < len(trades); {
		j, err := instantEnd(trades, i)
		if err != nil {
			return nil, 0, err
		}
		base, quote, priced := instantVolumes(trades[i:j])
		if priced > 0 {
			if havePending {
				if dur := trades[i].Timestamp.Sub(pendingStart); dur > 0 {
					accrueTWAPSlot(weightedSum, totalNanos, scratch, pendingBase, pendingQuote, dur)
					weighted += pendingPriced
				}
			}
			pendingBase, pendingQuote, pendingStart, pendingPriced, havePending = base, quote, trades[i].Timestamp, priced, true
		}
		i = j
	}
	if havePending {
		if dur := windowEnd.Sub(pendingStart); dur > 0 {
			accrueTWAPSlot(weightedSum, totalNanos, scratch, pendingBase, pendingQuote, dur)
			weighted += pendingPriced
		}
	}

	// Sign() <= 0, not == 0: every Δt added is positive, so a non-positive total is only reachable via the saturation above,
	// but the failure mode is a signed money value on a 200 response, so assert it.
	if totalNanos.Sign() <= 0 {
		return nil, 0, ErrNoTrades
	}
	// Undo the fixed-point scale in the same division that applies the
	// weights: TWAP = (Σ⌊price_k·SCALE·Δt_k⌋) / (SCALE · Σ Δt_k).
	return new(big.Rat).SetFrac(weightedSum, scratch.Mul(totalNanos, twapScale)), weighted, nil
}

// instantEnd returns the index one past the instant starting at trades[i]
// (the run of trades sharing its Timestamp), or [ErrUnsortedTrades] when
// trades[i] precedes its predecessor.
func instantEnd(trades []canonical.Trade, i int) (int, error) {
	if i > 0 && trades[i].Timestamp.Before(trades[i-1].Timestamp) {
		return 0, fmt.Errorf("%w: trade %d at %s precedes trade %d at %s", ErrUnsortedTrades,
			i, trades[i].Timestamp.Format(time.RFC3339Nano), i-1, trades[i-1].Timestamp.Format(time.RFC3339Nano))
	}
	j := i + 1
	for j < len(trades) && trades[j].Timestamp.Equal(trades[i].Timestamp) {
		j++
	}
	return j, nil
}

// instantVolumes sums the base and quote legs of the priced trades in
// one instant (both legs positive) and counts them.
func instantVolumes(group []canonical.Trade) (base, quote *big.Int, priced int) {
	base, quote = new(big.Int), new(big.Int)
	for k := range group {
		if !priceable(&group[k]) {
			continue
		}
		base.Add(base, group[k].BaseAmount.BigInt())
		quote.Add(quote, group[k].QuoteAmount.BigInt())
		priced++
	}
	return base, quote, priced
}

// accrueTWAPSlot adds one priced instant's slot: floor(quote × SCALE × Δt / base) to weightedSum and Δt ns to totalNanos; base/quote are the instant's
// own sums ([instantVolumes]). A non-positive Δt contributes nothing.
func accrueTWAPSlot(weightedSum, totalNanos, scratch, base, quote *big.Int, dur time.Duration) {
	if dur <= 0 {
		return
	}
	scratch.Mul(quote, twapScale)
	scratch.Mul(scratch, big.NewInt(int64(dur)))
	scratch.Quo(scratch, base)
	weightedSum.Add(weightedSum, scratch)
	totalNanos.Add(totalNanos, big.NewInt(int64(dur)))
}
