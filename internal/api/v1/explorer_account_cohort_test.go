package v1_test

import (
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type cohortHoldingEnv struct {
	Asset    string  `json:"asset"`
	Kind     string  `json:"kind"`
	Holders  uint64  `json:"holders"`
	Balance  string  `json:"balance"`
	PriceUSD *string `json:"price_usd"`
	ValueUSD *string `json:"value_usd"`
}

type cohortAssetFlowEnv struct {
	Asset     string  `json:"asset"`
	Inflow    string  `json:"inflow"`
	Outflow   string  `json:"outflow"`
	Scaled    bool    `json:"scaled"`
	InflowUSD *string `json:"inflow_usd"`
}

type cohortFlowPointEnv struct {
	Period         string               `json:"period"`
	PeriodStart    string               `json:"period_start"`
	Movements      uint64               `json:"movements"`
	ActiveAccounts uint64               `json:"active_accounts"`
	ByAsset        []cohortAssetFlowEnv `json:"by_asset"`
}

type cohortEnvelope struct {
	Data struct {
		Account  string `json:"account"`
		Relation string `json:"relation"`
		Covered  bool   `json:"covered"`
		Cohort   *struct {
			Accounts     uint64 `json:"accounts"`
			LiveAccounts uint64 `json:"live_accounts"`
			Active30d    uint64 `json:"active_30d"`
		} `json:"cohort"`
		Holdings          []cohortHoldingEnv `json:"holdings"`
		HoldingsTruncated bool               `json:"holdings_truncated"`
		Valuation         struct {
			TotalUSD         *string `json:"total_usd"`
			PricedHoldings   int     `json:"priced_holdings"`
			UnpricedHoldings int     `json:"unpriced_holdings"`
			Basis            string  `json:"basis"`
		} `json:"valuation"`
		Flows struct {
			Granularity string               `json:"granularity"`
			Assets      []string             `json:"assets"`
			Points      []cohortFlowPointEnv `json:"points"`
		} `json:"flows"`
		Contracts []struct {
			ContractID     string `json:"contract_id"`
			Protocol       string `json:"protocol"`
			ActiveAccounts uint64 `json:"active_accounts"`
		} `json:"contracts"`
		Positions []struct {
			Protocol string `json:"protocol"`
			Venue    string `json:"venue"`
			Holders  uint64 `json:"holders"`
			Amount   string `json:"amount"`
		} `json:"positions"`
		Cycle *struct {
			ComputedAt string `json:"computed_at"`
			TipLedger  uint32 `json:"tip_ledger"`
		} `json:"cycle"`
		Note string `json:"note"`
	} `json:"data"`
}

// cohortPositionAmountStr is an 18-decimal-scale fold total: past 2^53, so
// it survives the wire only if nothing on the way is a float.
const cohortPositionAmountStr = "8760000000000000000000000001"

func cohortPositionAmount() *big.Int {
	v, _ := new(big.Int).SetString(cohortPositionAmountStr, 10)
	return v
}

const cohortUSDC = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

func cohortSnapshot() clickhouse.AccountCohort {
	at := time.Date(2026, 9, 17, 6, 0, 0, 0, time.UTC)
	return clickhouse.AccountCohort{
		Root: graphSubject, Covered: true,
		Cycle:          clickhouse.AccountCohortCycle{ComputedAt: at, TipLedger: 64_400_000},
		CohortAccounts: 1200, LiveAccounts: 900,
		Activity: clickhouse.AccountCohortActivity{Active30d: 40, Active90d: 90, Active365d: 300},
		Holdings: []clickhouse.AccountCohortHolding{
			{Asset: "native", Holders: 900, Balance: big.NewInt(12_500_000_000)},  // 1,250 XLM
			{Asset: cohortUSDC, Holders: 300, Balance: big.NewInt(4_000_000_000)}, // 400 USDC
			{Asset: "pool:0a1b", Holders: 5, Balance: big.NewInt(70_000_000)},     // 7 shares
			{Asset: "CCTOKEN", Holders: 2, Balance: big.NewInt(1_000)},            // raw contract units
		},
		Flows: []clickhouse.AccountCohortFlow{
			{Month: histMonth(2026, time.July), Asset: "*", Inflow: big.NewInt(0), Outflow: big.NewInt(0), Movements: 50, ActiveAccounts: 21},
			{Month: histMonth(2026, time.July), Asset: cohortUSDC, Inflow: big.NewInt(1_000_000_000), Outflow: big.NewInt(250_000_000), Movements: 30, ActiveAccounts: 12},
			{Month: histMonth(2026, time.July), Asset: "native", Inflow: big.NewInt(30_000_000), Outflow: big.NewInt(0), Movements: 20, ActiveAccounts: 15},
			{Month: histMonth(2026, time.September), Asset: "*", Inflow: big.NewInt(0), Outflow: big.NewInt(0), Movements: 3, ActiveAccounts: 2},
			{Month: histMonth(2026, time.September), Asset: "CCTOKEN", Inflow: big.NewInt(5_000), Outflow: big.NewInt(0), Movements: 3, ActiveAccounts: 2},
		},
		Contracts: []clickhouse.AccountCohortContract{
			{ContractID: "CBLENDPOOL", Movements: 40, ActiveAccounts: 9, FirstAt: at.AddDate(0, -3, 0), LastAt: at},
			{ContractID: "CUNKNOWN", Movements: 1, ActiveAccounts: 1, FirstAt: at, LastAt: at},
		},
		Positions: []clickhouse.AccountCohortPosition{
			{Protocol: "blend", PositionKind: "lending_supply", Venue: "CBLENDPOOL", Asset: "", Holders: 4, Amount: cohortPositionAmount()},
		},
	}
}

func getCohort(t *testing.T, reader *stubExplorerReader, path string) (cohortEnvelope, int) {
	t.Helper()
	base := explorerTestServer(t, reader)
	resp := mustGet(t, base+path)
	var env cohortEnvelope
	if resp.StatusCode == http.StatusOK {
		mustDecode(t, resp, &env)
	} else {
		_ = resp.Body.Close()
	}
	return env, resp.StatusCode
}

func TestAccountGraphCohort_RequiresARelation(t *testing.T) {
	reader := &stubExplorerReader{cohort: cohortSnapshot(), cohortOK: true}
	for _, q := range []string{"", "?relation=funded"} {
		if _, status := getCohort(t, reader, "/v1/accounts/"+graphSubject+"/graph/cohort"+q); status != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400", q, status)
		}
	}
}

