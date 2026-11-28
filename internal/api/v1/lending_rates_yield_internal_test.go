package v1

import (
	"context"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

const yieldAsset = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"

var (
	yieldT0  = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	yieldReq = rateYieldRequest{window: 7 * 24 * time.Hour}
)

func yieldInt(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad int %q", s)
	}
	return n
}

func yieldState(t *testing.T, ledger uint32, bRate, dRate string, at time.Time) *clickhouse.LendingRateState {
	t.Helper()
	return &clickhouse.LendingRateState{Ledger: ledger, CloseTime: at, BRate: yieldInt(t, bRate), DRate: yieldInt(t, dRate), LastTime: at}
}

// yieldInputs is one V2 reserve accrued over six days of a seven-day window opening at yieldT0: b_rate 1.000 -> 1.001,
// d_rate 1.000 -> 1.002 at 1e12, a supply stream of eps 1e12 (0.01 BLND a second at 7 decimals) set before the window at
// $0.05, a complete stream list, and a backstop with no debt.
func yieldInputs(t *testing.T) rateYieldInputs {
	t.Helper()
	seven := 7
	end := yieldState(t, 200, "1001000000000", "1002000000000", yieldT0.Add(6*24*time.Hour))
	return rateYieldInputs{
		from: yieldT0, window: 7 * 24 * time.Hour,
		states: map[string]clickhouse.LendingRateStates{yieldAsset: {
			Base: yieldState(t, 100, "1000000000000", "1000000000000", yieldT0), First: end, End: end,
		}},
		resList: true,
		streams: map[string]LendingReserveEmissionView{yieldAsset + "\x00supply": {
			Asset: strRef(yieldAsset), Side: clickhouse.EmissionSideSupply,
			LendingEmissionStreamView: LendingEmissionStreamView{EPS: "1000000000000", Live: true, LastUpdate: WireTime(yieldT0.Add(-time.Hour))},
		}},
		price:        protocolTokenPrice(&LendingProtocolToken{PriceUSD: strRef("0.05"), Decimals: 7}, &seven),
		emisDecimals: &seven,
		backstopDebt: map[string]*big.Int{},
	}
}

// yieldReserve is 100,000 tokens supplied and 50,000 borrowed at $1, 7 decimals: a $100,000 supply side.
func yieldReserve() (LendingRate, ReserveView, *blend.ReserveData) {
	dec := uint32(7)
	rv := ReserveView{Asset: yieldAsset, Decimals: &dec, Supplied: "1000000000000", Borrowed: "500000000000", PriceUSD: strRef("1")}
	data := &blend.ReserveData{
		BRate: big.NewInt(1e12), DRate: big.NewInt(1e12), BSupply: big.NewInt(1e12), DSupply: big.NewInt(5e11), Version: blend.PoolV2,
	}
	return LendingRate{PoolStatus: strRef("active")}, rv, data
}

func ptrIs(p *string, want string) bool { return (p == nil && want == "") || (p != nil && *p == want) }

