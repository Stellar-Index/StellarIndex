package cometlp

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
)

// LP valuation of a two-token Comet pool at one sampled state (docs/operations/wasm-audits/comet.md). Every amount is
// an integer of 1e-7 units, of the quote token or of USD: all curated tokens and the LP share have 7 decimals, so a
// quote stroop times a USD-per-token price is already in 1e-7 USD. Every division rounds down.

// Method is how a Value was computed.
type Method string

const (
	// MethodFloor counts only the quote leg: a lower bound of what the shares redeem for at the sampled state.
	MethodFloor Method = "floor"
	// MethodImplied is the floor divided by the quote weight. It assumes the pool is at arbitrage equilibrium and carries
	// no information beyond the floor: their ratio is the constant 1e7/w_q.
	MethodImplied Method = "implied"
	// MethodMarked prices both legs at external prices.
	MethodMarked Method = "marked"
)

// Reasons a Value is absent, approximate or rounded to zero (the lending usd_reason vocabulary).
const (
	ReasonLPStateInvalid     = "lp_state_invalid"
	ReasonBelowPrecision     = "lp_value_below_precision"
	ReasonPoolOffMarket      = "pool_off_market"
	ReasonNoIndependentPrice = "no_independent_price"
	ReasonPoolFrozen         = "pool_frozen"
	ReasonUnpriced           = "unpriced"
)

// ErrLPStateInvalid is a sample the formulas do not apply to; callers serve it as ReasonLPStateInvalid.
var ErrLPStateInvalid = errors.New("cometlp: " + ReasonLPStateInvalid)

// Value is one valuation. Amount is nil when it was not computed, and Reason then says why; Reason
// lp_value_below_precision marks a positive value that rounds down to 0.
type Value struct {
	Amount      *big.Int
	Method      Method
	Approximate bool
	LowerBound  bool
	Reason      string
}

const weightScale = 10_000_000

// maxDivergence is the largest w_x·|ρ−1| the marked value is served at.
var maxDivergence = big.NewRat(2, 100)

var sevenDecimalScalar = big.NewInt(100_000_000_000) // 10^(18−7)

// legs is a validated two-token sample: the quote record, the other record and the LP supply.
type legs struct {
	q, x Record
	s    *big.Int
}

// quoteLegs refuses a sample whose arithmetic the formulas do not cover: not two tokens, no quote leg, non-positive
// supply or quote balance, a negative other balance, weights not summing to 1e7, or a token not of 7 decimals.
func (p PoolState) quoteLegs(quote string) (legs, error) {
	if len(p.Records) != 2 {
		return legs{}, fmt.Errorf("%w: %s has %d tokens, want 2", ErrLPStateInvalid, p.Pool, len(p.Records))
	}
	l := legs{q: p.Records[0], x: p.Records[1], s: p.TotalShares}
	if l.x.Token == quote {
		l.q, l.x = l.x, l.q
	}
	switch {
	case l.q.Token != quote:
		return legs{}, fmt.Errorf("%w: %s has no %s leg", ErrLPStateInvalid, p.Pool, quote)
	case l.s == nil || l.s.Sign() <= 0:
		return legs{}, fmt.Errorf("%w: %s total shares %v", ErrLPStateInvalid, p.Pool, l.s)
	case l.q.Balance.Sign() <= 0 || l.x.Balance.Sign() < 0:
		return legs{}, fmt.Errorf("%w: %s balances %s, %s", ErrLPStateInvalid, p.Pool, l.q.Balance, l.x.Balance)
	case l.q.Weight.Sign() <= 0 || l.x.Weight.Sign() <= 0 ||
		new(big.Int).Add(l.q.Weight, l.x.Weight).Cmp(big.NewInt(weightScale)) != 0:
		return legs{}, fmt.Errorf("%w: %s weights %s + %s", ErrLPStateInvalid, p.Pool, l.q.Weight, l.x.Weight)
	case l.q.Scalar.Cmp(sevenDecimalScalar) != 0 || l.x.Scalar.Cmp(sevenDecimalScalar) != 0:
		return legs{}, fmt.Errorf("%w: %s scalars %s, %s", ErrLPStateInvalid, p.Pool, l.q.Scalar, l.x.Scalar)
	}
	return l, nil
}

// floorDiv is ⌊num/den⌋ for den > 0, as a Value of method m; a positive quotient that rounds to 0 says so.
func floorDiv(num, den *big.Int, v Value) Value {
	v.Amount = new(big.Int).Div(num, den)
	if v.Amount.Sign() == 0 && num.Sign() > 0 {
		v.Reason = ReasonBelowPrecision
	}
	return v
}

