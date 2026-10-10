package v1_test

import (
	"math/big"
	"net/http"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// mkLStrkey builds a deterministic native-pool L-strkey from a seed.
func mkLStrkey(t *testing.T, seed byte) string {
	t.Helper()
	raw := make([]byte, 32)
	raw[0] = seed
	s, err := strkey.Encode(strkey.VersionByteLiquidityPool, raw)
	if err != nil {
		t.Fatalf("encode L-strkey: %v", err)
	}
	return s
}

func liquidityPoolsTestServer(t *testing.T, explorer v1.ExplorerReader) string {
	t.Helper()
	srv := v1.New(v1.Options{Explorer: explorer})
	return httpTestServer(t, srv).URL
}

func sampleNativePool(pool string, reserveA, reserveB int64, trustlines int64) clickhouse.NativeLiquidityPoolState {
	return clickhouse.NativeLiquidityPoolState{
		PoolHex:     "deadbeef",
		PoolStrkey:  pool,
		AssetA:      "native",
		ReserveA:    big.NewInt(reserveA),
		AssetB:      "USDC-GB7LCUIDT3C2DUOX4O2FSCCBH5NXIUJZ64YQ2N75N5POZRI4DA4AMGEE",
		ReserveB:    big.NewInt(reserveB),
		TotalShares: big.NewInt(reserveA + reserveB),
		Trustlines:  trustlines,
		FeeBps:      30,
		Ledger:      63_356_894,
	}
}

func TestLiquidityPools_Listing(t *testing.T) {
	poolBig := mkLStrkey(t, 1)
	poolSmall := mkLStrkey(t, 2)
	reader := &stubExplorerReader{
		// Reader returns already-ranked (trustlines desc). Handler must
		// preserve order + honour ?limit.
		nativeLPRanked: []clickhouse.NativeLiquidityPoolState{
			sampleNativePool(poolBig, 100_000_0000000, 25_000_0000000, 500),
			sampleNativePool(poolSmall, 10_0000000, 5_0000000, 3),
		},
	}
	base := liquidityPoolsTestServer(t, reader)

	resp := mustGet(t, base+"/v1/liquidity-pools")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data []v1.LiquidityPoolReservesRow `json:"data"`
	}
	mustDecode(t, resp, &body)
	if len(body.Data) != 2 {
		t.Fatalf("want 2 rows, got %d", len(body.Data))
	}
	if body.Data[0].Pool != poolBig || body.Data[1].Pool != poolSmall {
		t.Fatalf("ranking order not preserved: %s then %s", body.Data[0].Pool, body.Data[1].Pool)
	}
	row := body.Data[0]
	if row.Model != "constant_product" || row.FeeBps != 30 || row.AsOfLedger != 63_356_894 {
		t.Fatalf("row header = %+v", row)
	}
	if row.ReserveA.Asset != "native" || row.ReserveA.Decimals != 7 {
		t.Fatalf("reserve A = %+v", row.ReserveA)
	}
	if len(row.Depth) == 0 {
		t.Fatal("expected a depth table for a funded pool")
	}
	// Depth must be positive and monotone in slippage tier.
	if row.Depth[0].AssetAIn.MaxInput == "0" {
		t.Errorf("depth tier 0 max_input should be > 0, got %s", row.Depth[0].AssetAIn.MaxInput)
	}
}

func TestLiquidityPools_ListingLimit(t *testing.T) {
	reader := &stubExplorerReader{
		nativeLPRanked: []clickhouse.NativeLiquidityPoolState{
			sampleNativePool(mkLStrkey(t, 1), 100, 100, 500),
			sampleNativePool(mkLStrkey(t, 2), 100, 100, 4),
			sampleNativePool(mkLStrkey(t, 3), 100, 100, 2),
		},
	}
	base := liquidityPoolsTestServer(t, reader)
	resp := mustGet(t, base+"/v1/liquidity-pools?limit=2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data []v1.LiquidityPoolReservesRow `json:"data"`
	}
	mustDecode(t, resp, &body)
	if len(body.Data) != 2 {
		t.Fatalf("limit=2 must cap to 2 rows, got %d", len(body.Data))
	}
}

