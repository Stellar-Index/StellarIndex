package aggregate

import (
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

	// weightedSum accumulates Σ(price_i × Δt_i) in FIXED POINT (scaled by
	// twapScale), NOT as a big.Rat.
	//
	// It was a big.Rat, which made this loop super-linear and turned one
	// anonymous GET into multiple CPU-seconds. big.Rat.Add puts both
	// operands over a common denominator and renormalises: adding N
	// prices with distinct base amounts accretes their LCM, so the
	// running denominator grows without bound. Measured offline at the
	// handler's own maxTrades=10000 cap: n=1000 → 144ms, n=4000 → 5.0s,
	// n=8000 → 32.8s, n=10000 → 60.6s (final denominator 20.3 KB of
	// bignum) — roughly n^2.6.
	//
	// Measured on production 2026-08-04, one request:
	//   GET /v1/twap?base=native&quote=fiat:USD&window=48h
	//   → 200, 295-byte body, 7.47 CPU-SECONDS (read from /proc/<pid>/stat)
	// /v1/vwap over the identical DB path and the same 10000-trade cap
	// costs 0.27s, which isolates the accumulator as the ~7.1s: the cost
	// was flat across a 28x window range, the signature of the fixed
	// trade cap rather than a scan. journalctl already held three
	// 111-SECOND 200s with 296-byte bodies. Nothing could reclaim it —
	// aggregate.TWAP takes no ctx, and RequestTimeout only injects a
	// deadline, it does not abort the handler goroutine.
	//
	// Fixed point is the right tool because the exactness big.Rat buys is
	// discarded anyway: the result is serialised to a decimal string at
	// ~10 places. Each term truncates by <1 unit in the last place, and
	// every Δt is ≥1ns so Σ Δt ≥ n, bounding the RELATIVE error of the
	// quotient by n/(twapScale·ΣΔt) ≤ 1/twapScale = 10^-40 — thirty
	// orders of magnitude below the served precision. Every intermediate
	// stays ~320 bits wide, so the loop is linear.
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
	weightedSum := new(big.Int)
	totalNanos := new(big.Int)
	scratch := new(big.Int)

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

		// Accumulate ⌊quote × SCALE × Δt / base⌋ as a fixed-point
		// big.Int rather than adding an exact big.Rat per trade.
		//
		// Weight = Δt in nanoseconds (integer). Scaling by the same
		// factor on top + bottom, it cancels in the final division —
		// so raw nanoseconds is a valid weight choice.
		scratch.Mul(quote, twapScale)
		scratch.Mul(scratch, big.NewInt(int64(dur)))
		scratch.Quo(scratch, base)
		weightedSum.Add(weightedSum, scratch)
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
	// Undo the fixed-point scale in the same division that applies the
	// weights: TWAP = (Σ⌊price_i·SCALE·Δt_i⌋) / (SCALE · Σ Δt_i).
	return new(big.Rat).SetFrac(weightedSum, scratch.Mul(totalNanos, twapScale)), nil
}
