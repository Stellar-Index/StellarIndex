package cometlp_test

import (
	"errors"
	"math/big"
	"math/rand/v2"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/cometlp"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
)

var oneLP = big.NewInt(10_000_000)

// mustValue unwraps a valuation the test built to be valid; an error there is a broken test, so it panics.
func mustValue(v cometlp.Value, err error) cometlp.Value {
	if err != nil {
		panic(err)
	}
	return v
}

func amountIs(t *testing.T, what string, v cometlp.Value, want int64, reason string) {
	t.Helper()
	if v.Amount == nil || v.Amount.Cmp(big.NewInt(want)) != 0 || v.Reason != reason {
		t.Errorf("%s = %v (reason %q), want %d (reason %q)", what, v.Amount, v.Reason, want, reason)
	}
}

// Golden values at the lake tip, from the real entries.
func TestFloorImplied_GoldenValues(t *testing.T) {
	// BLNT/USDC: b_q = 38919029596, S = 3796905613114, w_q = 2e6, backstop B = 3790078201308.
	// F_B = ⌊38919029596·3790078201308 / 3796905613114⌋ = 38849047281 (remainder 2500418268534) = 3,884.9047281 USDC.
	// I_B = ⌊38919029596·3790078201308·1e7 / (3796905613114·2e6)⌋ = 194245236408 = 19,424.5236408 USDC.
	// Per LP: ⌊38919029596·1e7 / 3796905613114⌋ = 102501 = 0.0102501 USDC; implied 512509 = 0.0512509 USDC.
	usdc := fixtureState(t, comet.MainnetBLNTUSDCPool)
	b := fixtureHolding(t, comet.MainnetBLNTUSDCPool, blntBackstop)
	amountIs(t, "BLNT/USDC backstop floor", mustValue(cometlp.Floor(usdc, usdcSAC, b)), 38_849_047_281, "")
	amountIs(t, "BLNT/USDC backstop implied", mustValue(cometlp.Implied(usdc, usdcSAC, b)), 194_245_236_408, "")
	amountIs(t, "BLNT/USDC floor per LP", mustValue(cometlp.Floor(usdc, usdcSAC, oneLP)), 102_501, "")
	amountIs(t, "BLNT/USDC implied per LP", mustValue(cometlp.Implied(usdc, usdcSAC, oneLP)), 512_509, "")

	// BLNT/XLM: b_q = 525560728083, S = 6753401388209, backstop B = 6742862017325.
	// F_B = ⌊525560728083·6742862017325 / 6753401388209⌋ = 524740537024 = 52,474.0537024 XLM; I_B = 2623702685120.
	// Per LP: ⌊525560728083·1e7 / 6753401388209⌋ = 778216 = 0.0778216 XLM.
	xlm := fixtureState(t, comet.MainnetBLNTXLMPool)
	bx := fixtureHolding(t, comet.MainnetBLNTXLMPool, blntBackstop)
	amountIs(t, "BLNT/XLM backstop floor", mustValue(cometlp.Floor(xlm, xlmSAC, bx)), 524_740_537_024, "")
	amountIs(t, "BLNT/XLM backstop implied", mustValue(cometlp.Implied(xlm, xlmSAC, bx)), 2_623_702_685_120, "")
	amountIs(t, "BLNT/XLM floor per LP", mustValue(cometlp.Floor(xlm, xlmSAC, oneLP)), 778_216, "")

	f := mustValue(cometlp.Floor(usdc, usdcSAC, b))
	if f.Method != cometlp.MethodFloor || !f.LowerBound || f.Approximate {
		t.Errorf("floor flags = %+v, want a non-approximate lower bound", f)
	}
	if i := mustValue(cometlp.Implied(usdc, usdcSAC, b)); i.Method != cometlp.MethodImplied || !i.Approximate {
		t.Errorf("implied flags = %+v, want approximate", i)
	}
}

