package v1_test

import (
	"math/big"
	"net/http"
	"net/url"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
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

// ?asset= must match a Soroswap pair through every alias form: XLM's SAC for
// any XLM spelling, and a classic asset's derived SAC without a configured
// sac_wrappers entry. Pairs over other tokens are excluded.
func TestPoolReserves_AssetFilterLoopsAliases(t *testing.T) {
	xlmPair, usdcPair, otherPair := mkCStrkey(t, 1), mkCStrkey(t, 2), mkCStrkey(t, 3)
	usdcSAC := mustSAC(t, filterTestUSDC)
	tokOther := mkCStrkey(t, 10)
	state := func(pair, t0, t1 string) clickhouse.SoroswapPairState {
		return clickhouse.SoroswapPairState{
			Pair: pair, Token0: t0, Token1: t1,
			Reserve0: big.NewInt(1_000_000), Reserve1: big.NewInt(2_000_000), Ledger: 1,
		}
	}
	reader := &stubExplorerReader{pairStates: map[string]clickhouse.SoroswapPairState{
		xlmPair:   state(xlmPair, canonical.XLMSacContractID, tokOther),
		usdcPair:  state(usdcPair, tokOther, usdcSAC),
		otherPair: state(otherPair, tokOther, mkCStrkey(t, 11)),
	}}
	base := poolReservesTestServer(t, reader, []timescale.SoroswapPair{
		{PairStrkey: xlmPair, Token0Strkey: canonical.XLMSacContractID, Token1Strkey: tokOther},
		{PairStrkey: usdcPair, Token0Strkey: tokOther, Token1Strkey: usdcSAC},
		{PairStrkey: otherPair, Token0Strkey: tokOther, Token1Strkey: mkCStrkey(t, 11)},
	})

	cases := []struct {
		asset string
		want  []string
	}{
		{"native", []string{xlmPair}},
		{"crypto:XLM", []string{xlmPair}},
		{canonical.XLMSacContractID, []string{xlmPair}},
		{filterTestUSDC, []string{usdcPair}},
		{usdcSAC, []string{usdcPair}},
		{"EURC-GB7LCUIDT3C2DUOX4O2FSCCBH5NXIUJZ64YQ2N75N5POZRI4DA4AMGEE", nil},
	}
	for _, tc := range cases {
		resp := mustGet(t, base+"/v1/pools/reserves?asset="+url.QueryEscape(tc.asset))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("asset=%s: status = %d", tc.asset, resp.StatusCode)
		}
		var body struct {
			Data []v1.PoolReservesRow `json:"data"`
		}
		mustDecode(t, resp, &body)
		if len(body.Data) != len(tc.want) {
			t.Fatalf("asset=%s: got %d rows, want %v", tc.asset, len(body.Data), tc.want)
		}
		for i, row := range body.Data {
			if row.Pool != tc.want[i] {
				t.Fatalf("asset=%s: row %d = %s, want %s", tc.asset, i, row.Pool, tc.want[i])
			}
		}
	}
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
