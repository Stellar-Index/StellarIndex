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
// trades, with each INSTANT's price active until the next instant
// (or windowEnd for the final one). An instant is a run of trades
// sharing one Timestamp, and its price is that run's volume-weighted
// Σquote/Σbase.
//
// Grouping by instant is what makes the result independent of the
// order of same-timestamp trades. On-chain sources stamp every fill
// with its ledger close time, so a ledger's fills share one timestamp;
// weighting per trade would hand the whole interval to whichever fill
// sorts last within the ledger (a tx_hash tie-break) and zero weight to
// the rest — a dust print could carry the interval alone. Within an
// instant a fill counts by its size, like VWAP.
//
// Requirements:
//
//   - trades must be sorted by Timestamp, ascending. The function
//     does NOT sort internally — doing so silently would hide
//     caller bugs. If trades are unsorted, results are meaningless.
//   - windowEnd must be ≥ the last trade's timestamp. A windowEnd
//     earlier than the last trade's timestamp means the final
//     instant's slot is negative; we clamp to zero for that slot
//     rather than return an error, but ordering upstream is still
//     a bug.
//
// Returns [ErrNoTrades] for an empty slice or when the total
// duration is zero (every trade at the exact same timestamp as
// windowEnd and each other).
//
// Formula: TWAP = Σ(price_k × Δt_k) / Σ(Δt_k) over instants k, where
// Δt_k is the duration instant k's price was "current."
//
// Trades with zero base or quote volume are skipped — they have no
// defined price. An instant holding only such trades still ends the
// previous instant's slot and contributes no weight of its own.
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

	weighted := 0
	for i := 0; i < len(trades); {
		j := i + 1
		for j < len(trades) && trades[j].Timestamp.Equal(trades[i].Timestamp) {
			j++
		}
		end := windowEnd
		if j < len(trades) {
			end = trades[j].Timestamp
		}
		dur := end.Sub(trades[i].Timestamp)
		base, quote, priced := instantVolumes(trades[i:j])
		i = j
		if dur <= 0 || priced == 0 {
			continue
		}

		// Accumulate ⌊Σquote × SCALE × Δt / Σbase⌋ as a fixed-point
		// big.Int rather than adding an exact big.Rat per instant.
		//
		// Weight = Δt in nanoseconds (integer). Scaling by the same
		// factor on top + bottom, it cancels in the final division —
		// so raw nanoseconds is a valid weight choice.
		scratch.Mul(quote, twapScale)
		scratch.Mul(scratch, big.NewInt(int64(dur)))
		scratch.Quo(scratch, base)
		weightedSum.Add(weightedSum, scratch)
		totalNanos.Add(totalNanos, big.NewInt(int64(dur)))
		weighted += priced
	}

	// Sign() <= 0, not == 0. Every Δt added above is strictly positive
	// (the dur <= 0 continue), so a non-positive total is only reachable
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

// instantVolumes sums the base and quote legs of the priced trades in
// one instant (both legs positive) and counts them.
func instantVolumes(group []canonical.Trade) (base, quote *big.Int, priced int) {
	base, quote = new(big.Int), new(big.Int)
	for k := range group {
		b := group[k].BaseAmount.BigInt()
		q := group[k].QuoteAmount.BigInt()
		if b.Sign() <= 0 || q.Sign() <= 0 {
			continue
		}
		base.Add(base, b)
		quote.Add(quote, q)
		priced++
	}
	return base, quote, priced
}