// The drained Blend pool: 0.4351707 USDC against 1.862e27 LP base units. Its backstop's floor is
// 4351707·3944917012935820561 / 1862083261311633634135993981 = 0.0092 stroops: real, and below one stroop.
func TestFloor_DrainedPoolIsZeroBelowPrecision(t *testing.T) {
	st := fixtureState(t, comet.MainnetBackstopPool)
	b := fixtureHolding(t, comet.MainnetBackstopPool, blendBackstopV2)
	amountIs(t, "Blend backstop floor", mustValue(cometlp.Floor(st, usdcSAC, b)), 0, cometlp.ReasonBelowPrecision)
	amountIs(t, "Blend floor per LP", mustValue(cometlp.Floor(st, usdcSAC, oneLP)), 0, cometlp.ReasonBelowPrecision)
	amountIs(t, "Blend implied", mustValue(cometlp.Implied(st, usdcSAC, b)), 0, cometlp.ReasonBelowPrecision)
	amountIs(t, "no shares", mustValue(cometlp.Floor(st, usdcSAC, new(big.Int))), 0, "")
	v, err := cometlp.Valuate(st, usdcSAC, b, cometlp.Prices{Quote: cometlp.StableLowerBound(big.NewRat(1, 1))})
	if err != nil {
		t.Fatal(err)
	}
	if v.Floor.Amount.Sign() != 0 || v.Floor.Reason != cometlp.ReasonBelowPrecision || !v.Floor.LowerBound {
		t.Errorf("Blend USD floor = %+v, want 0 below precision, a lower bound", v.Floor)
	}
	// Spot = 4351707·8e6 / (4274375264·2e6) ≈ 0.0040724 USDC per BLND.
	spot, err := cometlp.PoolSpot(st, usdcSAC)
	if err != nil || spot.Cmp(big.NewRat(4351707*4, 4274375264)) != 0 {
		t.Errorf("Blend spot = %v (%v)", spot, err)
	}
}

// synth is a 20/80 pool with quote q and other leg x at 7 decimals.
func synth(bq, bx, s *big.Int) cometlp.PoolState {
	rec := func(tok string, b *big.Int, w int64, i uint32) cometlp.Record {
		return cometlp.Record{Token: tok, Balance: b, Weight: big.NewInt(w), Scalar: big.NewInt(100_000_000_000), Index: i}
	}
	return cometlp.PoolState{
		Pool:        comet.MainnetBLNTUSDCPool,
		Records:     []cometlp.Record{rec(blntSAC, bx, 8_000_000, 0), rec(usdcSAC, bq, 2_000_000, 1)},
		TotalShares: s,
		Instance:    cometlp.Instance{WASM: cometlp.WASMV110, SwapFee: big.NewInt(30_000), Controller: "G"},
	}
}

func TestQuoteLegs_RefusesInvalidSamples(t *testing.T) {
	one := big.NewInt(1)
	bad := map[string]cometlp.PoolState{
		"zero supply":  synth(one, one, new(big.Int)),
		"zero quote":   synth(new(big.Int), one, one),
		"negative leg": synth(one, big.NewInt(-1), one),
		"weights": func() cometlp.PoolState {
			s := synth(one, one, one)
			s.Records[0].Weight = big.NewInt(7_000_000)
			return s
		}(),
		"scalar":       func() cometlp.PoolState { s := synth(one, one, one); s.Records[1].Scalar = big.NewInt(1); return s }(),
		"no quote leg": func() cometlp.PoolState { s := synth(one, one, one); s.Records[1].Token = xlmSAC; return s }(),
		"three records": func() cometlp.PoolState {
			s := synth(one, one, one)
			s.Records = append(s.Records, s.Records[0])
			return s
		}(),
	}
	for name, st := range bad {
		if _, err := cometlp.Floor(st, usdcSAC, oneLP); !errors.Is(err, cometlp.ErrLPStateInvalid) {
			t.Errorf("%s: err = %v, want ErrLPStateInvalid", name, err)
		}
	}
	if _, err := cometlp.Floor(synth(one, one, one), usdcSAC, big.NewInt(-1)); !errors.Is(err, cometlp.ErrLPStateInvalid) {
		t.Errorf("negative shares: err = %v", err)
	}
}

