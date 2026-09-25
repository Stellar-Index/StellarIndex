package explorer

import (
	"context"
	"encoding/json"
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
	if !v.Valuation.Degraded {
		t.Errorf("valuation.degraded = false, want true: these two holdings read as unpriced only because the " +
			"context expired mid-walk (RLT-194), not because the price reader genuinely had no price for them")
	}
}

// A cohort priced within its budget — no context expiry — must NOT be
// reported degraded: Degraded distinguishes "the walk was cut short" from
// the ordinary case of some holdings genuinely having no live price.
func TestAccountCohortView_ValuationNotDegradedWhenPricingCompletesInBudget(t *testing.T) {
	h := &Handler{
		PricingEnabled: true,
		LookupUSDPrice: func(_ context.Context, a canonical.Asset) (string, bool) {
			if a.String() == "native" {
				return "0.10", true
			}
			return "", false
		},
	}
	at := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	snap := clickhouse.AccountCohort{
		Root: "GROOT", Relation: "created", Covered: true, Cycle: clickhouse.AccountCohortCycle{ComputedAt: at, TipLedger: 1},
		Holdings: []clickhouse.AccountCohortHolding{
			{Asset: "native", Holders: 9, Balance: big.NewInt(12_500_000_000)},
			{Asset: cohortTestUSDC, Holders: 3, Balance: big.NewInt(4_000_000_000)},
		},
	}
	v := h.accountCohortView(context.Background(), snap)
	if v.Valuation.PricedHoldings != 1 || v.Valuation.UnpricedHoldings != 1 {
		t.Fatalf("priced/unpriced = %d/%d, want 1/1", v.Valuation.PricedHoldings, v.Valuation.UnpricedHoldings)
	}
	if v.Valuation.Degraded {
		t.Errorf("valuation.degraded = true, want false: nothing here timed out, USDC simply has no live price")
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

// A month's flows are valued a second time at THAT month's own price:
// exact big.Rat, rounded once at the end, absent (never zero) where the
// reader joined no month price or the price is not a positive number,
// never applied to an unscaled contract token, and summed on the month
// point over the priced assets only — exactly, so two half-cents make
// one cent, not the two a sum of rounded strings would give.
func TestAccountCohortView_ValuesFlowsAtTheMonthsOwnPrice(t *testing.T) {
	h := &Handler{
		PricingEnabled: true,
		LookupUSDPrice: func(_ context.Context, a canonical.Asset) (string, bool) {
			switch a.String() {
			case "native":
				return "0.10", true
			case cohortTestUSDC:
				return "1", true
			case "AQUA-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
				"YBX-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN":
				return "0.001", true
			}
			return "", false
		},
	}
	const (
		aqua = "AQUA-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		ybx  = "YBX-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		eurc = "EURC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	)
	str := func(s string) *string { return &s }
	at := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	jul := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	aug := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	sep := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	snap := clickhouse.AccountCohort{
		Root: "GROOT", Relation: "created", Covered: true,
		Cycle: clickhouse.AccountCohortCycle{ComputedAt: at, TipLedger: 1},
		Flows: []clickhouse.AccountCohortFlow{
			{Month: jul, Asset: clickhouse.CohortAllAssets, Inflow: big.NewInt(0), Outflow: big.NewInt(0), Movements: 5, ActiveAccounts: 2},
			// 100 USDC in, 25 out, at 0.998 that month → 99.80 / 24.95
			{Month: jul, Asset: cohortTestUSDC, Inflow: big.NewInt(1_000_000_000), Outflow: big.NewInt(250_000_000), Movements: 3, ActiveAccounts: 1, PriceUSDThen: str("0.998")},
			// 3 XLM in at 0.3333333333 → 0.9999999999 → 1.00 (rounded once)
			{Month: jul, Asset: "native", Inflow: big.NewInt(30_000_000), Outflow: big.NewInt(0), Movements: 1, ActiveAccounts: 1, PriceUSDThen: str("0.3333333333")},
			// a contract token is never priced, even with a month price
			{Month: jul, Asset: "CCTOKEN", Inflow: big.NewInt(5_000), Outflow: big.NewInt(0), Movements: 1, ActiveAccounts: 1, PriceUSDThen: str("2")},

			{Month: aug, Asset: clickhouse.CohortAllAssets, Inflow: big.NewInt(0), Outflow: big.NewInt(0), Movements: 4, ActiveAccounts: 3},
			// two half-cents: 10 units × 0.0005 = 0.005 each
			{Month: aug, Asset: aqua, Inflow: big.NewInt(100_000_000), Outflow: big.NewInt(0), Movements: 1, ActiveAccounts: 1, PriceUSDThen: str("0.0005")},
			{Month: aug, Asset: ybx, Inflow: big.NewInt(100_000_000), Outflow: big.NewInt(0), Movements: 1, ActiveAccounts: 1, PriceUSDThen: str("0.0005")},
			// a zero price is a miss, never a zero dollar
			{Month: aug, Asset: eurc, Inflow: big.NewInt(70_000_000), Outflow: big.NewInt(0), Movements: 1, ActiveAccounts: 1, PriceUSDThen: str("0")},
			// no month price joined
			{Month: aug, Asset: cohortTestUSDC, Inflow: big.NewInt(10_000_000), Outflow: big.NewInt(0), Movements: 1, ActiveAccounts: 1},

			{Month: sep, Asset: clickhouse.CohortAllAssets, Inflow: big.NewInt(0), Outflow: big.NewInt(0), Movements: 1, ActiveAccounts: 1},
			{Month: sep, Asset: eurc, Inflow: big.NewInt(10_000_000), Outflow: big.NewInt(0), Movements: 1, ActiveAccounts: 1},
		},
	}
	v := h.accountCohortView(context.Background(), snap)
	if len(v.Flows.Points) != 3 {
		t.Fatalf("points = %d, want 3", len(v.Flows.Points))
	}
	byAsset := func(p AccountCohortFlowPointV, asset string) AccountCohortAssetFlowV {
		for _, af := range p.ByAsset {
			if af.Asset == asset {
				return af
			}
		}
		t.Fatalf("%s: no %s row", p.Period, asset)
		return AccountCohortAssetFlowV{}
	}

	julP := v.Flows.Points[0]
	usdc := byAsset(julP, cohortTestUSDC)
	if deref(usdc.InflowUSDThen) != "99.80" || deref(usdc.OutflowUSDThen) != "24.95" || deref(usdc.PriceUSDThen) != "0.998" {
		t.Errorf("USDC July then = in %s out %s price %s, want 99.80 / 24.95 / 0.998", deref(usdc.InflowUSDThen), deref(usdc.OutflowUSDThen), deref(usdc.PriceUSDThen))
	}
	if deref(usdc.InflowUSD) != "100.00" || deref(usdc.OutflowUSD) != "25.00" {
		t.Errorf("USDC July today-priced figures changed: in %s out %s", deref(usdc.InflowUSD), deref(usdc.OutflowUSD))
	}
	xlm := byAsset(julP, "native")
	if deref(xlm.InflowUSDThen) != "1.00" || deref(xlm.OutflowUSDThen) != "0.00" {
		t.Errorf("XLM July then = in %s out %s, want 1.00 / 0.00", deref(xlm.InflowUSDThen), deref(xlm.OutflowUSDThen))
	}
	if tok := byAsset(julP, "CCTOKEN"); tok.InflowUSDThen != nil || tok.PriceUSDThen != nil {
		t.Errorf("contract token must never be then-priced: %+v", tok)
	}
	if deref(julP.InflowUSDThen) != "100.80" || deref(julP.OutflowUSDThen) != "24.95" {
		t.Errorf("July point then = in %s out %s, want 100.80 / 24.95 (99.80 + 0.9999999999, rounded once)", deref(julP.InflowUSDThen), deref(julP.OutflowUSDThen))
	}

	augP := v.Flows.Points[1]
	for _, a := range []string{aqua, ybx} {
		if af := byAsset(augP, a); deref(af.InflowUSDThen) != "0.01" {
			t.Errorf("%s August then = %s, want 0.01", a, deref(af.InflowUSDThen))
		}
	}
	if af := byAsset(augP, eurc); af.InflowUSDThen != nil || af.PriceUSDThen != nil {
		t.Errorf("a zero month price must be a miss, got %+v", af)
	}
	if af := byAsset(augP, cohortTestUSDC); af.InflowUSDThen != nil || af.PriceUSDThen != nil {
		t.Errorf("no month price must leave the then figures absent, got %+v", af)
	}
	if deref(augP.InflowUSDThen) != "0.01" || deref(augP.OutflowUSDThen) != "0.00" {
		t.Errorf("August point then = in %s out %s, want 0.01 / 0.00 (the exact sum 0.005 + 0.005 rounded once, not 0.01 + 0.01)", deref(augP.InflowUSDThen), deref(augP.OutflowUSDThen))
	}

	sepP := v.Flows.Points[2]
	if sepP.InflowUSDThen != nil || sepP.OutflowUSDThen != nil {
		t.Errorf("a month with no then-priced asset must omit the point sums, got in %s out %s", deref(sepP.InflowUSDThen), deref(sepP.OutflowUSDThen))
	}
}

// A month point's today and then figures value ONE basket — the assets
// priced on both bases — so switching the chart's basis changes the price,
// never the asset set. An asset priced on one basis alone is in neither
// point sum, while its own by_asset row keeps the figure it has. Asserted
// on the wire shape the explorer chart reads.
func TestAccountCohortView_FlowPointSumsShareOneBasket(t *testing.T) {
	const (
		todayOnly = "AQUA-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		thenOnly  = "YBX-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	)
	price := func(a string) (cohortPrice, bool) {
		switch a {
		case cohortTestUSDC:
			return cohortPrice{Text: "1", Rat: big.NewRat(1, 1)}, true
		case todayOnly:
			return cohortPrice{Text: "2", Rat: big.NewRat(2, 1)}, true
		}
		return cohortPrice{}, false
	}
	str := func(s string) *string { return &s }
	jul := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	flows := []clickhouse.AccountCohortFlow{
		{Month: jul, Asset: clickhouse.CohortAllAssets, Inflow: big.NewInt(0), Outflow: big.NewInt(0), Movements: 3, ActiveAccounts: 1},
		// 10 in, 5 out: 10.00 / 5.00 today, 9.90 / 4.95 at 0.99 then
		{Month: jul, Asset: cohortTestUSDC, Inflow: big.NewInt(100_000_000), Outflow: big.NewInt(50_000_000), Movements: 1, ActiveAccounts: 1, PriceUSDThen: str("0.99")},
		// 100 in at 2 today = 200.00; no month price
		{Month: jul, Asset: todayOnly, Inflow: big.NewInt(1_000_000_000), Outflow: big.NewInt(0), Movements: 1, ActiveAccounts: 1},
		// 100 in at 3 then = 300.00; no live price
		{Month: jul, Asset: thenOnly, Inflow: big.NewInt(1_000_000_000), Outflow: big.NewInt(0), Movements: 1, ActiveAccounts: 1, PriceUSDThen: str("3")},
	}
	p := cohortFlowsView(flows, price).Points[0]
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{
		"priced_assets":    float64(1),
		"inflow_usd":       "10.00",
		"outflow_usd":      "5.00",
		"inflow_usd_then":  "9.90",
		"outflow_usd_then": "4.95",
	} {
		if got[k] != want {
			t.Errorf("point %s = %v, want %v (the USDC-only basket)", k, got[k], want)
		}
	}
	for _, af := range p.ByAsset {
		switch af.Asset {
		case todayOnly:
			if deref(af.InflowUSD) != "200.00" || af.InflowUSDThen != nil {
				t.Errorf("today-only row = %s / %s, want 200.00 / <nil>", deref(af.InflowUSD), deref(af.InflowUSDThen))
			}
		case thenOnly:
			if af.InflowUSD != nil || deref(af.InflowUSDThen) != "300.00" {
				t.Errorf("then-only row = %s / %s, want <nil> / 300.00", deref(af.InflowUSD), deref(af.InflowUSDThen))
			}
		}
	}
}

// A month whose basket is empty emits no point USD figure on either basis,
// and says so with priced_assets 0 rather than a zero dollar amount.
func TestAccountCohortView_FlowPointEmptyBasketOmitsBothBases(t *testing.T) {
	price := func(a string) (cohortPrice, bool) {
		if a == cohortTestUSDC {
			return cohortPrice{Text: "1", Rat: big.NewRat(1, 1)}, true
		}
		return cohortPrice{}, false
	}
	jul := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	flows := []clickhouse.AccountCohortFlow{
		{Month: jul, Asset: cohortTestUSDC, Inflow: big.NewInt(100_000_000), Outflow: big.NewInt(0), Movements: 1, ActiveAccounts: 1},
	}
	raw, err := json.Marshal(cohortFlowsView(flows, price).Points[0])
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["priced_assets"] != float64(0) {
		t.Errorf("priced_assets = %v, want 0", got["priced_assets"])
	}
	for _, k := range []string{"inflow_usd", "outflow_usd", "inflow_usd_then", "outflow_usd_then"} {
		if v, ok := got[k]; ok {
			t.Errorf("point %s = %v, want absent for an empty basket", k, v)
		}
	}
}

// The doc comment on AccountCohortFlowsV.Assets promises "the cohort's
// most moved first". The rows arrive ordered by (month, asset) for
// grouping into points, so building Assets by first appearance yields
// alphabetic order within the earliest month instead — this pins the
// ranked order the field actually promises.
func TestAccountCohortView_FlowAssetsOrderedByMovementNotFirstAppearance(t *testing.T) {
	noPrice := func(string) (cohortPrice, bool) { return cohortPrice{}, false }
	jul := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	aug := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	flows := []clickhouse.AccountCohortFlow{
		// "AAA" sorts before "ZEBRA" alphabetically and appears first, but
		// "ZEBRA" moves far more once August is counted.
		{Month: jul, Asset: "AAA", Inflow: big.NewInt(1), Outflow: big.NewInt(0), Movements: 1, ActiveAccounts: 1},
		{Month: jul, Asset: "ZEBRA", Inflow: big.NewInt(1), Outflow: big.NewInt(0), Movements: 1, ActiveAccounts: 1},
		{Month: aug, Asset: "ZEBRA", Inflow: big.NewInt(1), Outflow: big.NewInt(0), Movements: 100, ActiveAccounts: 1},
	}
	out := cohortFlowsView(flows, noPrice)
	want := []string{"ZEBRA", "AAA"}
	if len(out.Assets) != 2 || out.Assets[0] != want[0] || out.Assets[1] != want[1] {
		t.Errorf("flows.assets = %v, want %v (most moved first)", out.Assets, want)
	}
}

// AssetsTruncated must say when the reader's per-cohort cap
// (clickhouse.CohortFlowAssetsLimit) left assets out, mirroring
// HoldingsTruncated.
func TestAccountCohortView_FlowAssetsTruncatedFlag(t *testing.T) {
	noPrice := func(string) (cohortPrice, bool) { return cohortPrice{}, false }
	jul := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	full := make([]clickhouse.AccountCohortFlow, 0, clickhouse.CohortFlowAssetsLimit)
	for i := 0; i < clickhouse.CohortFlowAssetsLimit; i++ {
		asset := string(rune('A' + i))
		full = append(full, clickhouse.AccountCohortFlow{Month: jul, Asset: asset, Inflow: big.NewInt(1), Outflow: big.NewInt(0), Movements: 1, ActiveAccounts: 1})
	}
	if got := cohortFlowsView(full, noPrice).AssetsTruncated; !got {
		t.Errorf("assets_truncated = %v at the cap (%d assets), want true", got, clickhouse.CohortFlowAssetsLimit)
	}

	partial := []clickhouse.AccountCohortFlow{
		{Month: jul, Asset: "AAA", Inflow: big.NewInt(1), Outflow: big.NewInt(0), Movements: 1, ActiveAccounts: 1},
	}
	if got := cohortFlowsView(partial, noPrice).AssetsTruncated; got {
		t.Errorf("assets_truncated = %v under the cap (1 asset), want false", got)
	}
}