// TestRateYield_GoldenReserve pins every yield field of one reserve, derived by hand:
//
//	years            = 6 d / 365 d = 6/365 (the states' own last_time, not the 7-day window)
//	supply realised  = (1.001/1.000 - 1) / (6/365) = 0.001 x 60.8333 = 0.060833 -> 0.0608
//	borrow realised  = (1.002/1.000 - 1) / (6/365) = 0.002 x 60.8333 = 0.121667 -> 0.1217
//	supply APY       = 1.001^(365/6) - 1 = e^(60.8333 x 0.0009995) - 1 = 0.06269 -> 0.0627 (display only)
//	emissions tokens = 1e12 x 31,536,000 / 10^(7+7) = 315,360 BLND a year; x $0.05 = $15,768
//	emissions APR    = 15,768 / 100,000 = 0.15768 -> 0.1577; per $1000 a year 315,360 x 1000 / 100,000 = 3153.6
//	net supply       = 0.060833 + 0.15768 = 0.218513 -> 0.2185
//	net borrow       = 0 - 0.121667 -> -0.1217: the stream list was read whole and holds no borrow stream
func TestRateYield_GoldenReserve(t *testing.T) {
	row, rv, data := yieldReserve()
	y := rateYield(yieldInputs(t), yieldReq, row, rv, data)
	if y.RealisedFrom == nil || !time.Time(*y.RealisedFrom).Equal(yieldT0) || y.RealisedSpanSeconds == nil || *y.RealisedSpanSeconds != 6*86400 {
		t.Fatalf("realised window = %v span %v, want the states' last_time", y.RealisedFrom, y.RealisedSpanSeconds)
	}
	s, b := y.Supply, y.Borrow
	if !ptrIs(s.RealisedAPR, "0.0608") || !ptrIs(s.RealisedAPY, "0.0627") || !ptrIs(s.EmissionsAPR, "0.1577") ||
		!ptrIs(s.EmissionsTokensPer1000USDYear, "3153.6000000") || !ptrIs(s.NetYield, "0.2185") || s.NetLowerBound ||
		s.NetBasis != netBasisRealisedCurrent || !slices.Equal(s.NetCaveats, []string{caveatEmissionsCurrent}) ||
		len(s.NetExcluded) != 0 || !s.Rankable {
		t.Errorf("supply = %+v", s)
	}
	if !ptrIs(b.RealisedAPR, "0.1217") || !ptrIs(b.EmissionsAPR, "0.0000") || !ptrIs(b.EmissionsTokensPer1000USDYear, "0") ||
		!ptrIs(b.NetYield, "-0.1217") || b.NetLowerBound || len(b.NetCaveats) != 0 || !b.Rankable {
		t.Errorf("borrow = %+v", b)
	}
}

// TestRateYield_UnknownStreamIsNeverZero: with the reserve list unread, or this side's stream entry undecodable, the
// emissions are unknown, so the net is a lower bound naming them and does not rank.
func TestRateYield_UnknownStreamIsNeverZero(t *testing.T) {
	row, rv, data := yieldReserve()
	cases := map[string]func(*rateYieldInputs){
		emissionResListUnread: func(in *rateYieldInputs) { in.resList, in.streams = false, nil },
		emissionStreamBad:     func(in *rateYieldInputs) { in.undecodable = map[string]bool{yieldAsset + "\x00borrow": true} },
	}
	for reason, mutate := range cases {
		in := yieldInputs(t)
		mutate(&in)
		b := rateYield(in, yieldReq, row, rv, data).Borrow
		if b.EmissionsAPR != nil || !ptrIs(b.EmissionsAPRReason, reason) || !ptrIs(b.NetYield, "-0.1217") || !b.NetLowerBound ||
			!slices.Equal(b.NetExcluded, []LendingYieldExclusion{{"emissions", reason}}) || b.Rankable ||
			!slices.Contains(b.RankableReasons, rankStreamUnknown) {
			t.Errorf("%s: borrow = %+v", reason, b)
		}
	}
}

