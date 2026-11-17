package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

const xlm = int64(10_000_000) // one XLM in stroops

// usdLeg is one reserve of the fixture pool: on-chain b/d-tokens at rate 1.0, so tokens are underlying units.
type usdLeg struct {
	asset             string
	supply, borrow    int64 // on-chain tokens; 0 = no leg
	supplyNet, borNet int64
}

// earningsUSDServer serves one Blend pool of XLM (priced $2 with supply APR 0.05, borrow APR 0.1) and any unpriced reserves.
func earningsUSDServer(t *testing.T, pool string, legs []usdLeg, rateLastTime uint64, supplyActivity time.Time) *v1.Server {
	t.Helper()
	return earningsUSDServerPriced(t, pool, legs, rateLastTime, supplyActivity, false)
}

func earningsUSDServerPriced(t *testing.T, pool string, legs []usdLeg, rateLastTime uint64, supplyActivity time.Time, staleXLM bool) *v1.Server {
	t.Helper()
	ex := &stubEarningsExplorer{stubExplorerReader: &stubExplorerReader{}, positions: clickhouse.LendingAccountPositions{
		Readable: true, Supply: map[string]*big.Int{}, Borrow: map[string]*big.Int{},
	}}
	pr := &stubEarningsPositions{stubPositionsReader: &stubPositionsReader{}}
	var assets []string
	for _, l := range legs {
		assets = append(assets, l.asset)
		ex.reserves = append(ex.reserves, clickhouse.BlendReserveState{
			Pool: pool, Asset: l.asset, Decimals: 7, DecimalsFound: true,
			Data: blend.ReserveData{
				BRate: big.NewInt(1_000_000_000_000), DRate: big.NewInt(1_000_000_000_000),
				IRMod: new(big.Int), BSupply: new(big.Int), DSupply: new(big.Int), BackstopCredit: new(big.Int), Version: blend.PoolV2,
				LastTime: rateLastTime,
			},
			Metrics: blend.ReserveMetrics{
				SuppliedUnderlying: big.NewInt(1_000 * xlm), BorrowedUnderlying: big.NewInt(500 * xlm),
				HasAPR: true, SupplyAPR: 0.05, BorrowAPR: 0.1,
			},
		})
		f := timescale.BlendPositionFold{Pool: pool, Asset: l.asset, SupplyLastActivity: supplyActivity}
		if l.supply > 0 {
			ex.positions.Supply[l.asset] = big.NewInt(l.supply)
			f.HasSupplyLeg, f.SupplyNet, f.SupplyTokens = true, big.NewInt(l.supplyNet).String(), big.NewInt(l.supply).String()
		}
		if l.borrow > 0 {
			ex.positions.Borrow[l.asset] = big.NewInt(l.borrow)
			f.HasBorrowLeg, f.BorrowNet, f.BorrowTokens = true, big.NewInt(l.borNet).String(), big.NewInt(l.borrow).String()
		}
		pr.blend = append(pr.blend, f)
	}
	return v1.New(v1.Options{
		Explorer: ex, Positions: pr, Lending: &stubLendingReader{assets: assets}, CompletenessReader: verifiedLendingHistory(),
		Prices: &stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{"native/fiat:USD": {Price: "2"}},
			stale:     map[string]bool{"native/fiat:USD": staleXLM},
		},
	})
}

func nativeSAC(t *testing.T) string {
	t.Helper()
	sac, ok := xdrjson.SACContractID("native", "Public Global Stellar Network ; September 2015")
	if !ok {
		t.Fatal("native SAC id")
	}
	return sac
}

