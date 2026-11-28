package blend

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"
)

// SecondsPerYear is the pool contract's SECONDS_PER_YEAR (constants.rs:10 in both V1 and V2), the
// year calc_accrual annualises cur_ir over; a realised rate uses the same year so it is comparable.
const SecondsPerYear = 31_536_000

// MinRealisedInterval is the shortest window a realised rate is computed over: hourly is the grain
// stored reserve states are read at. Resolution is checked separately, against the actual r0.
const MinRealisedInterval = time.Hour

// RateDecimals is the precision realised and net yields are rendered at, as the API's APRs are.
const RateDecimals = 4

// ErrLowResolution means one unit of r0, annualised over the window, is not below half the last
// rendered decimal: the rate cannot be told apart from rounding, so no number is shown.
var ErrLowResolution = errors.New("realised rate: insufficient resolution for this rate and window")

var (
	errRealisedStartRate  = errors.New("realised rate: start rate must be positive")
	errRealisedEndRate    = errors.New("realised rate: end rate must not be negative")
	errRealisedInterval   = fmt.Errorf("realised rate: interval must be at least %s", MinRealisedInterval)
	errYieldPartAmbiguous = errors.New("net yield: a component must carry exactly one of a value or a reason")
	errYieldNegativeEmiss = errors.New("net yield: an emissions APR cannot be negative")
)

// realisedGrowth is r1/r0 - 1 and the window in years, after validating the inputs.
func realisedGrowth(r0, r1 *big.Int, t0, t1 time.Time) (growth, years *big.Rat, err error) {
	switch {
	case r0 == nil || r0.Sign() <= 0:
		return nil, nil, errRealisedStartRate
	case r1 == nil || r1.Sign() < 0:
		return nil, nil, errRealisedEndRate
	case t1.Sub(t0) < MinRealisedInterval:
		return nil, nil, errRealisedInterval
	}
	years = new(big.Rat).SetFrac(big.NewInt(int64(t1.Sub(t0))), big.NewInt(SecondsPerYear*int64(time.Second)))
	// One rate unit annualises to 1/(r0·years); it must be strictly below halfRateUnit.
	if unit := new(big.Rat).Inv(new(big.Rat).Mul(new(big.Rat).SetInt(r0), years)); unit.Cmp(halfRateUnit) >= 0 {
		return nil, nil, ErrLowResolution
	}
	growth = new(big.Rat).SetFrac(new(big.Int).Sub(r1, r0), r0)
	return growth, years, nil
}

// RealisedAPR is (r1/r0 - 1) annualised linearly over [t0, t1], exactly. r0 and r1 are one
// reserve's b_rate (supply) or d_rate (borrow) at the same scale, and t0, t1 the states' last_time.
// A fall in b_rate (a V2 default) is a negative rate and is returned as such.
func RealisedAPR(r0, r1 *big.Int, t0, t1 time.Time) (*big.Rat, error) {
	growth, years, err := realisedGrowth(r0, r1, t0, t1)
	if err != nil {
		return nil, err
	}
	return growth.Quo(growth, years), nil
}

// RealisedAPY is (r1/r0)^(1/years) - 1 rendered at RateDecimals: DISPLAY ONLY. Its exponent is a
// float, so it is never summed, compared for a decision or fed back into another figure.
func RealisedAPY(r0, r1 *big.Int, t0, t1 time.Time) (string, error) {
	v, err := realisedAPYFloat(r0, r1, t0, t1)
	if err != nil {
		return "", err
	}
	return FormatRate(new(big.Rat).SetFloat64(v)), nil
}

func realisedAPYFloat(r0, r1 *big.Int, t0, t1 time.Time) (float64, error) {
	growth, years, err := realisedGrowth(r0, r1, t0, t1)
	if err != nil {
		return 0, err
	}
	g, _ := growth.Float64() // i128:ok display-only APY exponent input: a rate ratio, not an amount
	y, _ := years.Float64()  // i128:ok display-only APY exponent input: a duration in years
	v := math.Expm1(math.Log1p(g) / y)
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, fmt.Errorf("realised APY: growth %s over %s years overflows", growth.FloatString(12), years.FloatString(6))
	}
	return v, nil
}

// FormatRate renders a rate fraction at RateDecimals, rounded half away from zero, with a value that
// rounds to zero unsigned: the rendering of the API's ratString(r, netAPRDecimals).
func FormatRate(r *big.Rat) string {
	s := r.FloatString(RateDecimals)
	if s[0] == '-' && new(big.Rat).Abs(r).Cmp(halfRateUnit) < 0 {
		return s[1:]
	}
	return s
}

// halfRateUnit is half of the last rendered decimal, 0.5·10^-RateDecimals.
var halfRateUnit = new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Mul(big.NewInt(2), new(big.Int).Exp(big.NewInt(10), big.NewInt(RateDecimals), nil)))

// YieldSide is the side of a reserve a net yield is for.
type YieldSide int

const (
	// YieldSupply nets base + emissions: what a supplier earns.
	YieldSupply YieldSide = iota
	// YieldBorrow nets emissions - base: a borrower's cost is negative, so higher is better on both sides.
	YieldBorrow
)

