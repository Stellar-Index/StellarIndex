package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

const blndAssetID = "BLND-GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"

// protocolsExplorer adds a backstop read to the rates fixture: one tier of 100 tokens per pool.
type protocolsExplorer struct {
	*ratesExplorer
}

func (e *protocolsExplorer) LendingBackstop(_ context.Context, _ string, tiered bool) (clickhouse.LendingBackstopState, error) {
	st := clickhouse.LendingBackstopState{Instance: &clickhouse.PoolInstance{Backstop: mkBackstopID}}
	tier := clickhouse.BackstopTierState{Balance: &clickhouse.BackstopBalance{Tokens: big.NewInt(100), Shares: big.NewInt(90), Q4W: big.NewInt(0)}}
	if tiered {
		tier.Tier, tier.Token = "FirstLoss", "CBLNTTIERTOKEN"
	} else {
		st.Token = "CBLENDLPTOKEN"
	}
	st.Tiers = []clickhouse.BackstopTierState{tier}
	return st, nil
}

const mkBackstopID = "CBACKSTOP"

func newProtocolsServer(t *testing.T) (ratesFixture, *v1.Server) {
	t.Helper()
	f := newRatesFixture(t)
	srv := v1.New(v1.Options{
		Explorer: &protocolsExplorer{ratesExplorer: f.explorer},
		Lending:  f.lending,
		Prices:   &assetKeyedPriceReader{usd: map[string]string{"native": "0.3", blndAssetID: "0.05"}},
		Supply:   &stubSupplyLooker{hit: true, snap: supply.Supply{CirculatingSupply: big.NewInt(1e14), TotalSupply: big.NewInt(2e14)}},
	})
	return f, srv
}

func getProtocols(t *testing.T, srv *v1.Server) ([]v1.LendingProtocolStats, *http.Response) {
	t.Helper()
	resp := mustGet(t, httpTestServer(t, srv).URL+"/v1/lending/protocols")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data []v1.LendingProtocolStats `json:"data"`
	}
	mustDecode(t, resp, &body)
	if len(body.Data) != 2 || body.Data[0].Protocol != "blend" || body.Data[1].Protocol != "blnt" {
		t.Fatalf("protocols = %+v", body.Data)
	}
	return body.Data, resp
}

func hasProtocolExclusion(ex []v1.LendingStatsExclusion, kind, pool, asset string) bool {
	for _, e := range ex {
		if e.Kind == kind && strOrNil(e.Pool) == pool && (e.Asset == nil && asset == "" || strOrNil(e.Asset) == asset) && e.Reason != "" {
			return true
		}
	}
	return false
}

