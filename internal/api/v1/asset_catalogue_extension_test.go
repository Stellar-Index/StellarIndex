package v1_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubAssetsReaderExt implements v1.AssetsReader for the asset-extension overlay tests; each method returns its canned value
// and everything else returns sql.ErrNoRows so behaviour degrades cleanly.
type stubAssetsReaderExt struct {
	row        timescale.AssetRow
	rowErr     error
	topMarkets []timescale.AssetTopMarket
	hist24     []timescale.AssetPricePoint
	hist7d     []timescale.AssetPricePoint
	marketsN   int64
	tradeN     int64
	ath        *timescale.AssetATH
}

func (s *stubAssetsReaderExt) ListAssetsExt(_ context.Context, _ timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	return nil, nil
}

func (s *stubAssetsReaderExt) GetAssetBySlug(_ context.Context, _ string) (timescale.AssetRow, error) {
	return timescale.AssetRow{}, sql.ErrNoRows
}

func (s *stubAssetsReaderExt) GetAssetByAssetID(_ context.Context, _ string) (timescale.AssetRow, error) {
	return s.row, s.rowErr
}

func (s *stubAssetsReaderExt) GetNativeAssetRow(_ context.Context) (timescale.AssetRow, error) {
	return s.row, s.rowErr
}

func (s *stubAssetsReaderExt) GetAssetTopMarkets(_ context.Context, _ string, _ int) ([]timescale.AssetTopMarket, error) {
	return s.topMarkets, nil
}

func (s *stubAssetsReaderExt) GetAssetPriceHistory24h(_ context.Context, _ string) ([]timescale.AssetPricePoint, error) {
	return s.hist24, nil
}

func (s *stubAssetsReaderExt) GetAssetPriceHistory7d(_ context.Context, _ string) ([]timescale.AssetPricePoint, error) {
	return s.hist7d, nil
}

func (s *stubAssetsReaderExt) GetAssetsPriceHistory24hBatch(_ context.Context, _ []string) (map[string][]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (s *stubAssetsReaderExt) GetAssetsPriceHistory7dBatch(_ context.Context, _ []string) (map[string][]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (s *stubAssetsReaderExt) GetAssetMarketsCount(_ context.Context, _ string) (int64, error) {
	return s.marketsN, nil
}

func (s *stubAssetsReaderExt) GetAssetATH(_ context.Context, _ string) (*timescale.AssetATH, error) {
	return s.ath, nil
}

func (s *stubAssetsReaderExt) GetAssetsATHBatch(_ context.Context, _ []string) (map[string]timescale.AssetATH, error) {
	return nil, nil
}

func (s *stubAssetsReaderExt) GetAssetTradeCount24h(_ context.Context, _ string) (int64, error) {
	return s.tradeN, nil
}

// ptr is a tiny helper.
func sptr(s string) *string { return &s }

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
