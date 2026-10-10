package v1_test

import (
	"context"
	"database/sql"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubAssetsReaderExt implements v1.AssetsReader for the asset-extension
// overlay test path. Each method returns the canned value its caller
// expects; everything else returns sql.ErrNoRows so behaviour
// degrades cleanly.
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