// YieldComponent names a part of a net yield; an API exclusion carries it as its kind.
type YieldComponent string

const (
	ComponentBase      YieldComponent = "base"
	ComponentEmissions YieldComponent = "emissions"
)

// YieldPart is one component: exactly one of Value (a rate fraction) or Reason (why it is unknown).
type YieldPart struct {
	Value  *big.Rat
	Reason string
}

// YieldExclusion names a component left out of a net yield; it maps onto the API's
// LendingStatsExclusion as {Kind: Component, Reason: Reason}.
type YieldExclusion struct {
	Component YieldComponent
	Reason    string
}

// NetYield is a composed yield. Value is nil when no honest figure exists; LowerBound marks a Value
// that leaves out the components Excluded names. Caveats name why a supplier may not be able to
// collect what the rate says they are owed.
type NetYield struct {
	Value      *big.Rat
	LowerBound bool
	Excluded   []YieldExclusion
	Caveats    []Caveat
}

// Caveat names a reason a reserve's yield is owed on paper but may not be withdrawable.
type Caveat string

const (
	CaveatPoolStatusUnknown          Caveat = "pool_status_unknown"
	CaveatPoolNotActive              Caveat = "pool_not_active"
	CaveatUtilizationUnknown         Caveat = "utilization_unknown"
	CaveatUtilizationFull            Caveat = "utilization_full"
	CaveatBackstopLiabilitiesUnknown Caveat = "backstop_liabilities_unknown"
	CaveatBackstopHoldsLiabilities   Caveat = "backstop_holds_liabilities"
	// CaveatValueUnknown is Rankable's own reason for a net yield with no value.
	CaveatValueUnknown Caveat = "value_unknown"
)

// CollectabilityFacts is what the caller read about the reserve and its pool; a nil or unknown fact
// counts against collectability rather than for it.
type CollectabilityFacts struct {
	PoolStatusKnown bool
	PoolActive      bool
	// Utilization is the reserve's utilization in 7 decimals; at 1e7 no supplier can withdraw.
	Utilization *big.Int
	// BackstopHoldsLiabilities is whether the pool's backstop carries debt in this reserve: that debt
	// keeps accruing into b_rate though no one may ever repay it.
	BackstopHoldsLiabilities *bool
}

func (f CollectabilityFacts) caveats() []Caveat {
	var out []Caveat
	switch {
	case !f.PoolStatusKnown:
		out = append(out, CaveatPoolStatusUnknown)
	case !f.PoolActive:
		out = append(out, CaveatPoolNotActive)
	}
	switch {
	case f.Utilization == nil:
		out = append(out, CaveatUtilizationUnknown)
	case f.Utilization.Cmp(scalar7) >= 0:
		out = append(out, CaveatUtilizationFull)
	}
	switch {
	case f.BackstopHoldsLiabilities == nil:
		out = append(out, CaveatBackstopLiabilitiesUnknown)
	case *f.BackstopHoldsLiabilities:
		out = append(out, CaveatBackstopHoldsLiabilities)
	}
	return out
}

// Rankable is the gate for any "earns most" ordering: a net yield ranks only with a value and no
// caveat, else false with every reason. A lower bound ranks, since it never overstates.
func Rankable(n NetYield) (bool, []Caveat) {
	reasons := append([]Caveat(nil), n.Caveats...)
	if n.Value == nil {
		reasons = append(reasons, CaveatValueUnknown)
	}
	return len(reasons) == 0, reasons
}

func (p YieldPart) check() error {
	if (p.Value == nil) == (p.Reason == "") {
		return errYieldPartAmbiguous
	}
	return nil
}

// ComposeNetYield nets base and emissions for side. Emissions only add, so without them the result is
// a lower bound; base can be negative (a realised default) or a cost, so without it there is no
// bound and Value is nil. A missing part is never read as zero. facts set the result's Caveats.
func ComposeNetYield(side YieldSide, base, emissions YieldPart, facts CollectabilityFacts) (NetYield, error) {
	if err := base.check(); err != nil {
		return NetYield{}, err
	}
	if err := emissions.check(); err != nil {
		return NetYield{}, err
	}
	if emissions.Value != nil && emissions.Value.Sign() < 0 {
		return NetYield{}, errYieldNegativeEmiss
	}
	out := NetYield{Caveats: facts.caveats()}
	if base.Reason != "" {
		out.Excluded = append(out.Excluded, YieldExclusion{ComponentBase, base.Reason})
	}
	if emissions.Reason != "" {
		out.Excluded = append(out.Excluded, YieldExclusion{ComponentEmissions, emissions.Reason})
	}
	if base.Value == nil {
		return out, nil
	}
	v := new(big.Rat).Set(base.Value)
	if side == YieldBorrow {
		v.Neg(v)
	}
	if emissions.Value != nil {
		v.Add(v, emissions.Value)
	}
	out.Value, out.LowerBound = v, emissions.Value == nil
	return out, nil
}