// TestRateYield_BaseOlderThanWindowIsNotAveraged: a reserve accrued 2% over the 59 days to its last state before a 7-day
// window (b_rate 1.00 -> 1.02), then 50% from 6 days ago to 1 day ago (1.02 -> 1.53). That old state is outside the 7-hour
// base slack, so the rate runs over those 5 days alone: (1.53/1.02 - 1) / (5/365) = 0.5 x 73 = 36.5. Averaging from the
// old state would give (1.53 - 1) / (64/365) = 3.02.
func TestRateYield_BaseOlderThanWindowIsNotAveraged(t *testing.T) {
	row, rv, data := yieldReserve()
	in := yieldInputs(t)
	first := yieldState(t, 150, "1020000000000", "1000000000000", yieldT0.Add(24*time.Hour))
	end := yieldState(t, 200, "1530000000000", "1000000000000", yieldT0.Add(6*24*time.Hour))
	old := yieldState(t, 50, "1000000000000", "1000000000000", yieldT0.Add(-58*24*time.Hour))
	for name, st := range map[string]clickhouse.LendingRateStates{
		"the reader saw a prior state":                   {Prior: true, First: first, End: end},
		"a base row whose own last_time is before slack": {Base: old, First: first, End: end},
	} {
		in.states[yieldAsset] = st
		y := rateYield(in, yieldReq, row, rv, data)
		if !ptrIs(y.Supply.RealisedAPR, "36.5000") || y.RealisedFrom == nil || !time.Time(*y.RealisedFrom).Equal(first.LastTime) ||
			*y.RealisedSpanSeconds != 5*86400 || !y.Supply.Rankable {
			t.Errorf("%s: span %v supply %+v", name, y.RealisedSpanSeconds, y.Supply)
		}
	}
	// A base that accrued 6 hours before the window is inside a 7-day window's slack, so it is the base.
	in.states[yieldAsset] = clickhouse.LendingRateStates{Base: yieldState(t, 100, "1020000000000", "1000000000000", yieldT0.Add(-6*time.Hour)), First: first, End: end}
	if y := rateYield(in, yieldReq, row, rv, data); *y.RealisedSpanSeconds != (6*24+6)*3600 {
		t.Errorf("base inside the slack: span %v", *y.RealisedSpanSeconds)
	}
	// One state inside the window after an old one: the base is before the window, named apart from none at all.
	in.states[yieldAsset] = clickhouse.LendingRateStates{Prior: true, First: end, End: end}
	if s := rateYield(in, yieldReq, row, rv, data).Supply; s.RealisedAPR != nil || !ptrIs(s.RealisedAPRReason, yieldReasonOldBase) {
		t.Errorf("old base, single state: supply = %+v", s)
	}
	// A first state 5 days in leaves a 2-day span: under half the window, so it does not rank.
	in.states[yieldAsset] = clickhouse.LendingRateStates{First: yieldState(t, 150, "1020000000000", "1000000000000", yieldT0.Add(5*24*time.Hour)), End: end}
	if s := rateYield(in, yieldReq, row, rv, data).Supply; s.RealisedAPR == nil || s.Rankable || !slices.Contains(s.RankableReasons, rankShortSpan) {
		t.Errorf("short span supply = %+v", s)
	}
	// One state inside the window and none near its start: no base.
	in.states[yieldAsset] = clickhouse.LendingRateStates{First: end, End: end}
	if s := rateYield(in, yieldReq, row, rv, data).Supply; s.RealisedAPR != nil || !ptrIs(s.RealisedAPRReason, yieldReasonNoBase) || s.NetYield != nil {
		t.Errorf("single state supply = %+v", s)
	}
}

// TestRateYield_RankGates: a disabled reserve and a history behind the request each keep a valued yield out of a ranking,
// by name.
func TestRateYield_RankGates(t *testing.T) {
	row, rv, data := yieldReserve()
	in := yieldInputs(t)
	disabled := row
	disabled.Exclusions = []string{rateExclReserveDisabled}
	behind := yieldReq
	behind.behind = true
	for _, tc := range []struct {
		name string
		s    LendingSideYield
		want string
	}{
		{"disabled", rateYield(in, yieldReq, disabled, rv, data).Supply, rateExclReserveDisabled},
		{"behind", rateYield(in, behind, row, rv, data).Supply, rankHistoryBehind},
	} {
		if !ptrIs(tc.s.NetYield, "0.2185") || tc.s.Rankable || !slices.Equal(tc.s.RankableReasons, []string{tc.want}) {
			t.Errorf("%s: supply = %+v", tc.name, tc.s)
		}
	}
}

