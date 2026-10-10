package v1_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// assetExtensionDetail serves GET /v1/assets/{id} for a Soroban contract
// whose whole price picture comes from the asset-catalogue overlay (no
// canonical price reader wired), and returns the decoded detail.
func assetExtensionDetail(t *testing.T, assetID string, cache *v1.NonstandardDecimalsCache, overlay *stubAssetsReaderExt) v1.AssetDetail {
	t.Helper()
	srv := v1.New(v1.Options{
		Assets: &stubAssetReader{byID: map[string]v1.AssetDetail{
			assetID: {AssetID: assetID, Type: "soroban"},
		}},
		AssetsReader:        overlay,
		NonstandardDecimals: cache,
	})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets/"+assetID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env.Data
}

// rawOverlay is what the catalogue readers return for a market whose RAW
// prices_1m ratio is 41.32 — the runbook's real CC2RB… incident value.
func rawOverlay(assetID string) *stubAssetsReaderExt {
	return &stubAssetsReaderExt{
		row: timescale.AssetRow{AssetID: assetID, PriceUSD: sptr("41.3200000000")},
		hist24: []timescale.AssetPricePoint{
			{T: "2026-09-17T00:00:00Z", P: sptr("41.3200000000")},
			{T: "2026-09-17T01:00:00Z", P: nil}, // an hour with no trades stays a gap
			{T: "2026-09-17T02:00:00Z", P: sptr("40.0000000000")},
		},
		hist7d: []timescale.AssetPricePoint{
			{T: "2026-09-11T00:00:00Z", P: sptr("39.5000000000")},
		},
		ath: &timescale.AssetATH{USD: "55.125", At: "2026-08-01T00:00:00Z"},
	}
}
