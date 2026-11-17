package v1

import (
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Why a leg's value_usd is null, and why a net_apr is absent.
const (
	earningsUSDWithheld = "withheld" // the leg's current value is withheld (reason) or unread
	earningsUSDUnpriced = "unpriced" // its reserve has no USD price on /reserves; a backstop token is never a reserve

	netAPRInputsPartial  = "inputs_partial"
	netAPRUnavailable    = "apr_unavailable"
	netAPRNetNotPositive = "net_value_not_positive"
)

// netAPRDecimals is the precision net_apr is rendered at; the reserve APRs it weights are served at 4.
const netAPRDecimals = 6

// LendingUSDTotals values supply and borrow legs in USD; decimal strings, exact sums rendered at 2 dp.
type LendingUSDTotals struct {
	SuppliedUSD string `json:"supplied_usd"`
	BorrowedUSD string `json:"borrowed_usd"`
	NetUSD      string `json:"net_usd"` // supplied_usd - borrowed_usd
	// LowerBound is set when a leg is left out of the sums; Unpriced names each as "pool:side:asset".
	LowerBound bool     `json:"lower_bound"`
	Unpriced   []string `json:"unpriced"`
	// NetAPR is (supplied value x supply APR - borrowed value x borrow APR) / net_usd at the reserves' current APRs, emissions
	// excluded; null with NetAPRReason when any leg is unpriced or withheld, an APR is unread, or net_usd is not positive.
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
	unpriced                                     []string
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
	net := new(big.Rat).Sub(&t.supplied, &t.borrowed)
	out := LendingUSDTotals{
		SuppliedUSD: t.supplied.FloatString(2), BorrowedUSD: t.borrowed.FloatString(2), NetUSD: net.FloatString(2),
		LowerBound: len(t.unpriced) > 0, Unpriced: append([]string{}, t.unpriced...),
	}
	switch {
	case out.LowerBound:
		out.NetAPRReason = strRef(netAPRInputsPartial)
	case t.aprMissing:
		out.NetAPRReason = strRef(netAPRUnavailable)
	case net.Sign() <= 0:
		out.NetAPRReason = strRef(netAPRNetNotPositive)
	default:
		apr := new(big.Rat).Sub(&t.supplyYield, &t.borrowYield)
		out.NetAPR = strRef(apr.Quo(apr, net).FloatString(netAPRDecimals))
	}
	return out
}

// aprRat is a served APR as the exact decimal it is rendered as, not its binary float expansion.
func aprRat(apr *float64) (*big.Rat, bool) {
	if apr == nil || math.IsNaN(*apr) || math.IsInf(*apr, 0) {
		return nil, false
	}
	return new(big.Rat).SetString(strconv.FormatFloat(*apr, 'f', -1, 64))
}

// valueEarningsLegs prices each supply and borrow leg at its pool's /reserves view and totals them per pool and overall. A leg
// whose current value is withheld is not valued, except under a history reason, which concerns net_flow only.
func valueEarningsLegs(legs []LendingEarningsLeg, reserves map[string]map[string]ReserveView) ([]LendingEarningsPool, LendingUSDTotals) {
	var all usdTally
	var order []*LendingEarningsPool
	tallies := map[string]*usdTally{}
	for i := range legs {
		leg := &legs[i]
		if leg.Side == earningsSideBackstop {
			leg.UsdReason = strRef(earningsUSDUnpriced)
			continue
		}
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
		value, reason := legUSD(leg.CurrentValue, rv, leg.Reason)
		if reason != "" {
			leg.UsdReason = strRef(reason)
			name := leg.Pool + ":" + leg.Side + ":" + asset
			t.unpriced, all.unpriced = append(t.unpriced, name), append(all.unpriced, name)
			continue
		}
		leg.ValueUSD = strRef(value.FloatString(2))
		if accrued, r := legUSD(leg.Accrued, rv, nil); leg.Accrued != nil && r == "" {
			leg.AccruedUSD = strRef(accrued.FloatString(2))
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

// legUSD values a base-unit amount at the reserve view's own price and decimals, the pair its supplied_usd uses; a zero
// amount is worth zero whatever its price.
func legUSD(amount *string, rv ReserveView, withheld *string) (*big.Rat, string) {
	if amount == nil || (withheld != nil && !strings.HasPrefix(*withheld, "history_")) {
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