// bone is the 1e18 fixed point of the pool's exit math.
var bone = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

// Floor is the quote leg exit_pool pays for B shares, with the contract's own two floors (c_math.rs v1.1.0
// calc_exit_ratio and calc_exit_withdrawal_amount, unchanged from v1.0.0): ratio = ⌊B·1e18/S⌋, out = ⌊b_q·ratio/1e18⌋.
// It is never above the real redemption and at most ⌊b_q·B/S⌋. shares = 10^7 gives the value of one LP token.
func Floor(p PoolState, quote string, shares *big.Int) (Value, error) {
	l, err := p.quoteLegs(quote)
	if err != nil {
		return Value{}, err
	}
	if shares.Sign() < 0 {
		return Value{}, fmt.Errorf("%w: %s shares", ErrLPStateInvalid, shares)
	}
	ratio := new(big.Int).Div(new(big.Int).Mul(shares, bone), l.s)
	v := floorDiv(new(big.Int).Mul(l.q.Balance, ratio), bone, Value{Method: MethodFloor, LowerBound: true})
	if v.Amount.Sign() == 0 && shares.Sign() > 0 {
		v.Reason = ReasonBelowPrecision // exit_pool reverts here (ErrMathApprox): the claim is real but below one unit
	}
	return v, nil
}

// Implied is ⌊b_q·B·1e7/(S·w_q)⌋ quote base units: the floor scaled by the normalised quote weight. Always approximate.
func Implied(p PoolState, quote string, shares *big.Int) (Value, error) {
	l, err := p.quoteLegs(quote)
	if err != nil {
		return Value{}, err
	}
	if shares.Sign() < 0 {
		return Value{}, fmt.Errorf("%w: %s shares", ErrLPStateInvalid, shares)
	}
	num := new(big.Int).Mul(l.q.Balance, shares)
	num.Mul(num, big.NewInt(weightScale))
	den := new(big.Int).Mul(l.s, l.q.Weight)
	return floorDiv(num, den, Value{Method: MethodImplied, Approximate: true}), nil
}

// PoolSpot is the pool's sans-fee spot price of the other token in the quote token, (b_q·w_x)/(b_x·w_q); nil when the
// other balance is zero.
func PoolSpot(p PoolState, quote string) (*big.Rat, error) {
	l, err := p.quoteLegs(quote)
	if err != nil {
		return nil, err
	}
	if l.x.Balance.Sign() == 0 {
		return nil, nil
	}
	return new(big.Rat).SetFrac(new(big.Int).Mul(l.q.Balance, l.x.Weight), new(big.Int).Mul(l.x.Balance, l.q.Weight)), nil
}

// WithinMarket reports whether w_x·|ρ−1| ≤ 2/100, ρ = P_x/(p_pool·P_q): by the identity M/I = w_q + w_x·ρ, the marked
// value is then within 2% of the implied one. pq and px are USD per whole token and must come from markets that exclude
// this pool's trades, or ρ is 1 by construction.
func WithinMarket(p PoolState, quote string, pq, px *big.Rat) (bool, error) {
	l, err := p.quoteLegs(quote)
	if err != nil {
		return false, err
	}
	spot, err := PoolSpot(p, quote)
	if err != nil || spot == nil || pq.Sign() <= 0 || px.Sign() < 0 {
		return false, err
	}
	rho := new(big.Rat).Quo(px, new(big.Rat).Mul(spot, pq))
	dev := rho.Sub(rho, big.NewRat(1, 1))
	dev.Abs(dev).Mul(dev, new(big.Rat).SetFrac(l.x.Weight, big.NewInt(weightScale)))
	return dev.Cmp(maxDivergence) <= 0, nil
}

// Prices are USD per whole token. Quote is the quote token's market price; a missing or non-positive one leaves every
// USD value absent (unpriced). QuoteLowerBound, when set, prices the floor and implied values instead: the USD floor is
// a lower bound only if its price is, so for a stablecoin pass StableLowerBound(Quote). Other nil means no independent,
// non-thin price excluding this pool's trades.
type Prices struct {
	Quote, QuoteLowerBound, Other *big.Rat
}

// StableLowerBound is min(1, p): a dollar stablecoin's price as a lower bound.
func StableLowerBound(p *big.Rat) *big.Rat {
	if p.Cmp(big.NewRat(1, 1)) > 0 {
		return big.NewRat(1, 1)
	}
	return new(big.Rat).Set(p)
}

