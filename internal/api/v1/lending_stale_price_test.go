package v1_test

import (
	"math/big"
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// A reserve valued at a price the reader marked stale makes the response stale; the lake being
// current does not vouch for the price.
func TestLendingPoolReserves_StalePriceMarksTheResponseStale(t *testing.T) {
	pool := mkCStrkey(t, 7)
	xlmSAC, ok := xdrjson.SACContractID("native", "Public Global Stellar Network ; September 2015")
	if !ok {
		t.Fatal("native SAC id")
	}
	for name, stale := range map[string]bool{"fresh price": false, "stale price": true} {
		srv := v1.New(v1.Options{
			Explorer: &stubExplorerReader{reserves: []clickhouse.BlendReserveState{
				reserveState(pool, xlmSAC, 7, true, big.NewInt(30_000_000), big.NewInt(10_000_000)),
			}},
			Lending: &stubLendingReader{assets: []string{xlmSAC}},
			Prices: &stubPriceReader{
				snapshots: map[string]v1.PriceSnapshot{"native/fiat:USD": {Price: "2"}},
				stale:     map[string]bool{"native/fiat:USD": stale},
			},
		})
		resp := mustGet(t, httpTestServer(t, srv).URL+"/v1/lending/pools/"+pool+"/reserves")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", name, resp.StatusCode)
		}
		var body struct {
			Data  v1.LendingPoolReservesView `json:"data"`
			Flags v1.Flags                   `json:"flags"`
		}
		mustDecode(t, resp, &body)
		if strOrNil(body.Data.TVLUSD) != "6.00" || body.Flags.Stale != stale {
			t.Errorf("%s: tvl=%s flags.stale=%v, want 6.00 and stale=%v", name, strOrNil(body.Data.TVLUSD), body.Flags.Stale, stale)
		}
	}
}
