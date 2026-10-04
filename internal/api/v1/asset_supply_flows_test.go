package v1_test

import (
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func supplyFlowsServer(t *testing.T, supply v1.TokenSupplyReader) string {
	t.Helper()
	return httpTestServer(t, v1.New(v1.Options{TokenSupply: supply})).URL
}

func flowDay(contract string, day time.Time, mint, burn, clawback string, flows uint64) clickhouse.SupplyFlowDay {
	m, b, c := mustBigInt(mint), mustBigInt(burn), mustBigInt(clawback)
	net := new(big.Int).Sub(m, new(big.Int).Add(b, c))
	return clickhouse.SupplyFlowDay{ContractID: contract, Day: day, Net: net, Mint: m, Burn: b, Clawback: c, Flows: flows}
}

func TestAssetSupplyFlows_DailyPerKindSeries(t *testing.T) {
	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	sac := mustSAC(t, usdc)
	d1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	stub := &stubFlowSupply{days: []clickhouse.SupplyFlowDay{
		// > 2^63: must survive as an exact string.
		flowDay(sac, d1, "18446744073709551621", "0", "0", 1),
		flowDay(sac, d1.AddDate(0, 0, 2), "100", "40", "10", 3),
		flowDay(mkCStrkey(t, 9), d1, "5", "0", "0", 1), // another contract
	}}
	base := supplyFlowsServer(t, stub)

	resp := mustGet(t, base+"/v1/assets/"+usdc+"/supply/flows")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.AssetSupplyFlows `json:"data"`
	}
	mustDecode(t, resp, &body)
	got := body.Data
	if got.ContractID != sac || got.AssetID != usdc || got.HistoryIncomplete {
		t.Fatalf("header = %+v, want contract %s, complete history", got, sac)
	}
	want := []v1.AssetSupplyFlowDay{
		{Day: "2026-09-01", Mint: "18446744073709551621", Burn: "0", Clawback: "0", Net: "18446744073709551621", Flows: 1},
		{Day: "2026-09-03", Mint: "100", Burn: "40", Clawback: "10", Net: "50", Flows: 3},
	}
	if len(got.Days) != len(want) {
		t.Fatalf("days = %+v, want %+v", got.Days, want)
	}
	for i := range want {
		if got.Days[i] != want[i] {
			t.Fatalf("day %d = %+v, want %+v", i, got.Days[i], want[i])
		}
	}
}

// Burns recorded before any mint mean the lake is missing earlier mints: the
// flows are still facts, but the series must say it cannot be cumulated.
func TestAssetSupplyFlows_NegativeRunningLevelFlagsIncomplete(t *testing.T) {
	c := mkCStrkey(t, 4)
	d1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	base := supplyFlowsServer(t, &stubFlowSupply{days: []clickhouse.SupplyFlowDay{
		flowDay(c, d1, "0", "70", "0", 1),
		flowDay(c, d1.AddDate(0, 0, 1), "100", "0", "0", 1),
	}})
	resp := mustGet(t, base+"/v1/assets/"+c+"/supply/flows")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.AssetSupplyFlows `json:"data"`
	}
	mustDecode(t, resp, &body)
	if !body.Data.HistoryIncomplete || len(body.Data.Days) != 2 {
		t.Fatalf("got %+v, want both days and history_incomplete=true", body.Data)
	}
}

func TestAssetSupplyFlows_Refusals(t *testing.T) {
	flows := supplyFlowsServer(t, &stubFlowSupply{})
	for _, tc := range []struct {
		url  string
		want int
	}{
		{flows + "/v1/assets/native/supply/flows", http.StatusNotFound},
		{flows + "/v1/assets/fiat:USD/supply/flows", http.StatusNotFound},
		{supplyFlowsServer(t, nil) + "/v1/assets/" + mkCStrkey(t, 4) + "/supply/flows", http.StatusServiceUnavailable},
	} {
		if resp := mustGet(t, tc.url); resp.StatusCode != tc.want {
			t.Fatalf("%s: status = %d, want %d", tc.url, resp.StatusCode, tc.want)
		}
	}
}