// TestLendingEarningsUSD_FullyPriced: 100 XLM supplied and 30 borrowed at $2 total $200 / $60 / $140; net APR is
// (200x0.05 - 60x0.1)/140 = 0.028571... -> 0.0286; each leg is valued at the price /reserves serves for the same reserve.
func TestLendingEarningsUSD_FullyPriced(t *testing.T) {
	pool, sac := mkCStrkey(t, 1), nativeSAC(t)
	srv := earningsUSDServer(t, pool, []usdLeg{{asset: sac, supply: 100 * xlm, supplyNet: 90 * xlm, borrow: 30 * xlm, borNet: 25 * xlm}}, 0, time.Time{})
	got := getEarnings(t, srv, earningsAccount, http.StatusOK)
	if len(got.Legs) != 2 || len(got.Pools) != 1 {
		t.Fatalf("legs = %+v pools = %+v", got.Legs, got.Pools)
	}
	sup, bor := got.Legs[0], got.Legs[1]
	if strOrNull(sup.ValueUSD) != "200.00" || strOrNull(sup.AccruedUSD) != "20.00" || sup.UsdReason != nil {
		t.Fatalf("supply = %+v", sup)
	}
	if strOrNull(bor.ValueUSD) != "60.00" || strOrNull(bor.AccruedUSD) != "10.00" {
		t.Fatalf("borrow = %+v", bor)
	}
	for _, tot := range []v1.LendingUSDTotals{got.Pools[0].LendingUSDTotals, got.Totals} {
		if tot.SuppliedUSD != "200.00" || tot.BorrowedUSD != "60.00" || strOrNull(tot.NetUSD) != "140.00" || tot.NetUSDReason != nil ||
			tot.LowerBound || len(tot.Excluded) != 0 || strOrNull(tot.NetAPR) != "0.0286" || tot.NetAPRReason != nil {
			t.Fatalf("totals = %+v apr=%s", tot, strOrNull(tot.NetAPR))
		}
	}
	res := getReserves(t, srv, pool)
	if len(res.Reserves) != 1 || res.Reserves[0].PriceUSD == nil {
		t.Fatalf("reserves = %+v", res.Reserves)
	}
	price, _ := new(big.Rat).SetString(*res.Reserves[0].PriceUSD)
	want := new(big.Rat).Mul(price, big.NewRat(100*xlm, xlm))
	if want.FloatString(2) != *sup.ValueUSD {
		t.Fatalf("value_usd %s is not 100 XLM at the /reserves price %s", *sup.ValueUSD, *res.Reserves[0].PriceUSD)
	}
}

// TestLendingEarningsUSD_UnpricedLegIsALowerBound: a reserve with no USD price leaves its leg unvalued and named; the totals
// are lower bounds and net APR is withheld.
func TestLendingEarningsUSD_UnpricedLegIsALowerBound(t *testing.T) {
	pool, sac, other := mkCStrkey(t, 1), nativeSAC(t), mkCStrkey(t, 5)
	srv := earningsUSDServer(t, pool, []usdLeg{
		{asset: sac, supply: 100 * xlm, supplyNet: 100 * xlm},
		{asset: other, supply: 7, supplyNet: 7},
	}, 0, time.Time{})
	got := getEarnings(t, srv, earningsAccount, http.StatusOK)
	if len(got.Legs) != 2 || got.Legs[1].ValueUSD != nil || strOrNull(got.Legs[1].UsdReason) != "unpriced" {
		t.Fatalf("legs = %+v", got.Legs)
	}
	assertPartial(t, got, "200.00", []v1.LendingStatsExclusion{{Kind: "supply", Pool: &pool, Asset: &other, Reason: "unpriced"}})
}

// TestLendingEarningsUSD_ValuedWhileAccruedWithheld: a rate_behind leg still serves current_value, so it is valued; only
// accrued (and so accrued_usd) is withheld, which needs no usd_reason.
func TestLendingEarningsUSD_ValuedWhileAccruedWithheld(t *testing.T) {
	pool, sac := mkCStrkey(t, 1), nativeSAC(t)
	srv := earningsUSDServer(t, pool, []usdLeg{{asset: sac, supply: 100 * xlm, supplyNet: 90 * xlm}}, 1_700_000_000, time.Unix(1_700_000_005, 0))
	got := getEarnings(t, srv, earningsAccount, http.StatusOK)
	if len(got.Legs) != 1 || strOrNull(got.Legs[0].Reason) != "rate_behind" || strOrNull(got.Legs[0].ValueUSD) != "200.00" ||
		got.Legs[0].Accrued != nil || got.Legs[0].AccruedUSD != nil || got.Legs[0].UsdReason != nil {
		t.Fatalf("legs = %+v", got.Legs)
	}
	if tot := got.Totals; tot.SuppliedUSD != "200.00" || strOrNull(tot.NetUSD) != "200.00" || tot.LowerBound || strOrNull(tot.NetAPR) != "0.0500" {
		t.Fatalf("totals = %+v", tot)
	}
}

