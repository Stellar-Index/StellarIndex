package v1_test

import (
	"encoding/json"
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	scUSDC   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	scPYUSD  = "GDQE7IXJ4HUHV6RQHIUPRJSEZE4DRS5WY577O2FY6YQ5LVWZ7JZTU2V5"
	scUSDT0  = "GATISXX6BZ6NC7IKQBY37CJD4SOZL3CYZJWXEDG6JVIY4WBS6KXJHN6Q"
	scEURC   = "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
	scYUSDC  = "GDGTVWSM4MGS4T7Z6W4RPWOCHE2I6RDFCIFZGS3DOA63LWQTRNZNTTFF"
	scImpost = "GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5"

	// 2^70 stroops: above 2^63 and 2^64, so any int64/float64 detour changes it.
	scBigSupply    = "1180591620717411303424"
	scBigSupplyUSD = "118059162071741.13"
)

func scServer(t *testing.T, supply map[string]string, rows map[string][]timescale.AssetRow) *v1.Server {
	t.Helper()
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	return v1.New(v1.Options{
		VerifiedCurrencies: cat,
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            rows,
			supply:              supply,
		},
	})
}

func scGet(t *testing.T, srv *v1.Server) (v1.StablecoinsView, []byte) {
	t.Helper()
	resp := mustGet(t, httpTestServer(t, srv).URL+"/v1/stablecoins")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	var v v1.StablecoinsView
	if err := json.Unmarshal(body.Data, &v); err != nil {
		t.Fatal(err)
	}
	return v, body.Data
}

func scAsset(t *testing.T, v v1.StablecoinsView, code string) v1.StablecoinAsset {
	t.Helper()
	for _, a := range v.Assets {
		if a.Code == code {
			return a
		}
	}
	t.Fatalf("no %s in %v", code, v.Assets)
	return v1.StablecoinAsset{}
}

func scFixture(t *testing.T) (v1.StablecoinsView, []byte) {
	rows := map[string][]timescale.AssetRow{
		scUSDC:  {rwaRow("USDC", scUSDC, sptr("1"), 900000)},
		scPYUSD: {rwaRow("PYUSD", scPYUSD, sptr("0.93"), 90000)},
		scUSDT0: {rwaRow("USDT0", scUSDT0, sptr("1"), 9000)},
		scEURC:  {rwaRow("EURC", scEURC, sptr("1.08"), 9000)},
		scYUSDC: {rwaRow("yUSDC", scYUSDC, sptr("1.05"), 9000)},
		// Same code, different issuer: never queried, never served.
		scImpost: {rwaRow("USDC", scImpost, sptr("1"), 1)},
	}
	supply := map[string]string{
		"USDC-" + scUSDC:   scBigSupply,
		"PYUSD-" + scPYUSD: "5000000000",
		"EURC-" + scEURC:   "1000000000",
		"yUSDC-" + scYUSDC: "10000000",
	}
	return scGet(t, scServer(t, supply, rows))
}

func TestStablecoins_MoneyRoundTripsAboveInt64(t *testing.T) {
	v, raw := scFixture(t)
	a := scAsset(t, v, "USDC")
	if a.CirculatingSupply == nil || *a.CirculatingSupply != scBigSupply {
		t.Fatalf("circulating_supply = %v, want %s exactly", a.CirculatingSupply, scBigSupply)
	}
	if a.SupplyUSD == nil || *a.SupplyUSD != scBigSupplyUSD {
		t.Fatalf("supply_usd = %v, want %s", a.SupplyUSD, scBigSupplyUSD)
	}
	var generic struct {
		Assets []map[string]any `json:"assets"`
	}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	for _, m := range generic.Assets {
		for _, k := range []string{"circulating_supply", "price_usd", "supply_usd"} {
			if x, ok := m[k]; ok {
				if _, isStr := x.(string); !isStr {
					t.Errorf("%s is %T, want a decimal string", k, x)
				}
			}
		}
	}
}

func TestStablecoins_TotalIsALowerBoundNamingWhatIsLeftOut(t *testing.T) {
	v, _ := scFixture(t)
	if !v.Total.LowerBound {
		t.Error("lower_bound = false, want true (USDT excluded, USDT0 unvalued)")
	}
	if len(v.Excluded) != 1 || v.Excluded[0].Ticker != "USDT" || v.Excluded[0].Reason != "no_stellar_issuer" {
		t.Errorf("excluded = %v, want USDT/no_stellar_issuer", v.Excluded)
	}
	reasons := map[string]string{}
	for _, e := range v.Total.NotSummed {
		reasons[e.Ticker] = e.Reason
	}
	if reasons["EURC"] != "non_usd_peg" || reasons["yUSDC"] != "yield_bearing_wrapper" {
		t.Errorf("not_summed = %v", v.Total.NotSummed)
	}
	// USDC + PYUSD only: USDT0 has no supply, EURC and yUSDC are not summed.
	if v.Total.SupplyUSD == nil || *v.Total.SupplyUSD != "118059162072206.13" {
		t.Errorf("supply_usd = %v, want 118059162072206.13", v.Total.SupplyUSD)
	}
	if v.Total.Assets != 5 || v.Total.AssetsValued != 4 || v.Total.AssetsUnvalued != 1 {
		t.Errorf("counts = %d/%d/%d, want 5/4/1", v.Total.Assets, v.Total.AssetsValued, v.Total.AssetsUnvalued)
	}
	if got := scAsset(t, v, "USDT0").ValuationStatus; got != "supply_unavailable" {
		t.Errorf("USDT0 status = %q, want supply_unavailable", got)
	}
}

func TestStablecoins_ImpersonatorIsNotInTheSet(t *testing.T) {
	v, _ := scFixture(t)
	for _, a := range v.Assets {
		if a.Issuer == scImpost {
			t.Fatalf("impostor issuer served: %+v", a)
		}
	}
	if len(v.Assets) != 5 {
		t.Errorf("assets = %d, want the 5 catalogue members", len(v.Assets))
	}
}

func TestStablecoins_DepegPriceIsServedNotNormalised(t *testing.T) {
	v, _ := scFixture(t)
	a := scAsset(t, v, "PYUSD")
	if a.PriceUSD == nil || *a.PriceUSD != "0.93" {
		t.Fatalf("price_usd = %v, want 0.93", a.PriceUSD)
	}
	if a.SupplyUSD == nil || *a.SupplyUSD != "465.00" {
		t.Fatalf("supply_usd = %v, want 465.00 (500 tokens at 0.93)", a.SupplyUSD)
	}
}

func TestStablecoins_ByPegKeepsEURApart(t *testing.T) {
	v, _ := scFixture(t)
	got := map[string]string{}
	for _, p := range v.ByPeg {
		if p.SupplyUSD != nil {
			got[p.Peg] = *p.SupplyUSD
		}
	}
	if got["EUR"] != "108.00" {
		t.Errorf("by_peg = %v, want EUR 108.00", got)
	}
}

func TestStablecoins_NothingValuedOmitsTotal(t *testing.T) {
	v, _ := scGet(t, scServer(t, nil, map[string][]timescale.AssetRow{}))
	if v.Total.SupplyUSD != nil {
		t.Errorf("supply_usd = %q, want absent", *v.Total.SupplyUSD)
	}
	if !v.Total.LowerBound {
		t.Error("lower_bound = false, want true")
	}
}
