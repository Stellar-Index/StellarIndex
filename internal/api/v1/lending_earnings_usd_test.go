package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"slices"
	"strconv"
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
		Prices: &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{"native/fiat:USD": {Price: "2"}}},
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

func ratStr(num, den int64, prec int) string { return big.NewRat(num, den).FloatString(prec) }

// TestLendingEarningsUSD_FullyPriced: 100 XLM supplied and 30 borrowed at $2 total $200 / $60 / $140; net APR is
// (200x0.05 - 60x0.1)/140 = 4/140; each leg is valued at the price /reserves serves for the same reserve.
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
		if tot.SuppliedUSD != "200.00" || tot.BorrowedUSD != "60.00" || tot.NetUSD != "140.00" || tot.LowerBound ||
			len(tot.Unpriced) != 0 || strOrNull(tot.NetAPR) != ratStr(4, 140, 6) || tot.NetAPRReason != nil {
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
	assertPartial(t, got, "200.00", pool+":supply:"+other)
}

// TestLendingEarningsUSD_WithheldLegIsNotValued: a rate_behind leg is not valued, so the totals are lower bounds.
func TestLendingEarningsUSD_WithheldLegIsNotValued(t *testing.T) {
	pool, sac := mkCStrkey(t, 1), nativeSAC(t)
	srv := earningsUSDServer(t, pool, []usdLeg{{asset: sac, supply: 100 * xlm, supplyNet: 90 * xlm}}, 1_700_000_000, time.Unix(1_700_000_005, 0))
	got := getEarnings(t, srv, earningsAccount, http.StatusOK)
	if len(got.Legs) != 1 || strOrNull(got.Legs[0].Reason) != "rate_behind" || got.Legs[0].ValueUSD != nil ||
		strOrNull(got.Legs[0].UsdReason) != "withheld" {
		t.Fatalf("legs = %+v", got.Legs)
	}
	assertPartial(t, got, "0.00", pool+":supply:"+sac)
}

func assertPartial(t *testing.T, got v1.LendingEarningsView, supplied, unpriced string) {
	t.Helper()
	for _, tot := range []v1.LendingUSDTotals{got.Pools[0].LendingUSDTotals, got.Totals} {
		if tot.SuppliedUSD != supplied || !tot.LowerBound || !slices.Equal(tot.Unpriced, []string{unpriced}) ||
			tot.NetAPR != nil || strOrNull(tot.NetAPRReason) != "inputs_partial" {
			t.Fatalf("totals = %+v", tot)
		}
	}
}

// TestLendingEarningsUSD_NetAPRSign: supply only serves the supply APR; borrowing more than is supplied withholds it.
func TestLendingEarningsUSD_NetAPRSign(t *testing.T) {
	pool, sac := mkCStrkey(t, 1), nativeSAC(t)
	got := getEarnings(t, earningsUSDServer(t, pool, []usdLeg{{asset: sac, supply: 10 * xlm, supplyNet: 10 * xlm}}, 0, time.Time{}),
		earningsAccount, http.StatusOK)
	if tot := got.Totals; strOrNull(tot.NetAPR) != "0.050000" || tot.BorrowedUSD != "0.00" {
		t.Fatalf("supply-only totals = %+v", tot)
	}
	got = getEarnings(t, earningsUSDServer(t, pool, []usdLeg{{asset: sac, supply: 10 * xlm, supplyNet: 10 * xlm, borrow: 20 * xlm, borNet: 20 * xlm}},
		0, time.Time{}), earningsAccount, http.StatusOK)
	if tot := got.Totals; tot.NetUSD != "-20.00" || tot.NetAPR != nil || strOrNull(tot.NetAPRReason) != "net_value_not_positive" {
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
		BSupply: bigInt(t, "1983236231897"), DSupply: bigInt(t, "1595659699325"), BackstopCredit: new(big.Int), Version: blend.PoolV2,
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
	if len(res.Reserves) != 1 || strOrNull(res.Reserves[0].PriceUSD) != "1.5" || res.Reserves[0].SupplyAPR == nil {
		t.Fatalf("reserves = %+v", res.Reserves)
	}
	got := getEarnings(t, srv, earningsAccount, http.StatusOK)
	if len(got.Legs) != 2 || strOrNull(got.Legs[0].ValueUSD) != "450.00" || strOrNull(got.Legs[0].AccruedUSD) != "15.00" ||
		strOrNull(got.Legs[1].ValueUSD) != "150.00" || strOrNull(got.Legs[1].AccruedUSD) != "0.00" {
		t.Fatalf("legs = %+v", got.Legs)
	}
	// (450 x supply_apr - 150 x borrow_apr) / 300, at the APRs /reserves serves.
	sup, _ := new(big.Rat).SetString(ratFloat(*res.Reserves[0].SupplyAPR))
	bor, _ := new(big.Rat).SetString(ratFloat(*res.Reserves[0].BorrowAPR))
	num := new(big.Rat).Sub(new(big.Rat).Mul(big.NewRat(450, 1), sup), new(big.Rat).Mul(big.NewRat(150, 1), bor))
	want := num.Quo(num, big.NewRat(300, 1)).FloatString(6)
	if p := got.Pools; len(p) != 1 || p[0].Protocol != "blnt" || p[0].NetUSD != "300.00" || strOrNull(p[0].NetAPR) != want {
		t.Fatalf("pools = %+v want apr %s", p, want)
	}
}

func ratFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