func TestFloor_Properties(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic property inputs
	n := func(limit int64) *big.Int { return big.NewInt(r.Int64N(limit) + 1) }
	for range 2000 {
		bq, bx, s := n(1e15), n(1e15), n(1e15)
		b := new(big.Int).Mod(n(1e15), s)
		st := synth(bq, bx, s)
		pq, px := big.NewRat(r.Int64N(1e6), 1e4), big.NewRat(r.Int64N(1e6), 1e4)
		v, err := cometlp.Valuate(st, usdcSAC, b, cometlp.Prices{Quote: pq, Other: px})
		if err != nil {
			t.Fatal(err)
		}
		if v.Floor.Amount.Cmp(v.Marked.Amount) > 0 || v.Floor.Amount.Cmp(v.Implied.Amount) > 0 {
			t.Fatalf("floor %s above marked %s or implied %s (bq %s bx %s S %s B %s)", v.Floor.Amount, v.Marked.Amount, v.Implied.Amount, bq, bx, s, b)
		}
		f := mustValue(cometlp.Floor(st, usdcSAC, b))
		more := mustValue(cometlp.Floor(synth(new(big.Int).Add(bq, n(1e9)), bx, s), usdcSAC, b))
		if more.Amount.Cmp(f.Amount) < 0 {
			t.Fatalf("floor fell when the quote balance rose: %s -> %s", f.Amount, more.Amount)
		}
		k := n(1e12)
		scaled := mustValue(cometlp.Floor(synth(new(big.Int).Mul(bq, k), new(big.Int).Mul(bx, k), new(big.Int).Mul(s, k)), usdcSAC, new(big.Int).Mul(b, k)))
		// Scaling the supply and the holding together leaves the exit ratio, so the floor, unchanged.
		same := mustValue(cometlp.Floor(synth(bq, bx, new(big.Int).Mul(s, k)), usdcSAC, new(big.Int).Mul(b, k)))
		if scaled.Amount.Cmp(new(big.Int).Mul(f.Amount, k)) < 0 || same.Amount.Cmp(f.Amount) != 0 {
			t.Fatalf("floor is not scale invariant at k=%s", k)
		}
		// A true lower bound: the floor times S never exceeds b_q·B.
		if new(big.Int).Mul(f.Amount, s).Cmp(new(big.Int).Mul(bq, b)) > 0 {
			t.Fatalf("floor %s rounds above b_q·B/S", f.Amount)
		}
	}
}

// evenPool is a 20/80 pool whose spot is exactly 1 quote per other token: b_q·w_x = b_x·w_q.
func evenPool() cometlp.PoolState {
	return synth(big.NewInt(20_000_000), big.NewInt(80_000_000), big.NewInt(100_000_000))
}

