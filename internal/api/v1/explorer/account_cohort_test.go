package explorer

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

const cohortTestUSDC = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// Valuation prices classic holdings at the live rate and sums only what
// it priced; the same price values the month's flows AT TODAY'S PRICE.
func TestAccountCohortView_PricesClassicHoldingsAndFlowsAtTheLiveRate(t *testing.T) {
	h := &Handler{
		PricingEnabled: true,
		LookupUSDPrice: func(_ context.Context, a canonical.Asset) (string, bool) {
			switch a.String() {
			case "native", "crypto:XLM":
				return "0.10", true
			case cohortTestUSDC:
				return "1", true
			}
			return "", false
		},
		ContractProtocol: func(_ context.Context, id string) (string, bool) {
			if id == "CBLENDPOOL" {
				return "blend", true
			}
			return "", false
		},
	}
	at := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	jul := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	snap := clickhouse.AccountCohort{
		Root: "GROOT", Relation: "created", Covered: true,
		Cycle: clickhouse.AccountCohortCycle{ComputedAt: at, TipLedger: 1},
		Holdings: []clickhouse.AccountCohortHolding{
			{Asset: "native", Holders: 9, Balance: big.NewInt(12_500_000_000)},      // 1,250 XLM → $125.00
			{Asset: cohortTestUSDC, Holders: 3, Balance: big.NewInt(4_000_000_000)}, // 400 USDC → $400.00
			{Asset: "pool:0a1b", Holders: 1, Balance: big.NewInt(70_000_000)},
			{Asset: "CCTOKEN", Holders: 1, Balance: big.NewInt(1_000)},
		},
		Flows: []clickhouse.AccountCohortFlow{
			{Month: jul, Asset: clickhouse.CohortAllAssets, Inflow: big.NewInt(0), Outflow: big.NewInt(0), Movements: 5, ActiveAccounts: 2},
			{Month: jul, Asset: cohortTestUSDC, Inflow: big.NewInt(1_000_000_000), Outflow: big.NewInt(250_000_000), Movements: 3, ActiveAccounts: 1},
			{Month: jul, Asset: "CCTOKEN", Inflow: big.NewInt(5_000), Outflow: big.NewInt(0), Movements: 2, ActiveAccounts: 1},
		},
		Contracts: []clickhouse.AccountCohortContract{
			{ContractID: "CBLENDPOOL", Movements: 4, ActiveAccounts: 2, FirstAt: at, LastAt: at},
			{ContractID: "CUNKNOWN", Movements: 1, ActiveAccounts: 1, FirstAt: at, LastAt: at},
		},
	}
	v := h.accountCohortView(context.Background(), snap)

	if v.Valuation.PricedHoldings != 2 || v.Valuation.UnpricedHoldings != 2 {
		t.Errorf("priced/unpriced = %d/%d, want 2/2", v.Valuation.PricedHoldings, v.Valuation.UnpricedHoldings)
	}
	if v.Valuation.TotalUSD == nil || *v.Valuation.TotalUSD != "525.00" {
		t.Errorf("total_usd = %v, want 525.00", v.Valuation.TotalUSD)
	}
	for _, hd := range v.Holdings {
		switch hd.Asset {
		case cohortTestUSDC:
			if hd.ValueUSD == nil || *hd.ValueUSD != "400.00" || hd.PriceUSD == nil || *hd.PriceUSD != "1" {
				t.Errorf("USDC = %+v", hd)
			}
		case "native":
			if hd.ValueUSD == nil || *hd.ValueUSD != "125.00" {
				t.Errorf("native = %+v", hd)
			}
		default:
			if hd.ValueUSD != nil || hd.PriceUSD != nil {
				t.Errorf("%s must be unpriced: %+v", hd.Asset, hd)
			}
		}
	}
	if len(v.Flows.Points) != 1 {
		t.Fatalf("points = %+v", v.Flows.Points)
	}
	for _, af := range v.Flows.Points[0].ByAsset {
		switch af.Asset {
		case cohortTestUSDC:
			if af.InflowUSD == nil || *af.InflowUSD != "100.00" || af.OutflowUSD == nil || *af.OutflowUSD != "25.00" {
				t.Errorf("USDC flow USD = %+v", af)
			}
		case "CCTOKEN":
			if af.InflowUSD != nil || af.Scaled {
				t.Errorf("contract token flow must be raw and unpriced: %+v", af)
			}
		}
	}
	if v.Contracts[0].Protocol != "blend" || v.Contracts[1].Protocol != "" {
		t.Errorf("contract labels = %q / %q, want blend / (none)", v.Contracts[0].Protocol, v.Contracts[1].Protocol)
	}
}

func TestStroops7(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0"}, {1, "0.0000001"}, {10_000_000, "1"}, {12_345_678, "1.2345678"}, {-25_000_000, "-2.5"}, {123_456_789_012, "12345.6789012"},
	} {
		if got := stroops7(big.NewInt(tc.in)); got != tc.want {
			t.Errorf("stroops7(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := stroops7(nil); got != "0" {
		t.Errorf("stroops7(nil) = %q", got)
	}
}
