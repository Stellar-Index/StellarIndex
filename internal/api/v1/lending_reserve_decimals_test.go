package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"sync"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// perContractDecimals is a v1.TokenDecimalsReader answering per contract;
// a contract absent from the map reports found=false (uncaptured metadata).
// buildReserveView runs under forEachBounded, so consulted is mutex-guarded.
type perContractDecimals struct {
	byContract map[string]uint32
	mu         sync.Mutex
	consulted  map[string]bool
}

func newPerContractDecimals(byContract map[string]uint32) *perContractDecimals {
	return &perContractDecimals{byContract: byContract, consulted: map[string]bool{}}
}

func (d *perContractDecimals) TokenDecimals(_ context.Context, contractID string) (uint32, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.consulted[contractID] = true
	v, ok := d.byContract[contractID]
	return v, ok, nil
}

func (d *perContractDecimals) wasConsulted(contractID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.consulted[contractID]
}

func reserveState(pool, asset string, dec uint32, found bool, supplied, borrowed *big.Int) clickhouse.BlendReserveState {
	return clickhouse.BlendReserveState{
		Pool: pool, Asset: asset, Decimals: dec, DecimalsFound: found,
		Metrics: blend.ReserveMetrics{SuppliedUnderlying: supplied, BorrowedUnderlying: borrowed},
	}
}

func getReserves(t *testing.T, srv *v1.Server, pool string) v1.LendingPoolReservesView {
	t.Helper()
	resp := mustGet(t, httpTestServer(t, srv).URL+"/v1/lending/pools/"+pool+"/reserves")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.LendingPoolReservesView `json:"data"`
	}
	mustDecode(t, resp, &env)
	return env.Data
}

func usdKey(t *testing.T, assetID string) string {
	t.Helper()
	a, err := canonical.ParseAsset(assetID)
	if err != nil {
		t.Fatalf("parse %q: %v", assetID, err)
	}
	return a.String() + "/fiat:USD"
}

