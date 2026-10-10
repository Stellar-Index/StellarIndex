package v1_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

const pegHandlerTestAUDD = "AUDD-GDC7X2MXTYSAKUUGAIQ7J7RPEIM7GXSAIWFYWWH4GLNFECQVJJLB2EEU"

// pegHandlerTestServer wires the full handleAssetGet chain for the
// declared-peg detail-path tests: a substance gate with a blanket
// verdict (reusing price_tip_test.go's stubSubstanceGate), an
// asset-catalogue overlay row carrying a (dust or real) market price +
// change pills, a fresh AUD FX point (chart_test.go's
// stubFXHistoryReader), and AUDD peg-configured.
func pegHandlerTestServer(t *testing.T, allow bool, row timescale.AssetRow) *v1.Server {
	t.Helper()
	aud, err := canonical.NewFiatAsset("AUD")
	if err != nil {
		t.Fatal(err)
	}
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			row.AssetID: {AssetID: row.AssetID, Type: "classic", Code: row.Code},
		},
	}
	return v1.New(v1.Options{
		Assets:       reader,
		AssetsReader: &stubAssetsReaderExt{row: row},
		Substance:    &stubSubstanceGate{allow: allow},
		FXHistory: &stubFXHistoryReader{points: []v1.FXQuotePoint{
			{Bucket: time.Now().UTC().Add(-24 * time.Hour), RateUSDText: "1.5267", InverseUSDText: "0.655"},
		}},
		FiatPeggedClassics: map[string]canonical.Asset{pegHandlerTestAUDD: aud},
	})
}

// derefOrNil renders a *string for failure messages.
func derefOrNil(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func getPegAssetDetail(t *testing.T, srv *v1.Server, assetID string) v1.AssetDetail {
	t.Helper()
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets/"+assetID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env.Data
}
