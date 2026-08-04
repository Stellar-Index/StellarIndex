package aggregate

import (
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TWAP returns the time-weighted average price over the given
// trades, with each trade's price active until the next trade's
// timestamp (or windowEnd for the final trade).
//
// Requirements:
//
//   - trades must be sorted by Timestamp, ascending. The function
//     does NOT sort internally — doing so silently would hide
//     caller bugs. If trades are unsorted, results are meaningless.
//   - windowEnd must be ≥ the last trade's timestamp. A windowEnd
//     earlier than the last trade's timestamp means the final
//     trade's slot is negative; we clamp to zero for that slot
//     rather than return an error, but ordering upstream is still
//     a bug.
//
// Returns [ErrNoTrades] for an empty slice or when the total
// duration is zero (every trade at the exact same timestamp as
// windowEnd and each other).
//
// Formula: TWAP = Σ(price_i × Δt_i) / Σ(Δt_i), where Δt_i is the
// duration the i-th price was "current."
//
// Trades with zero base volume are skipped — they have no defined
// price.
func TWAP(trades []canonical.Trade, windowEnd time.Time) (*big.Rat, error) {
	if len(trades) == 0 {
		return nil, ErrNoTrades
	}

	// weightedSum accumulates Σ(price_i × Δt_i) as a Rat.
	//
	// totalNanos accumulates Σ(Δt_i) as a *big.Int, NOT an int64. It was
	// an int64 of nanoseconds, which overflows: time.Time.Sub SATURATES
	// at MaxInt64 (~292.47 years), so a windowEnd far enough in the
	// future produces a saturated Δt, and adding any further positive
	// interval WRAPS THE SUM NEGATIVE — after which the final division
	// flips the sign of the published price. The guard below only tested
	// for zero, so a negative denominator sailed through.
	//
	// Reproduced against production 2026-08-04:
	//   /v1/twap?base=native&quote=fiat:USD&from=2026-08-01&to=9999-12-31
	//   → 200 {"price":"-0.1702543997", "flags":{"stale":false}}
	// The API places no upper bound on an explicit `to`, so any client
	// using a far-future sentinel for "no end bound" got a negative money
	// string on a success response. A big.Int accumulator removes the
	// overflow class rather than papering over this one entry point.
	weightedSum := new(big.Rat)
	totalNanos := new(big.Int)

	for i := range trades {
		base := trades[i].BaseAmount.BigInt()
		if base.Sign() <= 0 {
			continue
		}
		quote := trades[i].QuoteAmount.BigInt()
		if quote.Sign() <= 0 {
			continue
		}

		var dur time.Duration
		if i == len(trades)-1 {
			dur = windowEnd.Sub(trades[i].Timestamp)
		} else {
			dur = trades[i+1].Timestamp.Sub(trades[i].Timestamp)
		}
		if dur <= 0 {
			continue
		}

		price := new(big.Rat).SetFrac(quote, base)
		// Weight = Δt in nanoseconds (integer). Scaling by the same
		// factor on top + bottom, it cancels in the final division —
		// so raw nanoseconds is a valid weight choice.
		weight := big.NewRat(int64(dur), 1)
		price.Mul(price, weight)
		weightedSum.Add(weightedSum, price)
		totalNanos.Add(totalNanos, big.NewInt(int64(dur)))
	}

	// Sign() <= 0, not == 0. Every Δt added above is strictly positive
	// (the dur <= 0 continue), so a non-positive total is only reachable
	// via the saturation described above — but assert it rather than
	// assume it, because the failure mode is a signed money value on a
	// 200 response.
	if totalNanos.Sign() <= 0 {
		return nil, ErrNoTrades
	}
	return new(big.Rat).Quo(weightedSum, new(big.Rat).SetInt(totalNanos)), nil
}