func TestAccountGraphCohort_WarmingUntilTheFirstCycle(t *testing.T) {
	reader := &stubExplorerReader{cohortOK: false}
	if _, status := getCohort(t, reader, "/v1/accounts/"+graphSubject+"/graph/cohort?relation=created"); status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 before the first cycle", status)
	}
}

// A root the rollup does not carry is a 200 that SAYS so — never an
// empty cohort that reads as "holds nothing".
func TestAccountGraphCohort_UncoveredRootIsNotAnEmptyCohort(t *testing.T) {
	snap := cohortSnapshot()
	snap.Covered = false
	snap.Holdings, snap.Flows, snap.Contracts, snap.Positions = nil, nil, nil, nil
	reader := &stubExplorerReader{cohort: snap, cohortOK: true}
	env, status := getCohort(t, reader, "/v1/accounts/"+graphSubject+"/graph/cohort?relation=sponsored")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if env.Data.Covered {
		t.Error("covered = true for a root the rollup does not carry")
	}
	if env.Data.Cohort != nil {
		t.Error("an uncovered root must not carry cohort counts")
	}
	if env.Data.Cycle == nil || env.Data.Cycle.TipLedger != 64_400_000 {
		t.Error("the cycle is still dated so the reader knows the rollup ran")
	}
	if reader.cohortRelation != "sponsored" {
		t.Errorf("relation passed to the reader = %q, want sponsored", reader.cohortRelation)
	}
}