// TestLendingEarningsUSD_StalePriceFlagsStale: a stale XLM price on /reserves marks the earnings response stale.
func TestLendingEarningsUSD_StalePriceFlagsStale(t *testing.T) {
	pool, sac := mkCStrkey(t, 1), nativeSAC(t)
	srv := earningsUSDServerPriced(t, pool, []usdLeg{{asset: sac, supply: 100 * xlm, supplyNet: 100 * xlm}}, 0, time.Time{}, true)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/accounts/"+earningsAccount+"/earnings", nil))
	var body struct {
		Data  v1.LendingEarningsView
		Flags struct{ Stale bool }
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || !body.Flags.Stale ||
		len(body.Data.Legs) != 1 || strOrNull(body.Data.Legs[0].ValueUSD) != "200.00" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
}

// TestLendingEarningsUSD_PositionsReadFailure: an unread Positions entry leaves current_value null, so the supply leg is
// withheld, the backstop leg unpriced; both are named and the response is degraded (no-store).
func TestLendingEarningsUSD_PositionsReadFailure(t *testing.T) {
	pool, asset, token := mkCStrkey(t, 1), mkCStrkey(t, 2), mkCStrkey(t, 3)
	ex := &stubEarningsExplorer{
		stubExplorerReader: &stubExplorerReader{},
		posErr:             errors.New("lake down"),
		backstop: clickhouse.LendingBackstopState{
			Instance: &clickhouse.PoolInstance{Backstop: mkCStrkey(t, 4)}, Token: token,
			Tiers: []clickhouse.BackstopTierState{{Balance: &clickhouse.BackstopBalance{Shares: big.NewInt(100), Tokens: big.NewInt(100), Q4W: new(big.Int)}}},
		},
		held: clickhouse.LendingAccountBackstop{Held: map[string]*big.Int{"": big.NewInt(10)}},
	}
	pr := &stubEarningsPositions{
		stubPositionsReader: &stubPositionsReader{
			blend:    []timescale.BlendPositionFold{{Pool: pool, Asset: asset, HasSupplyLeg: true, SupplyNet: "90", SupplyTokens: "100"}},
			backstop: []timescale.BlendBackstopFold{{Pool: pool, SharesNet: "10"}},
		},
		flows: timescale.LendingAccountFlows{BlendBackstop: []timescale.LendingBackstopTokenFold{{Pool: pool, NetDeposited: "10"}}},
	}
	srv := v1.New(v1.Options{Explorer: ex, Positions: pr, Lending: &stubLendingReader{assets: []string{asset}}, CompletenessReader: verifiedLendingHistory()})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/accounts/"+earningsAccount+"/earnings", nil))
	var body struct{ Data v1.LendingEarningsView }
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || len(body.Data.Legs) != 2 ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if l := body.Data.Legs[0]; l.CurrentValue != nil || strOrNull(l.UsdReason) != "withheld" || strOrNull(body.Data.Legs[1].UsdReason) != "unpriced" {
		t.Fatalf("legs = %+v", body.Data.Legs)
	}
	assertPartial(t, body.Data, "0.00", []v1.LendingStatsExclusion{
		{Kind: "supply", Pool: &pool, Asset: &asset, Reason: "withheld"},
		{Kind: "backstop", Pool: &pool, Asset: &token, Reason: "unpriced"},
	})
}

func assertPartial(t *testing.T, got v1.LendingEarningsView, supplied string, excluded []v1.LendingStatsExclusion) {
	t.Helper()
	for _, tot := range []v1.LendingUSDTotals{got.Pools[0].LendingUSDTotals, got.Totals} {
		if tot.SuppliedUSD != supplied || !tot.LowerBound || !slices.EqualFunc(tot.Excluded, excluded, sameExclusion) ||
			tot.NetUSD != nil || strOrNull(tot.NetUSDReason) != "inputs_partial" ||
			tot.NetAPR != nil || strOrNull(tot.NetAPRReason) != "inputs_partial" {
			t.Fatalf("totals = %+v", tot)
		}
	}
}

func sameExclusion(a, b v1.LendingStatsExclusion) bool {
	return a.Kind == b.Kind && strOrNull(a.Pool) == strOrNull(b.Pool) && strOrNull(a.Asset) == strOrNull(b.Asset) && a.Reason == b.Reason
}

// TestLendingEarningsUSD_NetAPRSign: supply only serves the supply APR; borrowing more than is supplied withholds it.
func TestLendingEarningsUSD_NetAPRSign(t *testing.T) {
	pool, sac := mkCStrkey(t, 1), nativeSAC(t)
	got := getEarnings(t, earningsUSDServer(t, pool, []usdLeg{{asset: sac, supply: 10 * xlm, supplyNet: 10 * xlm}}, 0, time.Time{}),
		earningsAccount, http.StatusOK)
	if tot := got.Totals; strOrNull(tot.NetAPR) != "0.0500" || tot.BorrowedUSD != "0.00" {
		t.Fatalf("supply-only totals = %+v", tot)
	}
	got = getEarnings(t, earningsUSDServer(t, pool, []usdLeg{{asset: sac, supply: 10 * xlm, supplyNet: 10 * xlm, borrow: 20 * xlm, borNet: 20 * xlm}},
		0, time.Time{}), earningsAccount, http.StatusOK)
	if tot := got.Totals; strOrNull(tot.NetUSD) != "-20.00" || tot.NetAPR != nil || strOrNull(tot.NetAPRReason) != "net_value_not_positive" {
		t.Fatalf("underwater totals = %+v", tot)
	}
}

// blntEarningsExplorer serves a BLNT pool's reserves from real ReserveData plus the account's Positions entry.
type blntEarningsExplorer struct {
	*stubBLNTExplorer
	positions clickhouse.LendingAccountPositions
}

func (e *blntEarningsExplorer) LendingAccountPositions(context.Context, string, string) (clickhouse.LendingAccountPositions, error) {
	return e.positions, nil
}

func (*blntEarningsExplorer) LendingBackstop(context.Context, string, bool) (clickhouse.LendingBackstopState, error) {
	return clickhouse.LendingBackstopState{}, nil
}

func (*blntEarningsExplorer) LendingAccountBackstop(context.Context, string, string, string, []string) (clickhouse.LendingAccountBackstop, error) {
	return clickhouse.LendingAccountBackstop{}, nil
}

// TestLendingEarningsUSD_BLNT: a BLNT leg is valued at its pool's /reserves price and weighted by the fee-adjusted APR
// /reserves serves (0.0247 supply, 0.0385 borrow on this real reserve).
func TestLendingEarningsUSD_BLNT(t *testing.T) {
	const usdc = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	unknown := blend.PoolVersionUnknown
	rd := blnt.ReserveData{
		DRate: big.NewInt(1_000_000_000_000), BRate: big.NewInt(1_000_000_000_000), IRMod: bigInt(t, "5169744"),
		// At rate 1.0, the real reserve's underlying supplied and borrowed, so utilisation and the APRs are its own.
		BSupply: bigInt(t, "1985903398374"), DSupply: bigInt(t, "1599586439842"), BackstopCredit: new(big.Int), Version: blend.PoolV2,
	}
	ex := &blntEarningsExplorer{
		stubBLNTExplorer: &stubBLNTExplorer{stubExplorerReader: &stubExplorerReader{}, data: map[string]blnt.ReserveData{usdc: rd}, bstop: 2_000_000},
		positions: clickhouse.LendingAccountPositions{
			Readable: true,
			Supply:   map[string]*big.Int{usdc: big.NewInt(300 * xlm)}, Borrow: map[string]*big.Int{usdc: big.NewInt(100 * xlm)},
		},
	}
	now := time.Now().UTC()
	srv := v1.New(v1.Options{
		Explorer: ex,
		Positions: &stubEarningsPositions{stubPositionsReader: &stubPositionsReader{}, blntFolds: []timescale.BlendPositionFold{{
			Pool: blnt.MainnetFixedPool, Asset: usdc,
			HasSupplyLeg: true, SupplyNet: "2900000000", SupplyTokens: "3000000000",
			HasBorrowLeg: true, BorrowNet: "1000000000", BorrowTokens: "1000000000",
		}}},
		Lending: &stubBLNTLendingReader{
			stubLendingReader: stubLendingReader{version: &unknown},
			blntAssets:        []string{usdc},
			blntMeta: map[string][]byte{usdc: []byte(`{"index":1,"decimals":7,"c_factor":9500000,"l_factor":9500000,"util":8000000,` +
				`"max_util":9000000,"r_base":300000,"r_one":400000,"r_two":1200000,"r_three":50000000,"reactivity":20,` +
				`"supply_cap":"10000000000000000","enabled":true}`)},
		},
		CompletenessReader: &stubCompletenessReader{snaps: []timescale.CompletenessSnapshot{
			{Source: "blnt", Genesis: 100, ProjectionOK: true, ProjectionVerifiedFrom: 100, ComputedAt: now, ProjectionEvidencedAt: now},
		}},
		Prices: &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{usdc + "/fiat:USD": {Price: "1.5"}}},
	})
	res := getReserves(t, srv, blnt.MainnetFixedPool)
	if len(res.Reserves) != 1 || strOrNull(res.Reserves[0].PriceUSD) != "1.5" || res.Reserves[0].SupplyAPR == nil ||
		ratFloat(*res.Reserves[0].SupplyAPR) != "0.0247" || res.Reserves[0].BorrowAPR == nil || ratFloat(*res.Reserves[0].BorrowAPR) != "0.0385" {
		t.Fatalf("reserves = %+v", res.Reserves)
	}
	got := getEarnings(t, srv, earningsAccount, http.StatusOK)
	if len(got.Legs) != 2 || strOrNull(got.Legs[0].ValueUSD) != "450.00" || strOrNull(got.Legs[0].AccruedUSD) != "15.00" ||
		strOrNull(got.Legs[1].ValueUSD) != "150.00" || strOrNull(got.Legs[1].AccruedUSD) != "0.00" {
		t.Fatalf("legs = %+v", got.Legs)
	}
	// (450 x 0.0247 - 150 x 0.0385) / 300 = (11.115 - 5.775) / 300 = 0.0178.
	if p := got.Pools; len(p) != 1 || p[0].Protocol != "blnt" || strOrNull(p[0].NetUSD) != "300.00" || strOrNull(p[0].NetAPR) != "0.0178" {
		t.Fatalf("pools = %+v", p)
	}
}

func ratFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// TestLendingEarningsUSD_OmittedBorrowWithholdsNet: $1000 supplied and priced, 800 of an unpriced token borrowed. The real
// net is below $1000, so serving net_usd 1000.00 as a lower bound would overstate it: net_usd is null with a reason.
func TestLendingEarningsUSD_OmittedBorrowWithholdsNet(t *testing.T) {
	pool, sac, other := mkCStrkey(t, 1), nativeSAC(t), mkCStrkey(t, 5)
	srv := earningsUSDServer(t, pool, []usdLeg{
		{asset: sac, supply: 500 * xlm, supplyNet: 500 * xlm},
		{asset: other, borrow: 800 * xlm, borNet: 800 * xlm},
	}, 0, time.Time{})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/accounts/"+earningsAccount+"/earnings", nil))
	var body struct {
		Data struct {
			Pools  []map[string]any `json:"pools"`
			Totals map[string]any   `json:"totals"`
		} `json:"data"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || len(body.Data.Pools) != 1 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	for _, tot := range []map[string]any{body.Data.Pools[0], body.Data.Totals} {
		if tot["supplied_usd"] != "1000.00" || tot["borrowed_usd"] != "0.00" || tot["lower_bound"] != true ||
			tot["net_usd"] != nil || tot["net_usd_reason"] != "inputs_partial" || tot["net_apr"] != nil ||
			tot["net_apr_reason"] != "inputs_partial" {
			t.Fatalf("totals = %v", tot)
		}
	}
}
