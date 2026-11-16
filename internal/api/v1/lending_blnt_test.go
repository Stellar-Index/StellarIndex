package v1_test

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubBLNTLendingReader adds the BLNT reads to a Blend stub that knows no Blend lineage.
type stubBLNTLendingReader struct {
	stubLendingReader
	blntPools  []timescale.BlendPoolSummary
	blntAssets []string
	blntMeta   map[string][]byte
}

func (r *stubBLNTLendingReader) ListBLNTPools(context.Context) ([]timescale.BlendPoolSummary, error) {
	return r.blntPools, nil
}

func (r *stubBLNTLendingReader) BLNTPoolAssets(context.Context, string) ([]string, error) {
	return r.blntAssets, nil
}

func (r *stubBLNTLendingReader) BLNTReserveConfigMetadata(context.Context, string) (map[string][]byte, error) {
	return r.blntMeta, nil
}

// stubBLNTExplorer derives reserve state from real ReserveData through blnt.Metrics with the configs the handler passes.
type stubBLNTExplorer struct {
	*stubExplorerReader
	data  map[string]blnt.ReserveData
	bstop uint32
}

func (e *stubBLNTExplorer) BLNTPoolReserves(_ context.Context, pool string, assets []string, configs map[string]blnt.ReserveConfig) ([]clickhouse.BlendReserveState, error) {
	var out []clickhouse.BlendReserveState
	for _, a := range assets {
		cfg, ok := configs[a]
		rd := e.data[a]
		if !ok {
			continue
		}
		out = append(out, clickhouse.BlendReserveState{
			Pool: pool, Asset: a, Decimals: cfg.Decimals, DecimalsFound: true,
			Data: rd, Metrics: blnt.Metrics(rd, cfg, e.bstop),
		})
	}
	return out, nil
}

func bigInt(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad int %q", s)
	}
	return n
}

// TestLendingPools_ListsBLNTPoolsUnderTheirOwnProtocol — BLNT pools join the list as protocol "blnt", ordered with
// Blend's by auctions, never relabelled as Blend.
func TestLendingPools_ListsBLNTPoolsUnderTheirOwnProtocol(t *testing.T) {
	blendPool, blntPool := mkCStrkey(t, 1), blnt.MainnetFixedPool
	srv := v1.New(v1.Options{Lending: &stubBLNTLendingReader{
		stubLendingReader: stubLendingReader{pools: []timescale.BlendPoolSummary{{Pool: blendPool, AuctionsTotal: 3}}},
		blntPools:         []timescale.BlendPoolSummary{{Pool: blntPool, AuctionsTotal: 9}},
	}})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/pools", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var body struct{ Data []v1.LendingPool }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 2 || body.Data[0].Pool != blntPool || body.Data[0].Protocol != "blnt" ||
		body.Data[1].Pool != blendPool || body.Data[1].Protocol != "blend" {
		t.Fatalf("pools = %+v", body.Data)
	}
}

// TestLendingPoolReserves_ServesABLNTPoolWithTheFeeAdjustedSupplyAPR — a pool with no Blend lineage that blnt_events
// knows is served from its event-derived config; the supply APR carries BLNT's 0.2% protocol fee (Blend's formula
// would round to 0.0248 on this real reserve).
func TestLendingPoolReserves_ServesABLNTPoolWithTheFeeAdjustedSupplyAPR(t *testing.T) {
	const usdc = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	unknown := blend.PoolVersionUnknown
	rd := blnt.ReserveData{ // CACSQW… USDC ResData at ledger 65513222
		DRate: bigInt(t, "1002460888445"), BRate: bigInt(t, "1001344855663"), IRMod: bigInt(t, "5169744"),
		BSupply: bigInt(t, "1983236231897"), DSupply: bigInt(t, "1595659699325"), BackstopCredit: bigInt(t, "122270395"),
		Version: blend.PoolV2,
	}
	srv := v1.New(v1.Options{
		Explorer: &stubBLNTExplorer{stubExplorerReader: &stubExplorerReader{}, data: map[string]blnt.ReserveData{usdc: rd}, bstop: 2_000_000},
		Lending: &stubBLNTLendingReader{
			stubLendingReader: stubLendingReader{version: &unknown},
			blntAssets:        []string{usdc},
			blntMeta: map[string][]byte{usdc: []byte(`{"index":1,"decimals":7,"c_factor":9500000,"l_factor":9500000,"util":8000000,` +
				`"max_util":9000000,"r_base":300000,"r_one":400000,"r_two":1200000,"r_three":50000000,"reactivity":20,` +
				`"supply_cap":"10000000000000000","enabled":true}`)},
		},
	})
	got := getReserves(t, srv, blnt.MainnetFixedPool)
	if len(got.Reserves) != 1 {
		t.Fatalf("reserves = %+v", got.Reserves)
	}
	r := got.Reserves[0]
	if r.Supplied != "1985903398374" || r.Borrowed != "1599586439842" || r.UtilizationPct != 80.55 ||
		r.BorrowAPR == nil || *r.BorrowAPR != 0.0385 || r.SupplyAPR == nil || *r.SupplyAPR != 0.0247 {
		t.Fatalf("reserve = %+v borrow=%v supply=%v", r, r.BorrowAPR, r.SupplyAPR)
	}
}