func TestLiquidityPools_SinglePool(t *testing.T) {
	pool := mkLStrkey(t, 7)
	reader := &stubExplorerReader{
		nativeLPStates: map[string]clickhouse.NativeLiquidityPoolState{
			pool: sampleNativePool(pool, 42_0000000, 7_0000000, 12),
		},
	}
	base := liquidityPoolsTestServer(t, reader)
	resp := mustGet(t, base+"/v1/liquidity-pools?pool="+pool)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data []v1.LiquidityPoolReservesRow `json:"data"`
	}
	mustDecode(t, resp, &body)
	if len(body.Data) != 1 {
		t.Fatalf("want 1 row, got %d", len(body.Data))
	}
	if body.Data[0].Pool != pool || body.Data[0].ReserveA.Reserve != "420000000" {
		t.Fatalf("row = %+v", body.Data[0])
	}
}

func TestLiquidityPools_UnknownPool404(t *testing.T) {
	pool := mkLStrkey(t, 9)
	// Empty map → pool not captured.
	reader := &stubExplorerReader{nativeLPStates: map[string]clickhouse.NativeLiquidityPoolState{}}
	base := liquidityPoolsTestServer(t, reader)
	resp := mustGet(t, base+"/v1/liquidity-pools?pool="+pool)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown pool status = %d, want 404", resp.StatusCode)
	}
}

func TestLiquidityPools_InvalidPool400(t *testing.T) {
	reader := &stubExplorerReader{}
	base := liquidityPoolsTestServer(t, reader)
	for _, bad := range []string{"not-a-pool", "GB7LCUIDT3C2DUOX4O2FSCCBH5NXIUJZ64YQ2N75N5POZRI4DA4AMGEE"} {
		resp := mustGet(t, base+"/v1/liquidity-pools?pool="+bad)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid pool %q status = %d, want 400", bad, resp.StatusCode)
		}
	}
}

func TestLiquidityPools_HexPoolAccepted(t *testing.T) {
	pool := mkLStrkey(t, 3)
	hexID := "43001ffd4cde723425dd124c31f08f0d0e6eb96bc04e1fb7ad312ee221c79a52"
	reader := &stubExplorerReader{
		nativeLPStates: map[string]clickhouse.NativeLiquidityPoolState{
			pool: sampleNativePool(pool, 1, 1, 1),
		},
	}
	base := liquidityPoolsTestServer(t, reader)
	resp := mustGet(t, base+"/v1/liquidity-pools?pool="+hexID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("hex pool id status = %d, want 200", resp.StatusCode)
	}
}

func TestLiquidityPools_ExplorerUnavailable503(t *testing.T) {
	base := liquidityPoolsTestServer(t, nil)
	resp := mustGet(t, base+"/v1/liquidity-pools")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("nil explorer status = %d, want 503", resp.StatusCode)
	}
}

// An asset's native pools are not confined to the global top-N ranking: a
// pool ranked past the listing cap must still be served under ?asset=.
func TestLiquidityPools_AssetFilterBeyondTopN(t *testing.T) {
	ranked := make([]clickhouse.NativeLiquidityPoolState, 0, 101)
	for i := 0; i < 100; i++ {
		ranked = append(ranked, sampleNativePool(mkLStrkey(t, byte(i+1)), 1_0000000, 1_0000000, int64(1000-i)))
	}
	tail := sampleNativePool(mkLStrkey(t, 200), 7_0000000, 3_0000000, 1)
	tail.AssetA = "EURC-GBVRVE6CCHJFZ6IFEKYRCRODKPPJHJFPCYOZILWADK4CEZB6DEXBYTI6"
	ranked = append(ranked, tail)
	base := liquidityPoolsTestServer(t, &stubExplorerReader{nativeLPRanked: ranked})

	resp := mustGet(t, base+"/v1/liquidity-pools?asset="+tail.AssetA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data []v1.LiquidityPoolReservesRow `json:"data"`
	}
	mustDecode(t, resp, &body)
	if len(body.Data) != 1 || body.Data[0].Pool != tail.PoolStrkey {
		t.Fatalf("got %+v, want only the 101st-ranked pool %s", body.Data, tail.PoolStrkey)
	}

	// XLM's SAC aliases to native: the 100 native pools match, capped by limit.
	resp = mustGet(t, base+"/v1/liquidity-pools?limit=3&asset="+canonical.XLMSacContractID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	mustDecode(t, resp, &body)
	if len(body.Data) != 3 || body.Data[0].Pool != ranked[0].PoolStrkey {
		t.Fatalf("SAC-form filter got %d rows (first %+v), want the top 3 native pools", len(body.Data), body.Data)
	}
}