// TestRateYield_LowResolutionAndMissingBaseAreNullNeverZero: a b_rate defaulted near zero cannot be told from rounding,
// and a reserve first written inside the window has no base; both leave the realised rate and the net null with a reason.
func TestRateYield_LowResolutionAndMissingBaseAreNullNeverZero(t *testing.T) {
	row, rv, data := yieldReserve()
	in := yieldInputs(t)
	in.states[yieldAsset] = clickhouse.LendingRateStates{
		Base: yieldState(t, 100, "1000", "1000000000000", yieldT0),
		End:  yieldState(t, 200, "1001", "1000010000000", yieldT0.Add(time.Hour)),
	}
	y := rateYield(in, yieldReq, row, rv, data)
	if s := y.Supply; s.RealisedAPR != nil || !ptrIs(s.RealisedAPRReason, yieldReasonLowRes) || s.NetYield != nil ||
		!slices.Equal(s.NetExcluded, []LendingYieldExclusion{{"base", yieldReasonLowRes}}) || s.Rankable ||
		!slices.Contains(s.RankableReasons, "value_unknown") {
		t.Errorf("low resolution supply = %+v", s)
	}
	if !ptrIs(y.Borrow.RealisedAPR, "0.0876") { // d_rate is unaffected: 1e-5 x 8760 hours
		t.Errorf("borrow = %+v", y.Borrow)
	}

	end := yieldState(t, 200, "1001000000000", "1002000000000", yieldT0)
	in.states[yieldAsset] = clickhouse.LendingRateStates{First: end, End: end}
	y = rateYield(in, yieldReq, row, rv, data)
	for _, s := range []LendingSideYield{y.Supply, y.Borrow} {
		if s.RealisedAPR != nil || !ptrIs(s.RealisedAPRReason, yieldReasonNoBase) || s.NetYield != nil || s.RealisedAPY != nil || s.Rankable {
			t.Errorf("missing base = %+v, want null with %s", s, yieldReasonNoBase)
		}
	}
	if y.RealisedFrom != nil || y.RealisedSpanSeconds != nil {
		t.Errorf("missing base window = %v span %v", y.RealisedFrom, y.RealisedSpanSeconds)
	}
}

// TestRateYield_ThinEmissionPriceStaysOutOfNet: a thin-market emission token is display-only, so the net is the realised
// rate alone, a lower bound naming the excluded emissions, and the price-free token rate is still served.
func TestRateYield_ThinEmissionPriceStaysOutOfNet(t *testing.T) {
	row, rv, data := yieldReserve()
	in := yieldInputs(t)
	seven := 7
	in.price = protocolTokenPrice(&LendingProtocolToken{PriceUSD: strRef("0.05"), Decimals: 7, ThinMarket: true}, &seven)
	s := rateYield(in, yieldReq, row, rv, data).Supply
	if s.EmissionsAPR != nil || !ptrIs(s.EmissionsAPRReason, emissionPriceThin) || !ptrIs(s.EmissionsTokensPer1000USDYear, "3153.6000000") ||
		!ptrIs(s.NetYield, "0.0608") || !s.NetLowerBound ||
		!slices.Equal(s.NetExcluded, []LendingYieldExclusion{{"emissions", emissionPriceThin}}) || !s.Rankable {
		t.Errorf("thin supply = %+v", s)
	}
}

// TestRateYield_FrozenFullyUtilisedReserveIsNotRankable: a yield owed on paper by a frozen pool whose reserve is fully lent
// and whose backstop holds its debt names every caveat and never ranks.
func TestRateYield_FrozenFullyUtilisedReserveIsNotRankable(t *testing.T) {
	_, rv, data := yieldReserve()
	data.DSupply = new(big.Int).Set(data.BSupply)
	in := yieldInputs(t)
	in.backstopDebt = map[string]*big.Int{yieldAsset: big.NewInt(5)}
	s := rateYield(in, yieldReq, LendingRate{PoolStatus: strRef("frozen")}, rv, data).Supply
	want := []string{"pool_not_active", "utilization_full", "backstop_holds_liabilities"}
	if !ptrIs(s.NetYield, "0.2185") || !slices.Equal(s.NetCaveats, append(want, caveatEmissionsCurrent)) || s.Rankable ||
		!slices.Equal(s.RankableReasons, want) {
		t.Errorf("frozen supply = %+v", s)
	}
	in.backstopDebt = nil
	unknown := []string{"pool_status_unknown", "utilization_unknown", "backstop_liabilities_unknown", caveatEmissionsCurrent}
	if s := rateYield(in, yieldReq, LendingRate{}, rv, nil).Supply; s.Rankable || !slices.Equal(s.NetCaveats, unknown) {
		t.Errorf("unknown facts must count against ranking: %+v", s)
	}
}

