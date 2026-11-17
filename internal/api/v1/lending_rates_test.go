package v1_test

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"slices"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// ratesLendingReader is a BLNT-aware stub whose Blend lineage is V2 for every pool but the BLNT one.
type ratesLendingReader struct {
	stubBLNTLendingReader
	blntPool string
}

func (r *ratesLendingReader) BlendPoolVersion(_ context.Context, pool string) (blend.PoolVersion, error) {
	if pool == r.blntPool {
		return blend.PoolVersionUnknown, nil
	}
	return blend.PoolV2, nil
}

// ratesExplorer serves per-pool Blend state; a pool in fail errors.
type ratesExplorer struct {
	*stubBLNTExplorer
	pools map[string]clickhouse.BlendPoolState
	fail  string
}

func (e *ratesExplorer) BlendPoolReserves(_ context.Context, pool string, _ blend.PoolVersion, _ []string, _ map[string]blend.ReserveConfig) (clickhouse.BlendPoolState, error) {
	if pool == e.fail {
		return clickhouse.BlendPoolState{}, errors.New("lake down")
	}
	return e.pools[pool], nil
}

func rateReserve(pool, asset string, supplied int64, supplyAPR, borrowAPR float64, enabled bool) clickhouse.BlendReserveState {
	return clickhouse.BlendReserveState{
		Pool: pool, Asset: asset, Decimals: 7, DecimalsFound: true,
		Config: &blend.ReserveConfig{Decimals: 7, CFactor: 9_000_000, LFactor: 9_500_000, SupplyCap: big.NewInt(1e15), Enabled: enabled},
		Metrics: blend.ReserveMetrics{
			SuppliedUnderlying: big.NewInt(supplied), BorrowedUnderlying: big.NewInt(supplied / 2),
			UtilizationPct: 50, HasAPR: true, SupplyAPR: supplyAPR, BorrowAPR: borrowAPR,
		},
	}
}

func poolInstance(status uint32) *clickhouse.PoolInstance {
	return &clickhouse.PoolInstance{Config: blend.PoolConfig{Status: status}}
}

type ratesFixture struct {
	srv                                        *v1.Server
	activePool, frozenPool, failPool, blntPool string
	xlmSAC, otherToken, usdc                   string
	explorer                                   *ratesExplorer
	lending                                    *ratesLendingReader
}

func newRatesFixture(t *testing.T) ratesFixture {
	t.Helper()
	xlmSAC, ok := xdrjson.SACContractID("native", "Public Global Stellar Network ; September 2015")
	if !ok {
		t.Fatal("native SAC id")
	}
	const usdc = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	f := ratesFixture{
		activePool: mkCStrkey(t, 1), frozenPool: mkCStrkey(t, 2), failPool: mkCStrkey(t, 3), blntPool: blnt.MainnetFixedPool,
		xlmSAC: xlmSAC, otherToken: mkCStrkey(t, 40), usdc: usdc,
	}
	rd := blnt.ReserveData{
		DRate: bigInt(t, "1002460888445"), BRate: bigInt(t, "1001344855663"), IRMod: bigInt(t, "5169744"),
		BSupply: bigInt(t, "1983236231897"), DSupply: bigInt(t, "1595659699325"), BackstopCredit: bigInt(t, "122270395"),
		Version: blend.PoolV2,
	}
	explorer := &ratesExplorer{
		stubBLNTExplorer: &stubBLNTExplorer{stubExplorerReader: &stubExplorerReader{}, data: map[string]blnt.ReserveData{usdc: rd}, bstop: 2_000_000},
		pools: map[string]clickhouse.BlendPoolState{
			f.activePool: {Instance: poolInstance(1), Reserves: []clickhouse.BlendReserveState{
				rateReserve(f.activePool, xlmSAC, 50_000_000_000, 0.01, 0.03, true),
				rateReserve(f.activePool, f.otherToken, 10_000_000, 0.20, 0.25, false),
			}},
			f.frozenPool: {Instance: poolInstance(5), Reserves: []clickhouse.BlendReserveState{
				rateReserve(f.frozenPool, xlmSAC, 10_000_000_000, 0.05, 0.02, true),
			}},
		},
		fail: f.failPool,
	}
	lending := &ratesLendingReader{
		stubBLNTLendingReader: stubBLNTLendingReader{
			stubLendingReader: stubLendingReader{pools: []timescale.BlendPoolSummary{{Pool: f.activePool}, {Pool: f.frozenPool}, {Pool: f.failPool}}},
			blntPools:         []timescale.BlendPoolSummary{{Pool: f.blntPool}},
			blntAssets:        []string{usdc},
			blntMeta: map[string][]byte{usdc: []byte(`{"index":1,"decimals":7,"c_factor":9500000,"l_factor":9500000,"util":8000000,` +
				`"max_util":9000000,"r_base":300000,"r_one":400000,"r_two":1200000,"r_three":50000000,"reactivity":20,` +
				`"supply_cap":"10000000000000000","enabled":true}`)},
		},
		blntPool: f.blntPool,
	}
	lending.assets = []string{xlmSAC, f.otherToken}
	f.explorer, f.lending = explorer, lending
	f.srv = v1.New(v1.Options{Explorer: explorer, Lending: lending, Prices: &assetKeyedPriceReader{usd: map[string]string{"native": "0.3"}}})
	return f
}

