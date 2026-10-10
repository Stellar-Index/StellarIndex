package aggregate

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// twapScale is the fixed-point scale the per-trade weighted price is
// accumulated at: 10^40. It bounds the relative error of the returned
// quotient by 10^-40 (see the derivation in [TWAP]), which is far below
// the ~10 decimal places the value is ultimately serialised to, while
// keeping every intermediate a constant ~320 bits wide.
var twapScale = new(big.Int).Exp(big.NewInt(10), big.NewInt(40), nil)

// ErrUnsortedTrades is returned from [TWAP] when a trade's timestamp
// precedes its predecessor's. A descending slice would otherwise hand
// the oldest trade the whole window and publish its price as the average.
var ErrUnsortedTrades = errors.New("aggregate: trades not sorted by timestamp ascending")

// TWAP returns the time-weighted average price over the given trades, with
// each INSTANT's price active until the next PRICED instant (or windowEnd for
// the final one). An instant is a run of trades sharing one Timestamp; its
// price is that run's volume-weighted Σquote/Σbase.
//
// Grouping by instant makes the result independent of the order of
// same-timestamp trades: on-chain fills share their ledger close time, so
// per-trade weighting would hand the whole interval to whichever fill sorts
// last and let a dust print carry it alone.
//
// Trades must be sorted ascending by Timestamp; the function returns
// [ErrUnsortedTrades] rather than sorting, which would hide caller bugs. A
// windowEnd before the last trade clamps that slot to zero. Returns
// [ErrNoTrades] for an empty slice or a zero total duration.
//
// An instant [priceable] skips entirely (both legs non-positive on every
// trade) abstains: it neither sets a price nor ends the prevailing instant's
// slot, so the window's time stays covered rather than vanishing from the
// denominator.
func TWAP(trades []canonical.Trade, windowEnd time.Time) (*big.Rat, error) {
	price, _, err := TWAPWithCount(trades, windowEnd)
	return price, err
}

// TWAPWithCount is [TWAP] that also returns how many trades carried
// weight: priced trades in an instant with a positive slot.
func TWAPWithCount(trades []canonical.Trade, windowEnd time.Time) (*big.Rat, int, error) {
	if len(trades) == 0 {
		return nil, 0, ErrNoTrades
	}

	// weightedSum accumulates Σ(price_i × Δt_i) in FIXED POINT (scaled by
	// twapScale), NOT as a big.Rat: Rat.Add accretes the LCM of the operands'
	// denominators, so the loop goes super-linear (n=10000, the handler's
	// maxTrades cap, took ~60s) and aggregate.TWAP takes no ctx, so nothing can
	// cancel it. The exactness Rat buys is discarded anyway at ~10 served
	// places; truncation error is bounded by 1/twapScale = 10^-40 relative.
	//
	// totalNanos is a *big.Int, NOT an int64: time.Time.Sub saturates at
	// MaxInt64, so a far-future windowEnd (the API places no bound on `to`)
	// would wrap the sum negative and flip the published price's sign.
	weightedSum := new(big.Int)
	totalNanos := new(big.Int)
	scratch := new(big.Int)

	// pending is the last PRICED instant seen: its price is current
	// until the next priced instant's timestamp (or windowEnd). An
	// unpriced instant in between contributes nothing of its own and
	// does NOT close pending's slot — see the doc comment above.
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

	// Sign() <= 0, not == 0. Every Δt added above is strictly positive
	// (accrueTWAPSlot drops dur <= 0), so a non-positive total is only reachable
	// via the saturation described above — but assert it rather than
	// assume it, because the failure mode is a signed money value on a
	// 200 response.
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

// accrueTWAPSlot adds one priced instant's slot to the TWAP accumulators:
// ⌊quote × SCALE × Δt / base⌋ to weightedSum and Δt (integer ns) to
// totalNanos; the scale cancels in the final division. base/quote are the
// instant's own Σbase/Σquote from [instantVolumes] — a single trade is the
// degenerate one-trade instant. A non-positive Δt (a timestamp tie, or
// windowEnd before the instant) contributes nothing; callers already guard
// this, but the check stays so a future caller can't reintroduce a
// negative-duration slot silently.
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