func strOrNil(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestLendingPoolReserves_NoConfigNeverInventsDecimals pins CA2-A14-harden-3:
// a reserve whose rate config is missing must not be USD-valued with the
// placeholder exponent 7. Every reserve is priced at $2 per whole token.
//
//   - eighteen: an 18-dp Soroban token with no config — valued with the
//     contract's own declared decimals (3 tokens = $6.00), not 10^11 × that.
//   - unknown: no config and no declared decimals — USD withheld and left
//     out of tvl_usd rather than published off by an unknown power of ten.
//   - xlmSAC: the native SAC with no config — 7 by protocol, still priced.
func TestLendingPoolReserves_NoConfigNeverInventsDecimals(t *testing.T) {
	pool := mkCStrkey(t, 7)
	eighteen := mkCStrkey(t, 40)
	unknown := mkCStrkey(t, 41)
	xlmSAC, ok := xdrjson.SACContractID("native", "Public Global Stellar Network ; September 2015")
	if !ok {
		t.Fatal("native SAC id")
	}
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	states := []clickhouse.BlendReserveState{
		reserveState(pool, eighteen, 7, false, new(big.Int).Mul(big.NewInt(3), e18), e18),
		reserveState(pool, unknown, 7, false, big.NewInt(50_000_000), big.NewInt(10_000_000)),
		reserveState(pool, xlmSAC, 7, false, big.NewInt(30_000_000), big.NewInt(10_000_000)),
	}
	// The stored price is the RAW smallest-unit ratio; the confirmed 18
	// normalises eighteen's by 10^11 to $2 per whole token.
	prices := &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{
		usdKey(t, eighteen): {Price: "0.00000000002"},
		usdKey(t, unknown):  {Price: "2"},
		"native/fiat:USD":   {Price: "2"},
	}}
	srv := v1.New(v1.Options{
		Explorer:            &stubExplorerReader{reserves: states},
		Lending:             &stubLendingReader{assets: []string{eighteen, unknown, xlmSAC}},
		Prices:              prices,
		TokenDecimals:       newPerContractDecimals(map[string]uint32{eighteen: 18}),
		NonstandardDecimals: nonstandardDecimalsCacheWith(t, eighteen, 18),
	})
	got := getReserves(t, srv, pool)
	if len(got.Reserves) != 3 {
		t.Fatalf("len(reserves) = %d, want 3", len(got.Reserves))
	}

	r18 := got.Reserves[0]
	if strOrNil(r18.SuppliedUSD) != "6.00" || strOrNil(r18.BorrowedUSD) != "2.00" || r18.Decimals != 18 {
		t.Errorf("18-dp reserve: supplied_usd=%s borrowed_usd=%s decimals=%d, want 6.00 / 2.00 / 18",
			strOrNil(r18.SuppliedUSD), strOrNil(r18.BorrowedUSD), r18.Decimals)
	}
	ru := got.Reserves[1]
	if ru.SuppliedUSD != nil || ru.BorrowedUSD != nil {
		t.Errorf("unknown-decimals reserve: supplied_usd=%s borrowed_usd=%s, want both null",
			strOrNil(ru.SuppliedUSD), strOrNil(ru.BorrowedUSD))
	}
	if ru.Supplied != "50000000" || ru.Borrowed != "10000000" {
		t.Errorf("unknown-decimals reserve token amounts = %s/%s, want exact 50000000/10000000", ru.Supplied, ru.Borrowed)
	}
	rx := got.Reserves[2]
	if strOrNil(rx.SuppliedUSD) != "6.00" || strOrNil(rx.BorrowedUSD) != "2.00" || rx.Decimals != 7 {
		t.Errorf("native SAC reserve: supplied_usd=%s borrowed_usd=%s decimals=%d, want 6.00 / 2.00 / 7",
			strOrNil(rx.SuppliedUSD), strOrNil(rx.BorrowedUSD), rx.Decimals)
	}
	if strOrNil(got.TVLUSD) != "12.00" {
		t.Errorf("tvl_usd = %s, want 12.00 (18-dp 6.00 + SAC 6.00; the unknown-decimals reserve excluded)", strOrNil(got.TVLUSD))
	}
	if !got.LowerBound {
		t.Error("lower_bound = false, want true: tvl_usd excludes the unknown-decimals reserve")
	}
}

// TestLendingPoolReserves_ConfigDecimalsAreAuthoritative: a captured reserve
// config's decimals price the reserve as before; the lake is not consulted.
func TestLendingPoolReserves_ConfigDecimalsAreAuthoritative(t *testing.T) {
	pool := mkCStrkey(t, 7)
	asset := mkCStrkey(t, 42)
	dec := newPerContractDecimals(map[string]uint32{asset: 18})
	srv := v1.New(v1.Options{
		Explorer: &stubExplorerReader{reserves: []clickhouse.BlendReserveState{
			reserveState(pool, asset, 6, true, big.NewInt(3_000_000), big.NewInt(1_000_000)),
		}},
		Lending: &stubLendingReader{assets: []string{asset}},
		// Raw ratio 20, normalised by the confirmed 6 to $2 per whole token.
		Prices:              &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{usdKey(t, asset): {Price: "20"}}},
		TokenDecimals:       dec,
		NonstandardDecimals: nonstandardDecimalsCacheWith(t, asset, 6),
	})
	got := getReserves(t, srv, pool)
	if len(got.Reserves) != 1 {
		t.Fatalf("len(reserves) = %d, want 1", len(got.Reserves))
	}
	rv := got.Reserves[0]
	if strOrNil(rv.SuppliedUSD) != "6.00" || strOrNil(rv.BorrowedUSD) != "2.00" || rv.Decimals != 6 {
		t.Errorf("configured reserve: supplied_usd=%s borrowed_usd=%s decimals=%d, want 6.00 / 2.00 / 6",
			strOrNil(rv.SuppliedUSD), strOrNil(rv.BorrowedUSD), rv.Decimals)
	}
	if dec.wasConsulted(asset) {
		t.Error("lake token decimals consulted for a reserve whose config already declares them")
	}
}

