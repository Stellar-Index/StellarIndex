package v1_test

import (
	"math/big"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// A pool reserve the lake returned no state for is named, and a TVL that leaves it out is a lower bound.
func TestLendingPoolReserves_OmittedReserveMakesTVLALowerBound(t *testing.T) {
	pool := mkCStrkey(t, 7)
	missing := mkCStrkey(t, 42)
	xlmSAC, ok := xdrjson.SACContractID("native", "Public Global Stellar Network ; September 2015")
	if !ok {
		t.Fatal("native SAC id")
	}
	srv := func(assets []string) *v1.Server {
		return v1.New(v1.Options{
			Explorer: &stubExplorerReader{reserves: []clickhouse.BlendReserveState{
				reserveState(pool, xlmSAC, 7, true, big.NewInt(30_000_000), big.NewInt(10_000_000)),
			}},
			Lending: &stubLendingReader{assets: assets},
			Prices:  &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{"native/fiat:USD": {Price: "2"}}},
		})
	}
	whole := getReserves(t, srv([]string{xlmSAC}), pool)
	if whole.LowerBound || len(whole.OmittedReserves) != 0 || strOrNil(whole.TVLUSD) != "6.00" {
		t.Fatalf("complete pool: tvl=%s lower_bound=%v omitted=%v", strOrNil(whole.TVLUSD), whole.LowerBound, whole.OmittedReserves)
	}
	got := getReserves(t, srv([]string{xlmSAC, missing}), pool)
	if strOrNil(got.TVLUSD) != "6.00" || !got.LowerBound {
		t.Errorf("tvl=%s lower_bound=%v, want 6.00 marked a lower bound", strOrNil(got.TVLUSD), got.LowerBound)
	}
	if len(got.OmittedReserves) != 1 || got.OmittedReserves[0] != missing {
		t.Errorf("omitted_reserves = %v, want [%s]", got.OmittedReserves, missing)
	}
}
