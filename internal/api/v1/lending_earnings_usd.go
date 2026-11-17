package v1

import (
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Why a leg's value_usd or accrued_usd is null, and why a net_usd or net_apr is absent.
const (
	earningsUSDWithheld = "withheld" // the amount is null (withheld or unread)
	earningsUSDUnpriced = "unpriced" // its reserve has no USD price on /reserves; a backstop token is never a reserve

	netInputsPartial     = "inputs_partial"
	netAPRUnavailable    = "apr_unavailable"
	netAPRNetNotPositive = "net_value_not_positive"
)

// netAPRDecimals matches the 4 dp of the reserve APRs net_apr weights; more digits would be noise.
const netAPRDecimals = 4

// LendingUSDTotals values supply, borrow and backstop legs in USD; decimal strings, exact sums rounded half away from zero to
// 2 dp.
type LendingUSDTotals struct {
	SuppliedUSD string `json:"supplied_usd"`
	BorrowedUSD string `json:"borrowed_usd"`
	// NetUSD is supplied_usd - borrowed_usd, null with NetUSDReason when any leg is left out: an omitted borrow would make
	// it too high, so it cannot be a lower bound.
	NetUSD       *string `json:"net_usd"`
	NetUSDReason *string `json:"net_usd_reason"`
	// LowerBound marks supplied_usd and borrowed_usd as floors; Excluded names each leg left out of them.
	LowerBound bool                    `json:"lower_bound"`
	Excluded   []LendingStatsExclusion `json:"excluded"`
	// NetAPR is (supplied value x supply APR - borrowed value x borrow APR) / net_usd at the reserves' current APRs, emissions
	// excluded; null with NetAPRReason when net_usd is null, an APR is unread, or net_usd is not positive.
	NetAPR       *string `json:"net_apr"`
	NetAPRReason *string `json:"net_apr_reason"`
}

// LendingEarningsPool is one pool's USD totals.
type LendingEarningsPool struct {
	Protocol string `json:"protocol"`
	Pool     string `json:"pool"`
	LendingUSDTotals
}

// usdTally accumulates exact USD sums and APR-weighted values over priced legs.
type usdTally struct {
	supplied, borrowed, supplyYield, borrowYield big.Rat
	excluded                                     []LendingStatsExclusion
	aprMissing                                   bool
}

func (t *usdTally) add(side string, value *big.Rat, apr *float64) {
	sum, yield := &t.supplied, &t.supplyYield
	if side == earningsSideBorrow {
		sum, yield = &t.borrowed, &t.borrowYield
	}
	sum.Add(sum, value)
	if value.Sign() == 0 {
		return
	}
	r, ok := aprRat(apr)
	if !ok {
		t.aprMissing = true
		return
	}
	yield.Add(yield, r.Mul(r, value))
}

func (t *usdTally) totals() LendingUSDTotals {
	out := LendingUSDTotals{
		SuppliedUSD: ratString(&t.supplied, 2), BorrowedUSD: ratString(&t.borrowed, 2),
		LowerBound: len(t.excluded) > 0, Excluded: append([]LendingStatsExclusion{}, t.excluded...),
	}
	if out.LowerBound {
		out.NetUSDReason, out.NetAPRReason = strRef(netInputsPartial), strRef(netInputsPartial)
		return out
	}
	net := new(big.Rat).Sub(&t.supplied, &t.borrowed)
	out.NetUSD = strRef(ratString(net, 2))
	switch {
	case t.aprMissing:
		out.NetAPRReason = strRef(netAPRUnavailable)
	case net.Sign() <= 0:
		out.NetAPRReason = strRef(netAPRNetNotPositive)
	default:
		apr := new(big.Rat).Sub(&t.supplyYield, &t.borrowYield)
		out.NetAPR = strRef(ratString(apr.Quo(apr, net), netAPRDecimals))
	}
	return out
}

// ratString is r rounded half away from zero to prec digits, with a value that rounds to zero rendered unsigned.
func ratString(r *big.Rat, prec int) string {
	s := r.FloatString(prec)
	if strings.HasPrefix(s, "-") && strings.Trim(s, "-0.") == "" {
		return s[1:]
	}
	return s
}

// aprRat is a served APR as the exact decimal it is rendered as, not its binary float expansion.
func aprRat(apr *float64) (*big.Rat, bool) {
	if apr == nil || math.IsNaN(*apr) || math.IsInf(*apr, 0) {
		return nil, false
	}
	return new(big.Rat).SetString(strconv.FormatFloat(*apr, 'f', -1, 64))
}

// valueEarningsLegs prices each leg at its pool's /reserves view and totals them per pool and overall; a leg it cannot value
// is named in its pool's and the overall exclusions.
func valueEarningsLegs(legs []LendingEarningsLeg, reserves map[string]map[string]ReserveView) ([]LendingEarningsPool, LendingUSDTotals) {
	var all usdTally
	var order []*LendingEarningsPool
	tallies := map[string]*usdTally{}
	for i := range legs {
		leg := &legs[i]
		t := tallies[leg.Pool]
		if t == nil {
			t = &usdTally{}
			tallies[leg.Pool] = t
			order = append(order, &LendingEarningsPool{Protocol: leg.Protocol, Pool: leg.Pool})
		}
		asset := ""
		if leg.Asset != nil {
			asset = *leg.Asset
		}
		rv := reserves[leg.Pool][asset]
		value, reason := valueLeg(leg, rv)
		if reason != "" {
			pool := leg.Pool
			ex := LendingStatsExclusion{Kind: leg.Side, Pool: &pool, Asset: leg.Asset, Reason: reason}
			t.excluded, all.excluded = append(t.excluded, ex), append(all.excluded, ex)
			continue
		}
		apr := rv.SupplyAPR
		if leg.Side == earningsSideBorrow {
			apr = rv.BorrowAPR
		}
		t.add(leg.Side, value, apr)
		all.add(leg.Side, value, apr)
	}
	pools := make([]LendingEarningsPool, 0, len(order))
	for _, p := range order {
		p.LendingUSDTotals = tallies[p.Pool].totals()
		pools = append(pools, *p)
	}
	return pools, all.totals()
}

// valueLeg sets a leg's value_usd, accrued_usd and usd_reason and returns its USD value, or the reason it has none.
func valueLeg(leg *LendingEarningsLeg, rv ReserveView) (*big.Rat, string) {
	if leg.Side == earningsSideBackstop {
		leg.UsdReason = strRef(earningsUSDUnpriced)
		return nil, earningsUSDUnpriced
	}
	if leg.Decimals == nil && rv.Decimals != nil {
		d := int(*rv.Decimals)
		leg.Decimals = &d
	}
	value, reason := legUSD(leg.CurrentValue, rv)
	if reason != "" {
		leg.UsdReason = strRef(reason)
		return nil, reason
	}
	leg.ValueUSD = strRef(ratString(value, 2))
	if leg.Accrued == nil {
		return value, ""
	}
	// A zero current_value is worth zero unpriced, so a non-zero accrued can still fail to price here.
	if accrued, r := legUSD(leg.Accrued, rv); r != "" {
		leg.UsdReason = strRef(r)
	} else {
		leg.AccruedUSD = strRef(ratString(accrued, 2))
	}
	return value, ""
}

// legUSD values a base-unit amount at the reserve view's own price and decimals, the pair its supplied_usd uses; a zero
// amount is worth zero whatever its price.
func legUSD(amount *string, rv ReserveView) (*big.Rat, string) {
	if amount == nil {
		return nil, earningsUSDWithheld
	}
	n, ok := parseBig(*amount)
	if !ok {
		return nil, earningsUSDWithheld
	}
	if n.Sign() == 0 {
		return new(big.Rat), ""
	}
	if rv.PriceUSD == nil || rv.Decimals == nil {
		return nil, earningsUSDUnpriced
	}
	v, err := usdValueRat(n, *rv.PriceUSD, int(*rv.Decimals))
	if err != nil {
		return nil, earningsUSDUnpriced
	}
	return v, ""
}