// TestRateYield_BlendV1RealisesWithoutEmissions: a V1 reserve's rates are 1e9-scaled and the ratio is scale-free, so its
// realised rate is served; its emission storage is unverified, so the net is a lower bound without them.
func TestRateYield_BlendV1RealisesWithoutEmissions(t *testing.T) {
	in := rateYieldInputs{from: yieldT0}
	(&Server{}).readYieldEmissions(context.Background(), ratePoolRead{ratePool: ratePool{protocol: "blend"}, res: poolReserves{version: blend.PoolV1}}, yieldT0, &in)
	end := yieldState(t, 200, "1001000000", "1002000000", yieldT0.Add(6*24*time.Hour))
	in.states = map[string]clickhouse.LendingRateStates{yieldAsset: {Base: yieldState(t, 100, "1000000000", "1000000000", yieldT0), First: end, End: end}}
	_, rv, _ := yieldReserve()
	s := rateYield(in, yieldReq, LendingRate{}, rv, nil).Supply
	if !ptrIs(s.RealisedAPR, "0.0608") || !ptrIs(s.EmissionsAPRReason, emissionsReasonV1) || s.EmissionsTokensPer1000USDYear != nil ||
		!ptrIs(s.NetYield, "0.0608") || !s.NetLowerBound || s.Rankable {
		t.Errorf("v1 supply = %+v", s)
	}
}

// TestRateYieldInputs_FlagsCarryAStaleEmissionPrice: a stale price that valued a stream makes the response stale, as on
// /emissions; a price that valued nothing does not.
func TestRateYieldInputs_FlagsCarryAStaleEmissionPrice(t *testing.T) {
	in := yieldInputs(t)
	in.price.stale = true
	if f := in.flags(); !f.Stale || f.Degraded {
		t.Errorf("stale price: flags %+v", f)
	}
	in.price = kpiTokenPrice{reason: kpiUSDUnpriced, stale: true}
	if f := in.flags(); f.Stale {
		t.Errorf("an unused price must not stale the response: %+v", f)
	}
}

// TestRateYield_StatePairReasons names why no realised rate exists for each shape of the stored states.
func TestRateYield_StatePairReasons(t *testing.T) {
	st := func(ledger uint32, at time.Duration) *clickhouse.LendingRateState {
		return yieldState(t, ledger, "1000000000000", "1000000000000", yieldT0.Add(at))
	}
	states := func(s clickhouse.LendingRateStates) rateYieldInputs {
		return rateYieldInputs{from: yieldT0, states: map[string]clickhouse.LendingRateStates{yieldAsset: s}}
	}
	cases := []struct {
		name   string
		in     rateYieldInputs
		reason string
	}{
		{"no reader", rateYieldInputs{statesReason: yieldReasonNoReader}, yieldReasonNoReader},
		{"no row", rateYieldInputs{from: yieldT0, states: map[string]clickhouse.LendingRateStates{}}, yieldReasonNoAccrual},
		{"undecodable", states(clickhouse.LendingRateStates{End: st(2, 0), Undecodable: true}), yieldReasonUndecodable},
		{"removed", states(clickhouse.LendingRateStates{Base: st(1, 0), End: &clickhouse.LendingRateState{Ledger: 2, Removed: true}}), yieldReasonRemoved},
		{"one state spans the window", states(clickhouse.LendingRateStates{Base: st(1, 0), End: st(1, 0)}), yieldReasonNoAccrual},
		{"under an hour", states(clickhouse.LendingRateStates{Base: st(1, 0), End: st(2, 59*time.Minute)}), yieldReasonShortWindow},
		{"b_rate zero", states(clickhouse.LendingRateStates{Base: yieldState(t, 1, "0", "0", yieldT0), End: st(2, 2*time.Hour)}), yieldReasonBaseRateZero},
	}
	_, rv, _ := yieldReserve()
	for _, tc := range cases {
		if s := rateYield(tc.in, yieldReq, LendingRate{}, rv, nil).Supply; s.RealisedAPR != nil || !ptrIs(s.RealisedAPRReason, tc.reason) || s.NetYield != nil {
			t.Errorf("%s: supply = %+v, want null with %s", tc.name, s, tc.reason)
		}
	}
}