type ratesBody struct {
	Data v1.LendingRatesView `json:"data"`
}

func getRates(t *testing.T, base, query string) (ratesBody, *http.Response) {
	t.Helper()
	resp := mustGet(t, base+"/v1/lending/rates"+query)
	var body ratesBody
	if resp.StatusCode == http.StatusOK {
		mustDecode(t, resp, &body)
	}
	return body, resp
}

func findRate(rows []v1.LendingRate, pool, asset string) (v1.LendingRate, bool) {
	for _, r := range rows {
		if r.Pool == pool && r.Asset == asset {
			return r, true
		}
	}
	return v1.LendingRate{}, false
}

// TestLendingRates_AgreesWithReserves — every row equals the /reserves entry of the same pool and asset, for Blend and BLNT.
func TestLendingRates_AgreesWithReserves(t *testing.T) {
	f := newRatesFixture(t)
	base := httpTestServer(t, f.srv).URL
	body, resp := getRates(t, base, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(body.Data.Rates) != 4 {
		t.Fatalf("rates = %+v", body.Data.Rates)
	}
	for _, pool := range []string{f.activePool, f.frozenPool, f.blntPool} {
		for _, rv := range getReserves(t, f.srv, pool).Reserves {
			row, ok := findRate(body.Data.Rates, pool, rv.Asset)
			if !ok {
				t.Fatalf("no rate row for %s/%s", pool, rv.Asset)
			}
			assertRateMatchesReserve(t, row, rv)
		}
	}
	blntRow, _ := findRate(body.Data.Rates, f.blntPool, f.usdc)
	if blntRow.Protocol != "blnt" || blntRow.PoolVersion == nil || *blntRow.PoolVersion != "v3" || blntRow.SupplyAPR == nil || *blntRow.SupplyAPR != 0.0247 {
		t.Errorf("blnt row = %+v", blntRow)
	}
	xlm, _ := findRate(body.Data.Rates, f.activePool, f.xlmSAC)
	if xlm.Protocol != "blend" || xlm.PoolVersion == nil || *xlm.PoolVersion != "v2" || xlm.AssetID == nil || *xlm.AssetID != "native" ||
		xlm.SuppliedUSD == nil || *xlm.SuppliedUSD != "1500.00" || !xlm.Obtainable || len(xlm.Exclusions) != 0 {
		t.Errorf("xlm row = %+v", xlm)
	}
}

func assertRateMatchesReserve(t *testing.T, row v1.LendingRate, rv v1.ReserveView) {
	t.Helper()
	if row.Supplied != rv.Supplied || row.Borrowed != rv.Borrowed || row.UtilizationPct != rv.UtilizationPct ||
		decOrNeg(row.Decimals) != decOrNeg(rv.Decimals) || strOrNil(row.SuppliedUSD) != strOrNil(rv.SuppliedUSD) ||
		strOrNil(row.BorrowedUSD) != strOrNil(rv.BorrowedUSD) || strOrNil(row.PriceUSD) != strOrNil(rv.PriceUSD) ||
		row.PriceStale != rv.PriceStale || !floatPtrEq(row.SupplyAPR, rv.SupplyAPR) || !floatPtrEq(row.BorrowAPR, rv.BorrowAPR) {
		t.Errorf("rate %+v disagrees with reserve %+v", row, rv)
	}
	if (row.LastUpdate == nil) != (rv.LastUpdate == nil) || (row.LastUpdate != nil && *row.LastUpdate != *rv.LastUpdate) {
		t.Errorf("last_update %v != %v", row.LastUpdate, rv.LastUpdate)
	}
	if c := rv.Config; c != nil {
		if strOrNil(row.CollateralFactor) != c.CollateralFactor || strOrNil(row.LiabilityFactor) != c.LiabilityFactor ||
			strOrNil(row.SupplyCap) != strOrNil(c.SupplyCap) || row.Enabled == nil || *row.Enabled != c.Enabled {
			t.Errorf("config of %+v disagrees with %+v", row, c)
		}
	}
}

func floatPtrEq(a, b *float64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// TestLendingRates_FailingPoolIsOmittedAndDegraded — one pool's failed read names it in omitted_pools, marks the response
// degraded (no-store) and still serves every other pool.
func TestLendingRates_FailingPoolIsOmittedAndDegraded(t *testing.T) {
	f := newRatesFixture(t)
	body, resp := getRates(t, httpTestServer(t, f.srv).URL, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := body.Data.OmittedPools; len(got) != 1 || got[0].Pool != f.failPool || got[0].Protocol != "blend" || got[0].Reason == "" {
		t.Fatalf("omitted_pools = %+v", got)
	}
	if !body.Data.Partial || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("partial = %v, cache-control = %q", body.Data.Partial, resp.Header.Get("Cache-Control"))
	}
	if len(body.Data.Rates) != 4 {
		t.Errorf("rates = %d, want the other pools' 4", len(body.Data.Rates))
	}
}

// TestLendingRates_FlagsUnobtainableRates — a frozen pool's and a disabled reserve's rows are served, marked not obtainable.
func TestLendingRates_FlagsUnobtainableRates(t *testing.T) {
	f := newRatesFixture(t)
	body, _ := getRates(t, httpTestServer(t, f.srv).URL, "")
	frozen, ok := findRate(body.Data.Rates, f.frozenPool, f.xlmSAC)
	if !ok || frozen.Obtainable || !slices.Contains(frozen.Exclusions, "pool_not_active") ||
		frozen.PoolStatus == nil || *frozen.PoolStatus != "frozen" || frozen.PoolStatusCode == nil || *frozen.PoolStatusCode != 5 {
		t.Errorf("frozen row = %+v", frozen)
	}
	disabled, ok := findRate(body.Data.Rates, f.activePool, f.otherToken)
	if !ok || disabled.Obtainable || !slices.Equal(disabled.Exclusions, []string{"reserve_disabled"}) {
		t.Errorf("disabled row = %+v", disabled)
	}
}

// TestLendingRates_AssetFilterMatchesEveryXLMIdentity — native, crypto:XLM and the SAC select the same XLM rows.
func TestLendingRates_AssetFilterMatchesEveryXLMIdentity(t *testing.T) {
	f := newRatesFixture(t)
	base := httpTestServer(t, f.srv).URL
	for _, asset := range []string{"native", "crypto:XLM", f.xlmSAC} {
		body, resp := getRates(t, base, "?asset="+asset)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", asset, resp.StatusCode)
		}
		if len(body.Data.Rates) != 2 {
			t.Fatalf("%s: rates = %+v", asset, body.Data.Rates)
		}
		for _, r := range body.Data.Rates {
			if r.Asset != f.xlmSAC {
				t.Errorf("%s: row for %s", asset, r.Asset)
			}
		}
	}
	body, _ := getRates(t, base, "?asset="+f.usdc+"&protocol=blnt")
	if len(body.Data.Rates) != 1 || body.Data.Rates[0].Pool != f.blntPool || len(body.Data.OmittedPools) != 0 || body.Data.Partial {
		t.Errorf("usdc on blnt = %+v omitted %+v", body.Data.Rates, body.Data.OmittedPools)
	}
}

// TestLendingRates_Sorts — each sort key orders its rows, rows without the key last.
func TestLendingRates_Sorts(t *testing.T) {
	f := newRatesFixture(t)
	base := httpTestServer(t, f.srv).URL
	cases := []struct{ query, want0 string }{
		{"", f.otherToken},                 // supply_apr 0.20 is the highest
		{"?sort=supply_apr", f.otherToken}, // explicit default
		{"?sort=borrow_apr", f.xlmSAC},     // the frozen pool's 0.02 is the cheapest
		{"?sort=supplied_usd", f.xlmSAC},   // 1500 USD on the active pool; unpriced rows last
	}
	for _, tc := range cases {
		body, resp := getRates(t, base, tc.query)
		if resp.StatusCode != http.StatusOK || len(body.Data.Rates) != 4 || body.Data.Rates[0].Asset != tc.want0 {
			t.Fatalf("%q: status %d rows %+v", tc.query, resp.StatusCode, body.Data.Rates)
		}
		rows := body.Data.Rates
		switch tc.query {
		case "?sort=borrow_apr":
			if rows[0].Pool != f.frozenPool {
				t.Errorf("borrow_apr first = %+v", rows[0])
			}
		case "?sort=supplied_usd":
			if rows[0].Pool != f.activePool || rows[1].Pool != f.frozenPool || rows[2].SuppliedUSD != nil || rows[3].SuppliedUSD != nil {
				t.Errorf("supplied_usd order = %+v", rows)
			}
		}
	}
}

// TestLendingRates_RejectsUnknownParameters — an unknown sort or protocol, or an unparseable asset, is a 400.
func TestLendingRates_RejectsUnknownParameters(t *testing.T) {
	f := newRatesFixture(t)
	base := httpTestServer(t, f.srv).URL
	for _, q := range []string{"?sort=tvl", "?protocol=aave", "?asset=USDC"} {
		if _, resp := getRates(t, base, q); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, resp.StatusCode)
		}
	}
}
