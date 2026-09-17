package explorer

import (
	"context"
	"math/big"
	"strconv"
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

type namingReader struct{ capReader }

func (*namingReader) SACClassicAssetName(_ context.Context, id string) (string, bool, error) {
	if id == "CSACUSDC" {
		return "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", true, nil
	}
	return "", false, nil
}

// A contract no protocol claims is labelled by the lake when it is a
// token contract, and left unlabelled when it is not.
func TestAccountCohortView_LabelsTokenContractsTheRosterDoesNotClaim(t *testing.T) {
	h := &Handler{
		Reader:           &namingReader{capReader{probe: &deadlineProbe{}}},
		ContractProtocol: func(context.Context, string) (string, bool) { return "", false },
	}
	at := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	snap := clickhouse.AccountCohort{
		Root: "GROOT", Relation: "created", Covered: true, Cycle: clickhouse.AccountCohortCycle{ComputedAt: at, TipLedger: 1},
		Contracts: []clickhouse.AccountCohortContract{
			{ContractID: "CSACUSDC", Movements: 3, ActiveAccounts: 2, FirstAt: at, LastAt: at},
			{ContractID: "CUNKNOWN", Movements: 1, ActiveAccounts: 1, FirstAt: at, LastAt: at},
		},
	}
	v := h.accountCohortView(context.Background(), snap)
	if v.Contracts[0].Label != "token USDC" || v.Contracts[0].Protocol != "" {
		t.Errorf("SAC contract = %+v, want label \"token USDC\" and no protocol", v.Contracts[0])
	}
	if v.Contracts[1].Label != "" || v.Contracts[1].Protocol != "" {
		t.Errorf("unknown contract = %+v, want neither label nor protocol", v.Contracts[1])
	}
}

// Labels come before prices: a price reader that spends the whole
// request budget must not leave the contracts table labelled on a dead
// context. The stub labels only while the ctx is alive, exactly like the
// real index's roster reads.
func TestAccountCohortView_LabelsContractsBeforePricingSpendsTheBudget(t *testing.T) {
	h := &Handler{
		PricingEnabled: true,
		LookupUSDPrice: func(ctx context.Context, _ canonical.Asset) (string, bool) {
			<-ctx.Done() // the serial price walk that exhausted the 8 s budget on production
			return "", false
		},
		ContractProtocol: func(ctx context.Context, id string) (string, bool) {
			if ctx.Err() != nil || id != "CAQUARIUSPOOL" {
				return "", false
			}
			return "aquarius", true
		},
	}
	at := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	snap := clickhouse.AccountCohort{
		Root: "GROOT", Relation: "sponsored", Covered: true, Cycle: clickhouse.AccountCohortCycle{ComputedAt: at, TipLedger: 1},
		Holdings: []clickhouse.AccountCohortHolding{
			{Asset: "native", Holders: 9, Balance: big.NewInt(12_500_000_000)},
			{Asset: cohortTestUSDC, Holders: 3, Balance: big.NewInt(4_000_000_000)},
		},
		Contracts: []clickhouse.AccountCohortContract{
			{ContractID: "CAQUARIUSPOOL", Movements: 4, ActiveAccounts: 2, FirstAt: at, LastAt: at},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	v := h.accountCohortView(ctx, snap)
	if len(v.Contracts) != 1 || v.Contracts[0].Protocol != "aquarius" {
		t.Fatalf("contracts = %+v, want the pool labelled aquarius although pricing spent the budget", v.Contracts)
	}
	if v.Valuation.PricedHoldings != 0 || v.Valuation.UnpricedHoldings != 2 {
		t.Errorf("priced/unpriced = %d/%d, want 0/2 (the price reader timed out)", v.Valuation.PricedHoldings, v.Valuation.UnpricedHoldings)
	}
}

// Only the cohortPricedHoldingsCap largest holdings by balance are looked
// up; the rest are reported as unpriced over the cap, and pool shares and
// contract tokens never take a slot.
func TestAccountCohortView_PricesOnlyTheLargestHoldingsAndCountsTheRest(t *testing.T) {
	const over = 10
	looked := map[string]int{}
	h := &Handler{
		PricingEnabled: true,
		LookupUSDPrice: func(_ context.Context, a canonical.Asset) (string, bool) {
			looked[a.String()]++
			return "1", true
		},
	}
	at := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	var holdings []clickhouse.AccountCohortHolding
	// Served order is by holders, which is deliberately the REVERSE of
	// balance here so the cap is proven to rank by balance, not position.
	for i := 0; i < cohortPricedHoldingsCap+over; i++ {
		holdings = append(holdings, clickhouse.AccountCohortHolding{
			Asset:   "TOK" + strconv.Itoa(i) + "-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			Holders: uint64(1000 - i),
			Balance: big.NewInt(int64(i+1) * 10_000_000),
		})
	}
	holdings = append(holdings,
		clickhouse.AccountCohortHolding{Asset: "pool:0a1b", Holders: 1, Balance: big.NewInt(9_000_000_000_000)},
		clickhouse.AccountCohortHolding{Asset: "CCTOKEN", Holders: 1, Balance: big.NewInt(9_000_000_000_000)},
	)
	snap := clickhouse.AccountCohort{
		Root: "GROOT", Relation: "created", Covered: true, Cycle: clickhouse.AccountCohortCycle{ComputedAt: at, TipLedger: 1},
		Holdings: holdings,
	}
	v := h.accountCohortView(context.Background(), snap)

	if len(looked) != cohortPricedHoldingsCap {
		t.Fatalf("price lookups = %d, want exactly the cap %d", len(looked), cohortPricedHoldingsCap)
	}
	val := v.Valuation
	if val.PriceCap != cohortPricedHoldingsCap || val.PricedHoldings != cohortPricedHoldingsCap || val.UnpricedOverCap != over || val.UnpricedHoldings != over+2 {
		t.Fatalf("valuation = %+v, want price_cap %d, priced %d, unpriced_over_cap %d, unpriced %d",
			val, cohortPricedHoldingsCap, cohortPricedHoldingsCap, over, over+2)
	}
	for _, hd := range v.Holdings {
		if hd.Kind != "classic" {
			continue
		}
		bal, _ := strconv.Atoi(hd.Balance)
		if wantPriced := bal > over; (hd.ValueUSD != nil) != wantPriced {
			t.Errorf("%s balance %s priced=%v, want %v (the cap keeps the largest balances)", hd.Asset, hd.Balance, hd.ValueUSD != nil, wantPriced)
		}
	}
}

// Every served dollar is exact (ADR-0003): the price string is served as
// the reader gave it, and balance × price is computed and rounded as a
// rational, never through a float. 1 unit at 0.015 is 0.02 exactly;
// through float64 it is 0.0149999… and renders 0.01.
func TestAccountCohortView_USDFiguresAreExact(t *testing.T) {
	const pennyAsset = "PNY-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	h := &Handler{
		PricingEnabled: true,
		LookupUSDPrice: func(_ context.Context, a canonical.Asset) (string, bool) {
			switch a.String() {
			case pennyAsset:
				return "0.015", true
			case cohortTestUSDC:
				return "1.10", true
			}
			return "", false
		},
	}
	at := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	jul := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	snap := clickhouse.AccountCohort{
		Root: "GROOT", Relation: "created", Covered: true, Cycle: clickhouse.AccountCohortCycle{ComputedAt: at, TipLedger: 1},
		Holdings: []clickhouse.AccountCohortHolding{
			{Asset: pennyAsset, Holders: 1, Balance: big.NewInt(10_000_000)},        // 1 × 0.015 = 0.015 → 0.02
			{Asset: cohortTestUSDC, Holders: 1, Balance: big.NewInt(1_234_567_890)}, // 123.456789 × 1.10 = 135.8024679 → 135.80
		},
		Flows: []clickhouse.AccountCohortFlow{
			{Month: jul, Asset: clickhouse.CohortAllAssets, Inflow: big.NewInt(0), Outflow: big.NewInt(0), Movements: 1, ActiveAccounts: 1},
			{Month: jul, Asset: pennyAsset, Inflow: big.NewInt(30_000_000), Outflow: big.NewInt(10_000_000), Movements: 1, ActiveAccounts: 1}, // 0.045 → 0.05, 0.015 → 0.02
		},
	}
	v := h.accountCohortView(context.Background(), snap)
	want := map[string][2]string{pennyAsset: {"0.015", "0.02"}, cohortTestUSDC: {"1.10", "135.80"}}
	for _, hd := range v.Holdings {
		w := want[hd.Asset]
		if hd.PriceUSD == nil || *hd.PriceUSD != w[0] || hd.ValueUSD == nil || *hd.ValueUSD != w[1] {
			t.Errorf("%s price/value = %v/%v, want %q/%q", hd.Asset, deref(hd.PriceUSD), deref(hd.ValueUSD), w[0], w[1])
		}
	}
	if v.Valuation.TotalUSD == nil || *v.Valuation.TotalUSD != "135.82" {
		t.Errorf("total_usd = %v, want 135.82 (0.015 + 135.8024679 = 135.8174679, rounded once)", deref(v.Valuation.TotalUSD))
	}
	af := v.Flows.Points[0].ByAsset[0]
	if af.InflowUSD == nil || *af.InflowUSD != "0.05" || af.OutflowUSD == nil || *af.OutflowUSD != "0.02" {
		t.Errorf("flow USD = %v/%v, want 0.05/0.02", deref(af.InflowUSD), deref(af.OutflowUSD))
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
