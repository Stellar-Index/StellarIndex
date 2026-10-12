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

func TestTWAP_ZeroBaseTradeAbstains(t *testing.T) {
	// The zero-base middle trade has no price, so it must not end the
	// prevailing price's slot: 100 stays current until the next PRICED
	// trade. Deleting its 30s slot from ΣΔt instead would give 150.
	t0 := time.Unix(0, 0).UTC()
	trades := []canonical.Trade{
		mkTradeAt(1, 100, t0),
		mkTradeAt(0, 999, t0.Add(30*time.Second)),
		mkTradeAt(1, 200, t0.Add(60*time.Second)),
	}
	// Price 100 active t=0..60s (60s), price 200 active 60..90s (30s).
	// TWAP = (100*60 + 200*30) / 90 = 400/3.
	got, err := aggregate.TWAP(trades, t0.Add(90*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got.Cmp(big.NewRat(400, 3)) != 0 {
		t.Errorf("TWAP = %v, want 400/3", got)
	}
}

func TestTWAP_UnpriceableTradeDoesNotDeleteItsTimeSlot(t *testing.T) {
	// 1 for 60s then 9 for 60s is 5.00 whatever unpriceable records
	// (zero base, negative quote) sit between them.
	t0 := time.Unix(0, 0).UTC()
	trades := []canonical.Trade{
		mkTradeAt(1, 1, t0),
		mkTradeAt(0, 7, t0.Add(30*time.Second)),
		mkTradeAt(1, -3, t0.Add(45*time.Second)),
		mkTradeAt(1, 9, t0.Add(60*time.Second)),
	}
	got, err := aggregate.TWAP(trades, t0.Add(120*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got.Cmp(big.NewRat(5, 1)) != 0 {
		t.Errorf("TWAP = %v, want 5", got)
	}
}

func TestTWAP_DescendingInputReturnsErrUnsortedTrades(t *testing.T) {
	// Newest-first input: every interior Δt is negative, so without the
	// ordering check the oldest trade (price 1) takes the whole window
	// and is returned as the average. The true TWAP of this data is 5.
	t0 := time.Unix(0, 0).UTC()
	trades := []canonical.Trade{
		mkTradeAt(1, 9, t0.Add(60*time.Second)),
		mkTradeAt(1, 1, t0),
	}
	got, err := aggregate.TWAP(trades, t0.Add(120*time.Second))
	if !errors.Is(err, aggregate.ErrUnsortedTrades) {
		t.Fatalf("TWAP = %v, err = %v; want ErrUnsortedTrades", got, err)
	}
	if got != nil {
		t.Errorf("TWAP returned price %v alongside ErrUnsortedTrades", got)
	}
}

func TestTWAP_OutOfOrderUnpriceableTradeIsStillUnsorted(t *testing.T) {
	// Ordering is checked over every record, priced or not: an
	// out-of-order row is a caller bug regardless of its amounts.
	t0 := time.Unix(0, 0).UTC()
	trades := []canonical.Trade{
		mkTradeAt(1, 1, t0.Add(10*time.Second)),
		mkTradeAt(0, 5, t0),
		mkTradeAt(1, 9, t0.Add(60*time.Second)),
	}
	if _, err := aggregate.TWAP(trades, t0.Add(120*time.Second)); !errors.Is(err, aggregate.ErrUnsortedTrades) {
		t.Fatalf("err = %v, want ErrUnsortedTrades", err)
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

// TestTWAP_farFutureWindowEndCannotProduceNegativePrice is a saturation regression: time.Time.Sub saturates at MaxInt64, so a far-future windowEnd once wrapped an int64 sum negative and flipped the published price's sign.
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

// twapPathologicalTrades builds the shape that makes a big.Rat accumulator super-linear: every trade has a DISTINCT base amount,
// so each Add accretes the LCM of all denominators so far. Prime-ish bases maximise that growth.
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

// TestTWAP_PathologicalInputStaysLinear pins the CPU-exhaustion class shut: a big.Rat accumulator's LCM growth took ~60s at the handler's 10000-trade cap; this asserts the complexity class, not a latency SLO.
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

// TestTWAP_FixedPointMatchesExactRational proves the fixed-point accumulator agrees with the exact rational to 10^-30 (bound 10^-40), far beyond the ~10 served places.
func TestTWAP_FixedPointMatchesExactRational(t *testing.T) {
	trades := twapPathologicalTrades(300)
	windowEnd := trades[len(trades)-1].Timestamp.Add(time.Second)

	got, err := aggregate.TWAP(trades, windowEnd)
	if err != nil {
		t.Fatal(err)
	}

	// Exact reference: Σ(quote_i/base_i × Δt_i) / Σ(Δt_i) in big.Rat,
	// the big.Rat algorithm, computed here at a size where it is cheap.
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

// TestTWAP_SameInstantFillsWeightedBySize pins the per-instant rule: same-timestamp fills set the instant's price by summed volume, independent of sort order within the ledger.
func TestTWAP_SameInstantFillsWeightedBySize(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	bulk := mkTradeAt(999_999_998, 999_999_998, t0) // price 1
	dust := mkTradeAt(2, 15, t0)                    // price 7.5, 2 stroops
	next := mkTradeAt(1_000_000, 1_000_000, t0.Add(10*time.Second))
	end := t0.Add(20 * time.Second)

	// Instant t0: Σquote/Σbase = 1_000_000_013 / 1_000_000_000, for 10s;
	// then 1 for 10s. TWAP = (1.000000013 + 1) / 2 = 1.0000000065.
	want := big.NewRat(2_000_000_013, 2_000_000_000)
	for name, trades := range map[string][]canonical.Trade{
		"dust sorts last":  {bulk, dust, next},
		"dust sorts first": {dust, bulk, next},
	} {
		got, err := aggregate.TWAP(trades, end)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Cmp(want) != 0 {
			t.Errorf("%s: TWAP = %s, want %s (the dust fill must count by its size)",
				name, got.FloatString(12), want.FloatString(12))
		}
	}
}

// TestTWAPWithCount_CountsOnlyWeightCarryingTrades pins that an unpriced trade and one whose slot is empty carry no weight and are not counted as support.
func TestTWAPWithCount_CountsOnlyWeightCarryingTrades(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	end := t0.Add(20 * time.Second)
	trades := []canonical.Trade{
		mkTradeAt(1, 100, t0),
		mkTradeAt(0, 999, t0), // unpriced
		mkTradeAt(1, 300, t0.Add(10*time.Second)),
		mkTradeAt(1, 900, end), // zero-length slot
	}
	got, n, err := aggregate.TWAPWithCount(trades, end)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cmp(big.NewRat(200, 1)) != 0 {
		t.Errorf("TWAP = %s, want 200", got.FloatString(6))
	}
	if n != 2 {
		t.Errorf("weighted trades = %d, want 2", n)
	}
}