// Valuation is one holding's USD values (1e-7 USD) and the one to serve.
type Valuation struct {
	Floor, Implied, Marked Value
	Headline               Value
}

// Valuate values shares of the pool in USD. Frozen: only the floor (swaps are off, so no arbitrage pins the price).
// Otherwise marked when WithinMarket, else implied flagged approximate.
func Valuate(p PoolState, quote string, shares *big.Int, pr Prices) (Valuation, error) {
	fq, err := Floor(p, quote, shares)
	if err != nil {
		return Valuation{}, err
	}
	iq, err := Implied(p, quote, shares)
	if err != nil {
		return Valuation{}, err
	}
	pFloor := pr.QuoteLowerBound
	if pFloor == nil {
		pFloor = pr.Quote
	}
	if pr.Quote == nil || pr.Quote.Sign() <= 0 || pFloor.Sign() <= 0 {
		none := func(m Method) Value { return Value{Method: m, Reason: ReasonUnpriced} }
		return Valuation{Floor: none(MethodFloor), Implied: none(MethodImplied), Marked: none(MethodMarked), Headline: none(MethodFloor)}, nil
	}
	v := Valuation{Floor: toUSD(fq, pFloor), Implied: toUSD(iq, pFloor)}
	if p.Instance.Freeze {
		v.Implied = Value{Method: MethodImplied, Reason: ReasonPoolFrozen}
		v.Marked = Value{Method: MethodMarked, Reason: ReasonPoolFrozen}
		v.Headline = v.Floor
		return v, nil
	}
	if pr.Other == nil || pr.Other.Sign() < 0 {
		v.Marked = Value{Method: MethodMarked, Reason: ReasonNoIndependentPrice}
		v.Headline = approximately(v.Implied, ReasonNoIndependentPrice)
		return v, nil
	}
	l, _ := p.quoteLegs(quote) // validated by Floor
	num := new(big.Rat).Add(new(big.Rat).Mul(new(big.Rat).SetInt(l.q.Balance), pr.Quote), new(big.Rat).Mul(new(big.Rat).SetInt(l.x.Balance), pr.Other))
	num.Mul(num, new(big.Rat).SetFrac(shares, l.s))
	v.Marked = floorDiv(num.Num(), num.Denom(), Value{Method: MethodMarked})
	ok, err := WithinMarket(p, quote, pr.Quote, pr.Other)
	if err != nil {
		return Valuation{}, err
	}
	if ok {
		v.Headline = v.Marked
	} else {
		v.Headline = approximately(v.Implied, ReasonPoolOffMarket)
	}
	return v, nil
}

// toUSD is ⌊q·price⌋; a quote value already below precision keeps its reason.
func toUSD(q Value, price *big.Rat) Value {
	out := floorDiv(new(big.Int).Mul(q.Amount, price.Num()), price.Denom(), Value{Method: q.Method, Approximate: q.Approximate, LowerBound: q.LowerBound})
	if q.Reason != "" {
		out.Reason = q.Reason
	}
	return out
}

func approximately(v Value, reason string) Value {
	v.Approximate = true
	if v.Reason == "" {
		v.Reason = reason
	}
	return v
}

// LowerMedian is the index into xs of its lower median: of the values sorted ascending, element ⌊(n−1)/2⌋, so an even
// count takes the lower middle. -1 when xs is empty.
func LowerMedian(xs []*big.Rat) int {
	if len(xs) == 0 {
		return -1
	}
	idx := make([]int, len(xs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return xs[idx[a]].Cmp(xs[idx[b]]) < 0 })
	return idx[(len(xs)-1)/2]
}

// DailyValue is a day's value from its hour-close values: the lower median of the hours with an amount, with that
// hour's method, lower-bound flag and reason. Approximate when any hour was approximate or had no amount, and then the
// reason is the median hour's, else the first such hour's.
func DailyValue(hours []Value) Value {
	var xs []*big.Rat
	var at []int
	approx, reason := false, ""
	for i, h := range hours {
		if h.Approximate || h.Amount == nil {
			approx = true
			if reason == "" {
				reason = h.Reason
			}
		}
		if h.Amount != nil {
			xs, at = append(xs, new(big.Rat).SetInt(h.Amount)), append(at, i)
		}
	}
	m := LowerMedian(xs)
	if m < 0 {
		return Value{Approximate: approx, Reason: reason}
	}
	out := hours[at[m]]
	out.Amount = new(big.Int).Set(out.Amount)
	out.Approximate = approx
	if out.Reason == "" {
		out.Reason = reason
	}
	return out
}