// TestLendingProtocols_TotalsEqualReservesViews — each protocol's totals are the exact sums of its pools' /reserves figures.
func TestLendingProtocols_TotalsEqualReservesViews(t *testing.T) {
	f, srv := newProtocolsServer(t)
	stats, _ := getProtocols(t, srv)
	pools := map[string][]string{"blend": {f.activePool, f.frozenPool}, "blnt": {f.blntPool}}
	for _, st := range stats {
		supplied, borrowed := new(big.Rat), new(big.Rat)
		priced, reserves := 0, 0
		for _, pool := range pools[st.Protocol] {
			view := getReserves(t, srv, pool)
			reserves += len(view.OmittedReserves)
			for _, rv := range view.Reserves {
				reserves++
				if rv.SuppliedUSD == nil || rv.BorrowedUSD == nil {
					continue
				}
				s, _ := new(big.Rat).SetString(*rv.SuppliedUSD)
				b, _ := new(big.Rat).SetString(*rv.BorrowedUSD)
				supplied.Add(supplied, s)
				borrowed.Add(borrowed, b)
				priced++
			}
		}
		if st.ReserveCount != reserves {
			t.Errorf("%s: reserve_count = %d, want %d", st.Protocol, st.ReserveCount, reserves)
		}
		if priced == 0 {
			if st.TotalSuppliedUSD != nil || st.TotalBorrowedUSD != nil || st.TVLUSD != nil {
				t.Errorf("%s: nothing priced but totals %+v", st.Protocol, st)
			}
			continue
		}
		tvl := new(big.Rat).Sub(supplied, borrowed)
		if strOrNil(st.TotalSuppliedUSD) != supplied.FloatString(2) || strOrNil(st.TotalBorrowedUSD) != borrowed.FloatString(2) ||
			strOrNil(st.TVLUSD) != tvl.FloatString(2) {
			t.Errorf("%s: totals %v/%v/%v, want %s/%s/%s", st.Protocol, strOrNil(st.TotalSuppliedUSD), strOrNil(st.TotalBorrowedUSD),
				strOrNil(st.TVLUSD), supplied.FloatString(2), borrowed.FloatString(2), tvl.FloatString(2))
		}
	}
	blend := stats[0]
	if strOrNil(blend.TotalSuppliedUSD) != "1800.00" || strOrNil(blend.TotalBorrowedUSD) != "900.00" || strOrNil(blend.TVLUSD) != "900.00" {
		t.Errorf("blend totals = %+v", blend)
	}
	if blend.PoolCount != 3 || stats[1].PoolCount != 1 {
		t.Errorf("pool counts = %d, %d", blend.PoolCount, stats[1].PoolCount)
	}
	if len(blend.Backstop) != 1 || blend.Backstop[0].BackstopToken != "CBLENDLPTOKEN" || blend.Backstop[0].Tokens != "200" {
		t.Errorf("blend backstop = %+v", blend.Backstop)
	}
	if b := stats[1].Backstop; len(b) != 1 || b[0].BackstopToken != "CBLNTTIERTOKEN" || b[0].Tokens != "100" {
		t.Errorf("blnt backstop = %+v", b)
	}
}

// TestLendingProtocols_FailingPoolIsALowerBound — a pool whose reserves cannot be read is named and makes the totals a lower bound.
func TestLendingProtocols_FailingPoolIsALowerBound(t *testing.T) {
	f, srv := newProtocolsServer(t)
	stats, resp := getProtocols(t, srv)
	if !stats[0].LowerBound || !hasProtocolExclusion(stats[0].Excluded, "pool", f.failPool, "") {
		t.Errorf("blend = %+v", stats[0])
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("cache-control = %q", resp.Header.Get("Cache-Control"))
	}
}

// TestLendingProtocols_UnpricedReserveIsALowerBound — a reserve with no USD value is named and left out of every total.
func TestLendingProtocols_UnpricedReserveIsALowerBound(t *testing.T) {
	f, srv := newProtocolsServer(t)
	stats, _ := getProtocols(t, srv)
	if !stats[0].LowerBound || !hasProtocolExclusion(stats[0].Excluded, "reserve", f.activePool, f.otherToken) {
		t.Errorf("blend excluded = %+v", stats[0].Excluded)
	}
	if hasProtocolExclusion(stats[0].Excluded, "reserve", f.activePool, f.xlmSAC) {
		t.Errorf("priced XLM reserve excluded: %+v", stats[0].Excluded)
	}
}

// TestLendingProtocols_TokenValuation — the token reads the asset price and supply; with no price the market cap is absent.
func TestLendingProtocols_TokenValuation(t *testing.T) {
	_, srv := newProtocolsServer(t)
	stats, _ := getProtocols(t, srv)
	blnd, blntTok := stats[0].Token, stats[1].Token
	if blnd.AssetID != blndAssetID || blnd.Code != "BLND" || blnd.Issuer == "" || strOrNil(blnd.PriceUSD) == "" ||
		strOrNil(blnd.CirculatingSupply) != "100000000000000" || strOrNil(blnd.TotalSupply) != "200000000000000" {
		t.Errorf("blnd token = %+v", blnd)
	}
	if mc, _ := new(big.Rat).SetString(strOrNil(blnd.MarketCapUSD)); mc == nil || mc.Cmp(big.NewRat(500_000, 1)) != 0 {
		t.Errorf("blnd market_cap_usd = %q, want 500000", strOrNil(blnd.MarketCapUSD))
	}
	if blntTok.Code != "BLNT" || blntTok.PriceUSD != nil || blntTok.MarketCapUSD != nil || blntTok.CirculatingSupply == nil {
		t.Errorf("blnt token = %+v", blntTok)
	}
}
