package wiring

import (
	"context"
	"errors"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// StoreVolumeReader adapts *timescale.Store to v1.VolumeReader (trailing-24h USD volume across every pair the asset participates in).
type StoreVolumeReader struct{ S *timescale.Store }

func (r StoreVolumeReader) Volume24hUSDForAsset(ctx context.Context, assetKey string) (string, bool, error) {
	return r.S.Volume24hUSDForAsset(ctx, assetKey)
}

// SorobanVolume24hUSDForAsset implements the optional v1.SorobanVolumeReader — the XLM-anchored 24h USD-volume variant used for pure-Soroban SEP-41 assets whose liquidity is quoted in XLM rather than a USD-pegged classic.
func (r StoreVolumeReader) SorobanVolume24hUSDForAsset(ctx context.Context, assetKey string) (string, bool, error) {
	return r.S.SorobanVolume24hUSDForAsset(ctx, assetKey)
}

// StoreSupplyLooker adapts *timescale.Store to v1.SupplyLooker; timescale.ErrNotFound becomes v1.ErrSupplyNotFound, other errors propagate.
type StoreSupplyLooker struct{ S *timescale.Store }

func (r StoreSupplyLooker) LatestSupply(ctx context.Context, assetKey string) (supply.Supply, error) {
	snap, err := r.S.LatestSupply(ctx, assetKey)
	if err != nil {
		if errors.Is(err, timescale.ErrNotFound) {
			return supply.Supply{}, v1.ErrSupplyNotFound
		}
		return supply.Supply{}, err
	}
	return snap, nil
}

// SupplyCoverageStats delegates to the Store so the wrapper also satisfies v1.SupplyCoverageReader; without it /v1/diagnostics/ingestion's supply section renders empty.
func (r StoreSupplyLooker) SupplyCoverageStats(ctx context.Context) (timescale.SupplyCoverage, error) {
	return r.S.SupplyCoverageStats(ctx)
}

// DailyCirculatingSupply delegates to the Store's supply_1d CAGG reader (migration 0066), the supply leg of crypto market-cap-over- time on /v1/chart?price_type=market_cap.
func (r StoreSupplyLooker) DailyCirculatingSupply(ctx context.Context, assetKey string, from, to time.Time) ([]timescale.SupplyDayPoint, error) {
	return r.S.DailyCirculatingSupply(ctx, assetKey, from, to)
}

// FXHistoryReader adapts (*timescale.Store) to v1.FXHistoryReader.
type FXHistoryReader struct{ Store *timescale.Store }

func (r *FXHistoryReader) ListFXHistory(ctx context.Context, ticker string, from, to time.Time) ([]v1.FXQuotePoint, error) {
	rows, err := r.Store.ListFXHistory(ctx, ticker, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]v1.FXQuotePoint, len(rows))
	for i, q := range rows {
		out[i] = v1.FXQuotePoint{
			Bucket:         q.Bucket,
			RateUSDText:    q.RateUSDText,
			InverseUSDText: q.InverseUSDText,
		}
	}
	return out, nil
}

// FXCoverageStats delegates to the Store so the wrapper also satisfies v1.FXCoverageReader.
func (r *FXHistoryReader) FXCoverageStats(ctx context.Context) (timescale.FXCoverage, error) {
	return r.Store.FXCoverageStats(ctx)
}

// CAGGCoverageStats delegates so the wrapper satisfies v1.CAGGCoverageReader too.
func (r *FXHistoryReader) CAGGCoverageStats(ctx context.Context) (timescale.CAGGCoverage, error) {
	return r.Store.CAGGCoverageStats(ctx)
}

// SourceEntryCounts delegates so the wrapper also satisfies v1.SourceEntryCountReader; without it the type assertion fails closed and `entries` reads 0 for every source.
func (r *FXHistoryReader) SourceEntryCounts(ctx context.Context) (map[string]int64, error) {
	return r.Store.SourceEntryCounts(ctx)
}
