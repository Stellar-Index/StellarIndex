package v1

import (
	"slices"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestYieldsSummary_RanksExactNetsAndHoldsOutLowerBounds builds four pools' rows through rateYield, the /rates path:
//
//	PA, PB  the golden reserve: supply net 0.218513.. (0.2185), borrow net -0.121667.. (-0.1217)
//	PC      b_rate ends one unit higher: supply net 0.2185 rendered, but exactly 6.08e-11 above PA's, so it ranks first
//	PD      the emission token's market is thin: the supply net is the realised rate alone, a lower bound
//
// Supply ranks PC, then the tie PA, PB by pool id; PD is listed unrankable (net_lower_bound), never ranked below them.
// Borrow ranks all four negative nets, ties by pool id.
func TestYieldsSummary_RanksExactNetsAndHoldsOutLowerBounds(t *testing.T) {
	row, rv, data := yieldReserve()
	pc := yieldInputs(t)
	end := yieldState(t, 200, "1001000000001", "1002000000000", yieldT0.Add(6*24*time.Hour))
	pc.states = map[string]clickhouse.LendingRateStates{yieldAsset: {Base: pc.states[yieldAsset].Base, First: end, End: end}}
	pd := yieldInputs(t)
	seven := 7
	pd.price = protocolTokenPrice(&LendingProtocolToken{PriceUSD: strRef("0.05"), Decimals: 7, ThinMarket: true}, &seven)
	ins := map[string]rateYieldInputs{"PA": yieldInputs(t), "PB": yieldInputs(t), "PC": pc, "PD": pd}
	var rates LendingRatesView
	for _, pool := range []string{"PD", "PB", "PC", "PA"} {
		r := row
		r.Pool, r.Asset = pool, yieldAsset
		r.Yield = rateYield(ins[pool], yieldReq, r, rv, data)
		rates.Rates = append(rates.Rates, r)
	}
	v := lendingYieldsSummary(rates, "7d")
	lb := string(blend.CaveatNetLowerBound)
	order := func(rows []LendingYieldsSummaryRow) (pools, nets []string) {
		for _, r := range rows {
			pools, nets = append(pools, r.Pool), append(nets, *r.NetYield)
		}
		return pools, nets
	}
	if pools, nets := order(v.Supply.Ranked); !slices.Equal(pools, []string{"PC", "PA", "PB"}) || !slices.Equal(nets, []string{"0.2185", "0.2185", "0.2185"}) {
		t.Errorf("supply ranked %v %v, want PC (exactly higher) then the tie PA, PB", pools, nets)
	}
	if len(v.Supply.Unrankable) != 1 || v.Supply.Unrankable[0].Pool != "PD" || v.Supply.Unrankable[0].Rank != nil ||
		!slices.Equal(v.Supply.Unrankable[0].RankableReasons, []string{lb}) || v.Supply.Counts.ByReason[lb] != 1 {
		t.Errorf("supply unrankable %+v counts %+v, want PD held out as a lower bound", v.Supply.Unrankable, v.Supply.Counts)
	}
	if pools, nets := order(v.Borrow.Ranked); !slices.Equal(pools, []string{"PA", "PB", "PC", "PD"}) || nets[0] != "-0.1217" ||
		*v.Borrow.Ranked[3].Rank != 4 {
		t.Errorf("borrow ranked %v %v, want every negative net, ties by pool id", pools, nets)
	}
}