func TestValuate_MethodByDivergence(t *testing.T) {
	st, one := evenPool(), big.NewRat(1, 1)
	// ρ = P_x/(p_pool·P_q) = 1.025: w_x·|ρ−1| = 0.8·0.025 = 2/100 exactly, which passes.
	at := func(px *big.Rat) cometlp.Valuation {
		v, err := cometlp.Valuate(st, usdcSAC, oneLP, cometlp.Prices{Quote: one, Other: px})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := at(big.NewRat(1025, 1000)); v.Headline.Method != cometlp.MethodMarked || v.Headline.Approximate {
		t.Errorf("2%% boundary: headline %+v, want marked", v.Headline)
	}
	off := at(big.NewRat(10251, 10000))
	if off.Headline.Method != cometlp.MethodImplied || !off.Headline.Approximate || off.Headline.Reason != cometlp.ReasonPoolOffMarket {
		t.Errorf("past 2%%: headline %+v, want implied, approximate, pool_off_market", off.Headline)
	}
	// One LP of 1e8 shares owns 0.2 quote and 0.8 other: marked = 0.2 + 0.8·1.0251 = 1.02008 = 10200800 units.
	amountIs(t, "marked past 2%", off.Marked, 10_200_800, "")
	amountIs(t, "floor", off.Floor, 2_000_000, "")
	amountIs(t, "implied", off.Implied, 10_000_000, "")
	none, err := cometlp.Valuate(st, usdcSAC, oneLP, cometlp.Prices{Quote: one})
	if err != nil {
		t.Fatal(err)
	}
	if none.Marked.Amount != nil || none.Marked.Reason != cometlp.ReasonNoIndependentPrice || none.Headline.Reason != cometlp.ReasonNoIndependentPrice || !none.Headline.Approximate {
		t.Errorf("no independent price: marked %+v headline %+v", none.Marked, none.Headline)
	}
	unpriced, err := cometlp.Valuate(st, usdcSAC, oneLP, cometlp.Prices{})
	if err != nil || unpriced.Floor.Amount != nil || unpriced.Headline.Reason != cometlp.ReasonUnpriced {
		t.Errorf("no quote price: %+v (%v)", unpriced, err)
	}
	for _, p := range []*big.Rat{new(big.Rat), big.NewRat(-1, 1)} {
		z, err := cometlp.Valuate(st, usdcSAC, oneLP, cometlp.Prices{Quote: p, Other: one})
		if err != nil || z.Floor.Amount != nil || z.Floor.Reason != cometlp.ReasonUnpriced || z.Headline.Reason != cometlp.ReasonUnpriced {
			t.Errorf("quote price %s: %+v (%v), want unpriced", p, z, err)
		}
	}
	if got := cometlp.StableLowerBound(big.NewRat(1001, 1000)); got.Cmp(one) != 0 {
		t.Errorf("StableLowerBound(1.001) = %s", got)
	}
	// The divergence check reads the market quote price, the floor its lower bound: at P_q = 1.03 and P_x = 1 the pool
	// is 2.9% off (ρ = 1/1.03), though at the clamped 1 it would look exactly on market.
	pq := big.NewRat(103, 100)
	cl, err := cometlp.Valuate(st, usdcSAC, oneLP, cometlp.Prices{Quote: pq, QuoteLowerBound: cometlp.StableLowerBound(pq), Other: one})
	if err != nil || cl.Headline.Reason != cometlp.ReasonPoolOffMarket {
		t.Errorf("unclamped divergence: headline %+v (%v), want pool_off_market", cl.Headline, err)
	}
	amountIs(t, "floor at the clamped quote price", cl.Floor, 2_000_000, "")
}

// The floor is exactly exit_pool's quote-leg payout, ⌊b_q·⌊B·1e18/S⌋/1e18⌋, never the one-step ⌊b_q·B/S⌋ above it.
func TestFloor_IsTheContractsExitPayout(t *testing.T) {
	amountIs(t, "b=3 B=1 S=3", mustValue(cometlp.Floor(synth(big.NewInt(3), big.NewInt(3), big.NewInt(3)), usdcSAC, big.NewInt(1))), 0, cometlp.ReasonBelowPrecision)
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	r := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // deterministic property inputs
	for range 5000 {
		bq, s := big.NewInt(r.Int64N(1e18)+1), big.NewInt(r.Int64N(1e18)+1)
		b := big.NewInt(r.Int64N(s.Int64()) + 1)
		ratio := new(big.Int).Div(new(big.Int).Mul(b, e18), s)
		want := new(big.Int).Div(new(big.Int).Mul(bq, ratio), e18)
		got := mustValue(cometlp.Floor(synth(bq, big.NewInt(1), s), usdcSAC, b))
		if got.Amount.Cmp(want) != 0 {
			t.Fatalf("floor(bq %s, B %s, S %s) = %s, exit_pool pays %s", bq, b, s, got.Amount, want)
		}
	}
}

func TestValuate_FrozenServesOnlyTheFloor(t *testing.T) {
	st := evenPool()
	st.Instance.Freeze = true
	v, err := cometlp.Valuate(st, usdcSAC, oneLP, cometlp.Prices{Quote: big.NewRat(1, 1), Other: big.NewRat(1, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if v.Implied.Amount != nil || v.Marked.Amount != nil || v.Implied.Reason != cometlp.ReasonPoolFrozen || v.Marked.Reason != cometlp.ReasonPoolFrozen {
		t.Errorf("frozen implied %+v marked %+v, want both null with pool_frozen", v.Implied, v.Marked)
	}
	if v.Headline.Method != cometlp.MethodFloor || v.Headline.Amount.Int64() != 2_000_000 {
		t.Errorf("frozen headline = %+v, want the floor", v.Headline)
	}
}

func TestLowerMedian(t *testing.T) {
	rats := func(xs ...int64) []*big.Rat {
		out := make([]*big.Rat, len(xs))
		for i, x := range xs {
			out[i] = big.NewRat(x, 1)
		}
		return out
	}
	for _, tc := range []struct {
		xs   []*big.Rat
		want int
	}{
		{nil, -1},
		{rats(5), 0},
		{rats(3, 1, 2), 2},    // sorted 1 2 3: element 1 is the 2 at index 2
		{rats(4, 1, 3, 2), 3}, // even: sorted 1 2 3 4, element ⌊3/2⌋ = 1, the lower middle 2
		{[]*big.Rat{big.NewRat(1, 3), big.NewRat(1, 2)}, 0},
	} {
		if got := cometlp.LowerMedian(tc.xs); got != tc.want {
			t.Errorf("LowerMedian(%v) = %d, want %d", tc.xs, got, tc.want)
		}
	}
}

func TestDailyValue(t *testing.T) {
	v := func(n int64, m cometlp.Method, approx bool) cometlp.Value {
		return cometlp.Value{Amount: big.NewInt(n), Method: m, Approximate: approx}
	}
	off := v(30, cometlp.MethodImplied, true)
	off.Reason = cometlp.ReasonPoolOffMarket
	hours := []cometlp.Value{v(10, cometlp.MethodMarked, false), off, v(20, cometlp.MethodMarked, false), v(40, cometlp.MethodMarked, false)}
	got := cometlp.DailyValue(hours)
	if got.Amount.Int64() != 20 || got.Method != cometlp.MethodMarked || !got.Approximate || got.Reason != cometlp.ReasonPoolOffMarket {
		t.Errorf("daily = %+v, want 20 marked, approximate with the off-market hour's reason", got)
	}
	// A frozen hour serves a lower-bound floor; a marked median among it is not a lower bound.
	frozen := cometlp.Value{Amount: big.NewInt(5), Method: cometlp.MethodFloor, LowerBound: true}
	mixed := cometlp.DailyValue([]cometlp.Value{frozen, v(20, cometlp.MethodMarked, false), v(30, cometlp.MethodMarked, false)})
	if mixed.Amount.Int64() != 20 || mixed.LowerBound || mixed.Approximate {
		t.Errorf("marked median beside a frozen floor hour = %+v, want 20, not a lower bound", mixed)
	}
	gap := cometlp.DailyValue([]cometlp.Value{v(10, cometlp.MethodMarked, false), {Method: cometlp.MethodMarked, Reason: cometlp.ReasonPoolFrozen}})
	if gap.Amount.Int64() != 10 || !gap.Approximate || gap.Reason != cometlp.ReasonPoolFrozen {
		t.Errorf("daily with a null hour = %+v", gap)
	}
	if empty := cometlp.DailyValue(nil); empty.Amount != nil {
		t.Errorf("empty day = %+v", empty)
	}
}
