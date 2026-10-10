package v1_test

import (
	"net/http"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const filterTestUSDC = "USDC-GB7LCUIDT3C2DUOX4O2FSCCBH5NXIUJZ64YQ2N75N5POZRI4DA4AMGEE"

func mustSAC(t *testing.T, assetID string) string {
	t.Helper()
	a, err := canonical.ParseAsset(assetID)
	if err != nil {
		t.Fatalf("parse %q: %v", assetID, err)
	}
	sac, err := a.SacContractID()
	if err != nil {
		t.Fatalf("sac of %q: %v", assetID, err)
	}
	return sac
}

func TestPoolAssetFilter_RejectsBadInput(t *testing.T) {
	pair := mkCStrkey(t, 1)
	reserves := poolReservesTestServer(t, &stubExplorerReader{}, []timescale.SoroswapPair{
		{PairStrkey: pair, Token0Strkey: mkCStrkey(t, 10), Token1Strkey: mkCStrkey(t, 11)},
	})
	native := liquidityPoolsTestServer(t, &stubExplorerReader{})
	for _, u := range []string{
		reserves + "/v1/pools/reserves?asset=not-an-asset",
		reserves + "/v1/pools/reserves?asset=native&pool=" + pair,
		native + "/v1/liquidity-pools?asset=not-an-asset",
		native + "/v1/liquidity-pools?asset=native&pool=" + mkLStrkey(t, 1),
	} {
		if resp := mustGet(t, u); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", u, resp.StatusCode)
		}
	}
}
