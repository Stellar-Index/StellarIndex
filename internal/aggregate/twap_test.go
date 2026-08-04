package aggregate_test

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// mkTradeAt is mkTrade with a custom timestamp.
func mkTradeAt(base, quote int64, ts time.Time) canonical.Trade {
	t := mkTrade(base, quote)
	t.Timestamp = ts
	return t
}

func TestTWAP_SingleIntervalUniform(t *testing.T) {
	// Two trades at t=0, t=60s, both priced at 100. windowEnd=120s.
	// First price active for 60s, second for 60s. TWAP = 100.
	t0 := time.Unix(0, 0).UTC()
	trades := []canonical.Trade{
		mkTradeAt(1, 100, t0),
		mkTradeAt(1, 100, t0.Add(60*time.Second)),
	}
	got, err := aggregate.TWAP(trades, t0.Add(120*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got.Cmp(big.NewRat(100, 1)) != 0 {
		t.Errorf("TWAP = %v, want 100", got)
	}
}

func TestTWAP_TimeWeighting(t *testing.T) {
	// Price 100 active for 10s, price 200 active for 30s.
	// TWAP = (100×10 + 200×30) / 40 = 7000/40 = 175.
	t0 := time.Unix(0, 0).UTC()
	trades := []canonical.Trade{
		mkTradeAt(1, 100, t0),                     // 100 for 10s
		mkTradeAt(1, 200, t0.Add(10*time.Second)), // 200 for 30s
	}
	got, err := aggregate.TWAP(trades, t0.Add(40*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	want := big.NewRat(175, 1)
	if got.Cmp(want) != 0 {
		t.Errorf("TWAP = %v, want 175", got)
	}
}

func TestTWAP_EmptyReturnsErr(t *testing.T) {
	_, err := aggregate.TWAP(nil, time.Now())
	if !errors.Is(err, aggregate.ErrNoTrades) {
		t.Fatalf("err = %v, want ErrNoTrades", err)
	}
}

func TestTWAP_AllZeroDurationReturnsErr(t *testing.T) {
	// windowEnd equals the single trade's timestamp → zero duration.
	t0 := time.Unix(100, 0).UTC()
	_, err := aggregate.TWAP([]canonical.Trade{mkTradeAt(1, 100, t0)}, t0)
	if !errors.Is(err, aggregate.ErrNoTrades) {
		t.Fatalf("err = %v, want ErrNoTrades (zero-duration window)", err)
	}
}

func TestTWAP_SkipsZeroBaseTrades(t *testing.T) {
	// The zero-base middle trade must not contribute to either the
	// weighted sum or the duration accumulator.
	t0 := time.Unix(0, 0).UTC()
	trades := []canonical.Trade{
		mkTradeAt(1, 100, t0),
		mkTradeAt(0, 999, t0.Add(30*time.Second)),
		mkTradeAt(1, 200, t0.Add(60*time.Second)),
	}
	// Price 100 active t=0..30s (30s), skipped slot 30..60s,
	// price 200 active 60..90s (30s).
	// TWAP = (100*30 + 200*30) / 60 = 150.
	got, err := aggregate.TWAP(trades, t0.Add(90*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got.Cmp(big.NewRat(150, 1)) != 0 {
		t.Errorf("TWAP = %v, want 150", got)
	}
}

func TestTWAP_NonPositiveFinalDurationClamps(t *testing.T) {
	// windowEnd BEFORE the last trade's timestamp means the last
	// trade's slot is negative — we clamp to zero rather than error,
	// so the TWAP reflects only the prior trades.
	t0 := time.Unix(0, 0).UTC()
	trades := []canonical.Trade{
		mkTradeAt(1, 100, t0),                     // 100 for 10s
		mkTradeAt(1, 500, t0.Add(10*time.Second)), // late trade, ignored
	}
	got, err := aggregate.TWAP(trades, t0.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// Only the first trade contributed: price 100 × 10s / 10s = 100.
	if got.Cmp(big.NewRat(100, 1)) != 0 {
		t.Errorf("TWAP = %v, want 100 (late trade clamped)", got)
	}
}

// TestTWAP_farFutureWindowEndCannotProduceNegativePrice is the
// regression test for the cold audit of 2026-08-04.
//
// totalNanos was an int64 of nanoseconds. time.Time.Sub SATURATES at
// MaxInt64 (~292.47 years), so a far-future windowEnd produced a
// saturated Δt and adding any further positive interval wrapped the sum
// NEGATIVE — the final division then flipped the sign of the published
// price. The guard tested only for zero, so the negative sailed through.
//
// Reproduced against production before the fix:
//
//	/v1/twap?base=native&quote=fiat:USD&from=2026-08-01&to=9999-12-31
//	-> 200 {"price":"-0.1702543997","flags":{"stale":false}}
//
// The API puts no upper bound on an explicit `to`, so any client using a
// far-future sentinel for "no end bound" got a negative money string on a
// success response.
func TestTWAP_farFutureWindowEndCannotProduceNegativePrice(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	trades := []canonical.Trade{
		mkTradeAt(100, 10, base),
		mkTradeAt(100, 12, base.Add(time.Hour)),
	}

	for _, end := range []time.Time{
		time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2400, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2319, 6, 1, 0, 0, 0, 0, time.UTC), // just past the ~292y Duration ceiling
	} {
		got, err := aggregate.TWAP(trades, end)
		if err != nil {
			continue // refusing is an acceptable answer; a negative is not
		}
		if got.Sign() <= 0 {
			t.Errorf("TWAP(windowEnd=%s) = %s — a non-positive price is never a valid answer; the nanosecond accumulator wrapped",
				end.Format(time.RFC3339), got.RatString())
		}
	}
}

// twapPathologicalTrades builds the shape that made the old big.Rat
// accumulator super-linear: every trade carries a DISTINCT base amount, so
// each Add accretes the LCM of all denominators seen so far. Prime-ish
// bases maximise that growth.
func twapPathologicalTrades(n int) []canonical.Trade {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	trades := make([]canonical.Trade, 0, n)
	for i := range n {
		base := int64(2*i + 1_000_003)
		quote := base*3 + int64(i)
		trades = append(trades, mkTradeAt(base, quote, t0.Add(time.Duration(i)*time.Second)))
	}
	return trades
}

// TestTWAP_PathologicalInputStaysLinear pins the CPU-exhaustion class shut.
//
// The accumulator used to be a big.Rat, whose denominator grows as the LCM
// of every trade's base amount. At the /v1/twap handler's own maxTrades
// cap of 10000 that cost 60.6s of pure CPU for one unauthenticated request
// (measured 7.47 CPU-seconds against production, and journalctl already
// held three 111-second 200s with 296-byte bodies). aggregate.TWAP takes
// no ctx and RequestTimeout only injects a deadline without aborting the
// handler goroutine, so nothing could reclaim it.
//
// The bound here is deliberately loose — this asserts the COMPLEXITY CLASS,
// not a latency SLO. Anything still quadratic blows a 10s budget by ~6x on
// the pre-fix code while the linear version lands in single-digit ms.
func TestTWAP_PathologicalInputStaysLinear(t *testing.T) {
	const handlerMaxTrades = 10_000
	trades := twapPathologicalTrades(handlerMaxTrades)
	windowEnd := trades[len(trades)-1].Timestamp.Add(time.Second)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := aggregate.TWAP(trades, windowEnd); err != nil {
			t.Errorf("TWAP: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("TWAP over 10000 distinct-base trades did not finish in 10s — " +
			"the super-linear accumulator is back; one anonymous GET burns this much CPU")
	}
}

// TestTWAP_FixedPointMatchesExactRational proves the fixed-point
// accumulator agrees with the exact rational computation it replaced, to
// far more precision than the value is ever serialised at.
//
// Error model: each term truncates by <1 unit in the last place and every
// Δt is >=1ns, so the relative error of the quotient is bounded by
// 1/twapScale = 10^-40. Assert 10^-30 — comfortably inside that bound and
// twenty orders below the ~10 decimal places /v1/twap publishes.
func TestTWAP_FixedPointMatchesExactRational(t *testing.T) {
	trades := twapPathologicalTrades(300)
	windowEnd := trades[len(trades)-1].Timestamp.Add(time.Second)

	got, err := aggregate.TWAP(trades, windowEnd)
	if err != nil {
		t.Fatal(err)
	}

	// Exact reference: Σ(quote_i/base_i × Δt_i) / Σ(Δt_i) in big.Rat,
	// the pre-fix algorithm, computed here at a size where it is cheap.
	want := new(big.Rat)
	totalNanos := new(big.Int)
	for i := range trades {
		var dur time.Duration
		if i == len(trades)-1 {
			dur = windowEnd.Sub(trades[i].Timestamp)
		} else {
			dur = trades[i+1].Timestamp.Sub(trades[i].Timestamp)
		}
		price := new(big.Rat).SetFrac(trades[i].QuoteAmount.BigInt(), trades[i].BaseAmount.BigInt())
		price.Mul(price, big.NewRat(int64(dur), 1))
		want.Add(want, price)
		totalNanos.Add(totalNanos, big.NewInt(int64(dur)))
	}
	want.Quo(want, new(big.Rat).SetInt(totalNanos))

	diff := new(big.Rat).Sub(got, want)
	diff.Abs(diff)
	rel := new(big.Rat).Quo(diff, want)
	tol := new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil))
	if rel.Cmp(tol) > 0 {
		t.Errorf("relative error %s exceeds 1e-30\n got  = %s\n want = %s",
			rel.FloatString(45), got.FloatString(30), want.FloatString(30))
	}
}