// Holdings are rendered in whole units, classified, and valued only
// where the live price exists: a pool share and a contract token are
// served unpriced, never dropped and never priced at zero.
func TestAccountGraphCohort_HoldingsAreClassifiedAndPricedWhereTheyCanBe(t *testing.T) {
	reader := &stubExplorerReader{cohort: cohortSnapshot(), cohortOK: true}
	env, status := getCohort(t, reader, "/v1/accounts/"+graphSubject+"/graph/cohort?relation=created")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if env.Data.Cohort == nil || env.Data.Cohort.Accounts != 1200 || env.Data.Cohort.LiveAccounts != 900 || env.Data.Cohort.Active30d != 40 {
		t.Errorf("cohort counts = %+v", env.Data.Cohort)
	}
	byAsset := map[string]cohortHoldingEnv{}
	for _, h := range env.Data.Holdings {
		byAsset[h.Asset] = h
	}
	if got := byAsset["native"]; got.Kind != "native" || got.Balance != "1250" {
		t.Errorf("native holding = %+v, want kind native balance 1250", got)
	}
	if got := byAsset[cohortUSDC]; got.Kind != "classic" || got.Balance != "400" {
		t.Errorf("USDC holding = %+v, want kind classic balance 400", got)
	}
	if got := byAsset["pool:0a1b"]; got.Kind != "pool_share" || got.Balance != "7" || got.ValueUSD != nil {
		t.Errorf("pool share = %+v, want kind pool_share, balance 7, unpriced", got)
	}
	if got := byAsset["CCTOKEN"]; got.Kind != "contract" || got.ValueUSD != nil {
		t.Errorf("contract token = %+v, want kind contract, unpriced", got)
	}
	// The explorer test server wires no price source, so nothing is
	// priced here — and that is served as "unpriced", never as $0. The
	// priced path is proven in the explorer package's unit test.
	if env.Data.Valuation.UnpricedHoldings != 4 || env.Data.Valuation.PricedHoldings != 0 || env.Data.Valuation.TotalUSD != nil {
		t.Errorf("valuation without a price source = %+v, want 4 unpriced, no total", env.Data.Valuation)
	}
	if env.Data.Valuation.Basis != "live_vwap_current" {
		t.Errorf("valuation basis = %q", env.Data.Valuation.Basis)
	}
	if env.Data.HoldingsTruncated {
		t.Error("four holdings must not read as truncated")
	}
}

// Flows keep the all-assets row as the month's headline and break the
// rest out per asset; a contract token is served in its own raw unit and
// says so; a quiet month emits no point.
func TestAccountGraphCohort_FlowsAreMonthlyAndUnitHonest(t *testing.T) {
	reader := &stubExplorerReader{cohort: cohortSnapshot(), cohortOK: true}
	env, _ := getCohort(t, reader, "/v1/accounts/"+graphSubject+"/graph/cohort?relation=created")
	pts := env.Data.Flows.Points
	if len(pts) != 2 {
		t.Fatalf("points = %d, want 2 (July, September — August was quiet): %+v", len(pts), pts)
	}
	jul := pts[0]
	if jul.Period != "2026-07" || jul.Movements != 50 || jul.ActiveAccounts != 21 {
		t.Errorf("July headline = %+v, want period 2026-07 movements 50 active 21 (the all-assets row)", jul)
	}
	if len(jul.ByAsset) != 2 {
		t.Fatalf("July by_asset = %+v, want USDC + native only (never the '*' row)", jul.ByAsset)
	}
	var usdc cohortAssetFlowEnv
	for _, a := range jul.ByAsset {
		if a.Asset == cohortUSDC {
			usdc = a
		}
		if a.Asset == "*" {
			t.Error("the all-assets row leaked into by_asset")
		}
	}
	if usdc.Inflow != "100" || usdc.Outflow != "25" || !usdc.Scaled {
		t.Errorf("USDC July flow = %+v, want inflow 100 outflow 25 scaled", usdc)
	}
	sep := pts[1]
	if sep.Period != "2026-09" || len(sep.ByAsset) != 1 || sep.ByAsset[0].Scaled || sep.ByAsset[0].Inflow != "5000" {
		t.Errorf("September = %+v, want one raw contract-unit row of 5000", sep)
	}
	if env.Data.Flows.Granularity != "1M" {
		t.Errorf("granularity = %q", env.Data.Flows.Granularity)
	}
	if len(env.Data.Flows.Assets) != 3 {
		t.Errorf("assets broken out = %v, want 3", env.Data.Flows.Assets)
	}
}

func TestAccountGraphCohort_ContractsAndPositionsAreServedAsRead(t *testing.T) {
	reader := &stubExplorerReader{cohort: cohortSnapshot(), cohortOK: true}
	env, _ := getCohort(t, reader, "/v1/accounts/"+graphSubject+"/graph/cohort?relation=created")
	if len(env.Data.Contracts) != 2 || env.Data.Contracts[0].ContractID != "CBLENDPOOL" || env.Data.Contracts[0].ActiveAccounts != 9 {
		t.Errorf("contracts = %+v", env.Data.Contracts)
	}
	if len(env.Data.Positions) != 1 || env.Data.Positions[0].Protocol != "blend" || env.Data.Positions[0].Holders != 4 || env.Data.Positions[0].Amount != cohortPositionAmountStr {
		t.Errorf("positions = %+v", env.Data.Positions)
	}
	if env.Data.Note == "" {
		t.Error("note must always be present")
	}
}