// TestLendingPoolReserves_UnpricedReserveReportsDeclaredDecimals: with no
// config and no USD price, the reserve's decimals field still carries the
// token's declared exponent (clients scale supplied/borrowed with it), not 7.
func TestLendingPoolReserves_UnpricedReserveReportsDeclaredDecimals(t *testing.T) {
	pool := mkCStrkey(t, 7)
	asset := mkCStrkey(t, 43)
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	srv := v1.New(v1.Options{
		Explorer: &stubExplorerReader{reserves: []clickhouse.BlendReserveState{
			reserveState(pool, asset, 7, false, e18, e18),
		}},
		Lending:       &stubLendingReader{assets: []string{asset}},
		Prices:        &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{}},
		TokenDecimals: newPerContractDecimals(map[string]uint32{asset: 18}),
	})
	got := getReserves(t, srv, pool)
	if len(got.Reserves) != 1 {
		t.Fatalf("len(reserves) = %d, want 1", len(got.Reserves))
	}
	rv := got.Reserves[0]
	if rv.Decimals != 18 || rv.SuppliedUSD != nil || got.TVLUSD != nil {
		t.Errorf("unpriced reserve: decimals=%d supplied_usd=%s tvl_usd=%s, want 18 / <nil> / <nil>",
			rv.Decimals, strOrNil(rv.SuppliedUSD), strOrNil(got.TVLUSD))
	}
}

// TestLendingPoolReserves_PriceScaleMustMatchReserveDecimals: the USD price
// is normalised through the nonstandard-decimals projection (else 7), so a
// 6-decimal reserve with no projection row carries the RAW ratio. Dividing
// its amounts by 10^6 would publish ten times the value (30.00 for 3 tokens
// at a raw 1, where the truth is 3.00) — the reserve's USD figures are
// withheld instead and it leaves tvl_usd. With the row, it is valued.
func TestLendingPoolReserves_PriceScaleMustMatchReserveDecimals(t *testing.T) {
	pool := mkCStrkey(t, 7)
	asset := mkCStrkey(t, 44)
	newSrv := func(nd *v1.NonstandardDecimalsCache) *v1.Server {
		return v1.New(v1.Options{
			Explorer: &stubExplorerReader{reserves: []clickhouse.BlendReserveState{
				reserveState(pool, asset, 6, true, big.NewInt(30_000_000), big.NewInt(10_000_000)),
			}},
			Lending:             &stubLendingReader{assets: []string{asset}},
			Prices:              &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{usdKey(t, asset): {Price: "1"}}},
			NonstandardDecimals: nd,
		})
	}

	got := getReserves(t, newSrv(nil), pool)
	if len(got.Reserves) != 1 {
		t.Fatalf("len(reserves) = %d, want 1", len(got.Reserves))
	}
	rv := got.Reserves[0]
	if rv.SuppliedUSD != nil || rv.BorrowedUSD != nil || got.TVLUSD != nil {
		t.Errorf("no projection row: supplied_usd=%s borrowed_usd=%s tvl_usd=%s, want all null",
			strOrNil(rv.SuppliedUSD), strOrNil(rv.BorrowedUSD), strOrNil(got.TVLUSD))
	}
	if rv.Decimals != 6 || rv.Supplied != "30000000" {
		t.Errorf("decimals=%d supplied=%s, want 6 / 30000000 served either way", rv.Decimals, rv.Supplied)
	}

	got = getReserves(t, newSrv(nonstandardDecimalsCacheWith(t, asset, 6)), pool)
	rv = got.Reserves[0]
	if strOrNil(rv.SuppliedUSD) != "3.00" || strOrNil(rv.BorrowedUSD) != "1.00" {
		t.Errorf("confirmed 6: supplied_usd=%s borrowed_usd=%s, want 3.00 / 1.00",
			strOrNil(rv.SuppliedUSD), strOrNil(rv.BorrowedUSD))
	}
}
